//! Kiln's Rust runtime against the Go compiler's modules: every shared case of language/testdata/scripts that
//! runs, the instances, natives and collector checks the browser's runtime makes (js/src/script.test.ts), the
//! satisfies check against Go's, and numbers and strings as Go writes, reads and quotes them. The fixtures are
//! written by Go's TestWriteRuntimeFixtures, which these tests run first, so they need Go.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::OnceLock;
use std::time::Instant;

use kiln::gofmt::{format_number, parse_number, quote};
use kiln::{Call, Error, HostModule, Instance, Interface, NativeResult, Resolver, Value, load_script};

#[derive(serde::Deserialize)]
struct Fixtures {
    cases: Vec<FixtureCase>,
    modules: HashMap<String, String>,
    hosts: HashMap<String, String>,
    formats: Vec<FormatCase>,
    parses: Vec<ParseCase>,
    quotes: HashMap<String, String>,
    interfaces: HashMap<String, serde_json::Value>,
    satisfies: Vec<SatisfiesCase>,
}

#[derive(serde::Deserialize)]
struct FixtureCase {
    name: String,
    module: String,
    want: Option<String>,
    logs: Option<String>,
    eval: Option<String>,
}

#[derive(serde::Deserialize)]
struct FormatCase {
    bits: String,
    text: String,
}

#[derive(serde::Deserialize)]
struct ParseCase {
    text: String,
    bits: Option<String>,
}

#[derive(serde::Deserialize)]
struct SatisfiesCase {
    module: String,
    host: String,
    interface: String,
    why: String,
}

fn dir() -> PathBuf {
    Path::new(env!("CARGO_TARGET_TMPDIR")).join("kiln-fixtures")
}

/// Has Go write the fixtures, once for the tests of this file.
fn fixtures() -> &'static Fixtures {
    static FIXTURES: OnceLock<Fixtures> = OnceLock::new();
    FIXTURES.get_or_init(|| {
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("..");
        let out = Command::new("go")
            .args(["test", "./language", "-run", "^TestWriteRuntimeFixtures$", "-count=1"])
            .current_dir(&root)
            .env("KILN_FIXTURES", dir())
            .output()
            .expect("the runtime's tests need Go (https://go.dev/dl) to compile their scripts");
        assert!(
            out.status.success(),
            "writing the fixtures failed:\n{}",
            String::from_utf8_lossy(&out.stdout)
        );
        let raw = std::fs::read_to_string(dir().join("fixtures.json")).expect("fixtures.json");
        serde_json::from_str(&raw).expect("the fixtures' JSON")
    })
}

fn module(file: &str) -> Vec<u8> {
    std::fs::read(dir().join(file)).expect("a fixture module")
}

fn float(bits: &str) -> f64 {
    f64::from_bits(u64::from_str_radix(bits, 16).expect("hex bits"))
}

type Settle = Box<dyn FnOnce(&mut Instance) -> Result<(), Error> + Send>;

/// What a case's natives get, as script_test.go's caseData: what it logs, and the promises to settle in order.
#[derive(Default)]
struct CaseData {
    logs: Vec<String>,
    pending: VecDeque<Settle>,
    resolvers: Vec<Resolver>,
}

/// A native's promise that the harness settles with settle once main returned.
fn later(
    call: &mut Call<'_, '_>,
    settle: impl FnOnce(&Resolver, &mut Instance) -> Result<(), Error> + Send + 'static,
) -> NativeResult {
    let (p, r) = call.new_promise()?;
    let data = call.data_mut::<CaseData>().ok_or("no harness settles promises here")?;
    data.resolvers.push(r.clone());
    data.pending.push_back(Box::new(move |inst| settle(&r, inst)));
    Ok(p)
}

/// The cases' "test" host module, as script_test.go has it.
fn test_host() -> HostModule {
    let objects = HostModule::new("test", "");
    let probes = objects.clone();
    HostModule::new("test", fixtures().hosts["test"].as_str())
        .native("log", |call, _, args| {
            let line = args[0].as_str().unwrap_or_default().to_string();
            if let Some(d) = call.data_mut::<CaseData>() {
                d.logs.push(line);
            } else if let Some(logs) = call.data_mut::<Vec<String>>() {
                logs.push(line);
            }
            Ok(Value::default())
        })
        .native("later", |call, _, args| {
            let v = args[0].clone();
            later(call, move |r, inst| r.resolve(inst, v))
        })
        .native("laterText", |call, _, args| {
            let v = args[0].clone();
            later(call, move |r, inst| r.resolve(inst, v))
        })
        .native("failLater", move |call, _, args| {
            let e = objects.object("ProbeError", args[0].as_str().unwrap_or_default().to_string());
            later(call, move |r, inst| r.reject(inst, e))
        })
        .native("ProbeError.code", |_, this, _| {
            Ok(Value::str(this.payload::<String>().ok_or("not a ProbeError")?.as_str()))
        })
        .native("probe", move |_, _, args| {
            Ok(probes.object("Probe", args[0].as_str().unwrap_or_default().to_string()))
        })
        .native("Probe.name", |_, this, _| {
            Ok(Value::str(this.payload::<String>().ok_or("not a Probe")?.as_str()))
        })
        .native("Probe.twice", |_, _, args| {
            let n = args[0].as_number().unwrap_or_default();
            if n < 0.0 {
                return Err("no negative numbers".into());
            }
            Ok(Value::num(n * 2.0))
        })
}

/// A value written as the cases' want sections write it (show in script_test.go).
fn show(v: &Value) -> String {
    use kiln::Kind;
    match v.kind() {
        Kind::String => quote(v.as_str().unwrap()),
        Kind::Number => format_number(v.as_number().unwrap()),
        Kind::Array => format!(
            "[{}]",
            v.as_array().unwrap().iter().map(show).collect::<Vec<_>>().join(", ")
        ),
        Kind::Map => format!(
            "{{{}}}",
            v.as_map()
                .unwrap()
                .iter()
                .map(|(k, x)| format!("{}: {}", show(k), show(x)))
                .collect::<Vec<_>>()
                .join(", ")
        ),
        Kind::Object => v.class_name().unwrap_or("?").to_string(),
        Kind::Nothing => "nothing".into(),
        _ => v.to_string(),
    }
}

/// Calls the case's main, then settles the promises its natives made, in order, as they are made.
fn run_main(module: &[u8], host: &HostModule) -> (Value, Vec<String>, Result<(), Error>) {
    let script = load_script(module, &[host]).expect("the case loads");
    let mut inst = script.instantiate(CaseData::default()).expect("an instance");
    let mut got = inst.call_function("main", &[]);
    while got.is_ok() {
        let Some(next) = inst.data_mut::<CaseData>().unwrap().pending.pop_front() else {
            break;
        };
        if let Err(e) = next(&mut inst) {
            got = Err(e);
        }
    }
    let logs = std::mem::take(&mut inst.data_mut::<CaseData>().unwrap().logs);
    match got {
        Ok(v) => (v, logs, Ok(())),
        Err(e) => (Value::default(), logs, Err(e)),
    }
}

#[test]
fn shared_script_cases() {
    let fx = fixtures();
    let host = test_host();
    let mut failures = Vec::new();
    for c in &fx.cases {
        let (got, logs, err) = run_main(&module(&c.module), &host);
        match (&c.eval, &err) {
            (Some(want), Err(Error::Script(e))) if e.to_string().contains(want.as_str()) => {}
            (Some(want), _) => failures.push(format!(
                "{}: main() = {}, {err:?}; want an evaluation error with {want:?}",
                c.name,
                show(&got)
            )),
            (None, Err(e)) => failures.push(format!("{}: main(): {e}", c.name)),
            (None, Ok(())) => {}
        }
        if let Some(want) = &c.want
            && &show(&got) != want
        {
            failures.push(format!("{}: main() = {}, want {want}", c.name, show(&got)));
        }
        if let Some(want) = &c.logs
            && &logs.join("\n") != want
        {
            failures.push(format!("{}: logs:\n{}\nwant:\n{want}", c.name, logs.join("\n")));
        }
    }
    assert!(
        failures.is_empty(),
        "{} of {} cases failed:\n{}",
        failures.len(),
        fx.cases.len(),
        failures.join("\n\n")
    );
    assert!(fx.cases.len() > 50, "only {} cases", fx.cases.len());
}

#[test]
fn numbers_are_written_as_go_writes_them() {
    for f in &fixtures().formats {
        assert_eq!(format_number(float(&f.bits)), f.text, "bits {}", f.bits);
    }
}

#[test]
fn numbers_are_read_as_go_reads_them() {
    for p in &fixtures().parses {
        let v = parse_number(&p.text).filter(|f| f.is_finite());
        match &p.bits {
            None => assert!(v.is_none(), "{:?} read as {v:?}, but Go refuses it", p.text),
            Some(bits) => {
                let v = v.unwrap_or_else(|| panic!("{:?} wasn't read", p.text));
                assert_eq!(
                    v.to_bits(),
                    float(bits).to_bits(),
                    "{:?} = {v}, want {}",
                    p.text,
                    float(bits)
                );
            }
        }
    }
}

#[test]
fn strings_are_quoted_as_go_quotes_them() {
    for (s, q) in &fixtures().quotes {
        assert_eq!(&quote(s), q);
    }
}

fn behaviour_host() -> HostModule {
    HostModule::new("host", fixtures().hosts["host"].as_str()).native("log", |call, _, args| {
        call.data_mut::<Vec<String>>()
            .unwrap()
            .push(args[0].as_str().unwrap().to_string());
        Ok(Value::default())
    })
}

#[test]
fn an_instance_takes_props_keeps_its_fields_and_calls_its_host() {
    let host = behaviour_host();
    let script = load_script(&module(&fixtures().modules["door"]), &[&host]).unwrap();
    let class = script.default_class().unwrap();
    assert_eq!(class.name(), "Door");
    let interfaces = Interface::parse_all(&fixtures().interfaces["host"].to_string()).unwrap();
    assert_eq!(class.satisfies(&interfaces["Behaviour"]), Ok(()));
    let mut door = script.new_instance(&class, Vec::<String>::new()).unwrap();
    assert_eq!(door.field_kind("speed"), Some(kiln::Kind::Number));
    assert_eq!(door.field_kind("opened"), None);
    assert!(door.set("speed", Value::str("fast")).is_err());
    door.set("speed", Value::num(2.0)).unwrap();
    assert!(door.has_method("onUpdate") && !door.has_method("onStart"));
    door.call("onUpdate", &[Value::num(0.5)]).unwrap();
    door.call("onUpdate", &[Value::num(0.5)]).unwrap();
    assert_eq!(door.data::<Vec<String>>().unwrap(), &["hi 1", "hi 2"]);
    assert_eq!(door.get("greeting"), Some(Value::str("hi")));
    let err = door.call("onUpdate", &[Value::str("x")]).unwrap_err();
    assert_eq!(err.to_string(), "Door.onUpdate wants a number, not a string");
}

#[test]
fn a_script_compiled_against_another_host_module_is_refused() {
    let fx = fixtures();
    let changed = HostModule::new(
        "test",
        format!("{}\nexport interface Extra {{ x: number }}", fx.hosts["test"]),
    );
    let logging = fx.cases.iter().find(|c| c.logs.is_some()).unwrap();
    let err = load_script(&module(&logging.module), &[&changed]).unwrap_err();
    assert!(err.to_string().contains("another version"), "{err}");
    let err = load_script(&module(&fx.modules["door"]), &[]).unwrap_err();
    assert!(err.to_string().contains("doesn't offer"), "{err}");
    let host = HostModule::new("host", fx.hosts["host"].as_str());
    let err = load_script(&module(&fx.modules["door"]), &[&host]).unwrap_err();
    assert_eq!(err.to_string(), "the host gives no code for the native log of \"host\"");
    assert!(
        load_script(b"not a module", &[])
            .unwrap_err()
            .to_string()
            .contains("not a compiled script")
    );
}

#[test]
fn the_collector_frees_garbage_and_keeps_what_the_instance_and_the_host_hold() {
    let script = load_script(&module(&fixtures().modules["churn"]), &[]).unwrap();
    let mut churn = script.new_instance(&script.default_class().unwrap(), ()).unwrap();
    for i in 0..200 {
        churn.call("churn", &[Value::num(f64::from(i))]).unwrap();
    }
    assert_eq!(churn.call("label", &[]).unwrap(), Value::str("kept 199"));
    let item = churn.call("make", &[Value::str("apple")]).unwrap();
    churn.keep(std::slice::from_ref(&item));
    for i in 0..50 {
        churn.call("churn", &[Value::num(f64::from(i))]).unwrap();
    }
    assert_eq!(
        churn.call_on(&item, "describe", &[]).unwrap(),
        Value::str("apple:new,APPLE")
    );
    let other = churn.call("make", &[Value::str("pear")]).unwrap();
    churn.call("label", &[]).unwrap();
    let err = churn.call_on(&other, "describe", &[]).unwrap_err();
    assert!(err.to_string().contains("isn't an object of this script"), "{err}");
    // more than the heap budget kept is refused
    let err = churn.call("hoard", &[Value::num(3000.0)]).unwrap_err();
    assert!(err.to_string().contains("memory"), "{err}");
}

#[derive(Debug)]
struct Tag {
    name: String,
}

#[test]
fn natives_take_and_give_objects_arrays_and_maps_and_dropped_host_objects_are_released() {
    let maker = HostModule::new("objects", "");
    let tags = maker.clone();
    let objects = HostModule::new("objects", fixtures().hosts["objects"].as_str())
        .native("tag", move |_, _, args| {
            Ok(tags.object(
                "Tag",
                Tag {
                    name: args[0].as_str().unwrap().into(),
                },
            ))
        })
        .native("Tag.name", |_, this, _| {
            Ok(Value::str(this.payload::<Tag>().unwrap().name.as_str()))
        })
        .native("Tag.shout", |_, this, _| {
            Ok(Value::str(this.payload::<Tag>().unwrap().name.to_uppercase() + "!"))
        })
        .native("sum", |_, _, args| {
            Ok(Value::num(
                args[0]
                    .as_array()
                    .unwrap_or_default()
                    .iter()
                    .filter_map(Value::as_number)
                    .sum(),
            ))
        })
        .native("counts", |_, _, args| {
            let mut counts: Vec<(Value, f64)> = Vec::new();
            for w in args[0].as_array().unwrap() {
                match counts.iter_mut().find(|(k, _)| k == w) {
                    Some((_, n)) => *n += 1.0,
                    None => counts.push((w.clone(), 1.0)),
                }
            }
            Ok(Value::map(counts.into_iter().map(|(k, n)| (k, Value::num(n)))))
        })
        .native("same", |_, _, args| Ok(Value::bool(args[0] == args[1])));
    let script = load_script(&module(&fixtures().modules["objects"]), &[&objects]).unwrap();
    let mut user = script.new_instance(&script.default_class().unwrap(), ()).unwrap();
    assert_eq!(user.call("run", &[]).unwrap(), Value::str("bell X! 6.5 212"));
    let bell = maker.object("Tag", Tag { name: "bell".into() });
    assert_eq!(
        user.call("hold", std::slice::from_ref(&bell)).unwrap(),
        Value::str("bell")
    );
    assert_eq!(user.call("again", &[bell]).unwrap(), Value::bool(true));
    let names = Value::map([
        (Value::str("a"), Value::bool(false)),
        (Value::str("b"), Value::bool(true)),
    ]);
    let xs = Value::array(vec![Value::num(1.0), Value::num(2.5)]);
    assert_eq!(user.call("total", &[xs, names]).unwrap(), Value::str("3.5 a+b true"));
    for i in 0..300 {
        user.call("see", &[maker.object("Tag", Tag { name: format!("t{i}") })])
            .unwrap();
    }
    assert_eq!(user.get("last"), Some(Value::str("t299")));
    assert!(
        user.native_count() < 100,
        "the instance still holds {} host objects",
        user.native_count()
    );
}

#[test]
fn classes_satisfy_interfaces_as_go_says() {
    let fx = fixtures();
    let hosts = [test_host(), behaviour_host(), {
        let h = HostModule::new("objects", fx.hosts["objects"].as_str());
        ["tag", "Tag.name", "Tag.shout", "sum", "counts", "same"]
            .into_iter()
            .fold(h, |h, n| h.native(n, |_, _, _| Ok(Value::default())))
    }];
    for c in &fx.satisfies {
        let host = hosts.iter().find(|h| h.name() == c.host).unwrap();
        let script = load_script(&module(&c.module), &[host]).unwrap();
        let interfaces = Interface::parse_all(&fx.interfaces[&c.host].to_string()).unwrap();
        let got = script
            .default_class()
            .unwrap()
            .satisfies(&interfaces[&c.interface])
            .err()
            .unwrap_or_default();
        assert_eq!(got, c.why, "{} against {}", c.module, c.interface);
    }
}

#[test]
fn errors_name_their_module_line_and_column() {
    let fx = fixtures();
    let host = test_host();
    let case = fx.cases.iter().find(|c| c.eval.is_some()).unwrap();
    let (_, _, err) = run_main(&module(&case.module), &host);
    let e = err.unwrap_err();
    let se = e.as_script().unwrap_or_else(|| panic!("{e:?} isn't the script's"));
    assert!(
        se.module.starts_with("script/") && se.line >= 1 && se.col >= 1,
        "{se:?}"
    );
}

#[test]
fn compiled_scripts_run_fast() {
    let script = load_script(&module(&fixtures().modules["churn"]), &[]).unwrap();
    let mut churn = script.new_instance(&script.default_class().unwrap(), ()).unwrap();
    let start = Instant::now();
    for i in 0..100 {
        churn.call("churn", &[Value::num(f64::from(i))]).unwrap();
    }
    // 100 calls of 100 objects and 50 strings each
    assert!(start.elapsed().as_millis() < 2000, "{:?}", start.elapsed());
}

#[test]
fn instances_move_between_threads() {
    fn send<T: Send>(_: &T) {}
    fn share<T: Send + Sync>(_: &T) {}
    let script = load_script(&module(&fixtures().modules["churn"]), &[]).unwrap();
    share(&script);
    let class = script.default_class().unwrap();
    let mut churn = script.new_instance(&class, ()).unwrap();
    send(&churn);
    let label = std::thread::spawn(move || {
        churn.call("churn", &[Value::num(7.0)]).unwrap();
        churn.call("label", &[]).unwrap()
    })
    .join()
    .unwrap();
    assert_eq!(label, Value::str("kept 7"));
}

#[test]
fn resolvers_refuse_what_their_promise_isnt_and_settle_once() {
    let fx = fixtures();
    let host = test_host();
    let case = fx
        .cases
        .iter()
        .find(|c| c.name.ends_with("/assignments take what an await gives"))
        .unwrap();
    let script = load_script(&module(&case.module), &[&host]).unwrap();
    let mut inst = script.instantiate(CaseData::default()).unwrap();
    inst.call_function("main", &[]).unwrap();
    let text = inst.data::<CaseData>().unwrap().resolvers[0].clone();
    let probe = HostModule::new("test", "");
    let err = text
        .reject(&mut inst, probe.object("ProbeError", "x".to_string()))
        .unwrap_err();
    assert_eq!(err.to_string(), "the promise can't fail");
    let err = text.resolve(&mut inst, Value::num(1.0)).unwrap_err();
    assert_eq!(err.to_string(), "a number is no string");
    assert!(text.pending());
    text.resolve(&mut inst, Value::str("hi")).unwrap();
    assert!(!text.pending());
    text.resolve(&mut inst, Value::str("again")).unwrap();
    let n = inst.data::<CaseData>().unwrap().resolvers[1].clone();
    let err = n.reject(&mut inst, probe.object("Probe", "p".to_string())).unwrap_err();
    assert_eq!(err.to_string(), "a Probe is no ProbeError");
    n.resolve(&mut inst, Value::num(7.0)).unwrap();
    assert_eq!(inst.data::<CaseData>().unwrap().logs, ["hi 7 true"]);
    // a resolver settles only the instance whose native made it
    let mut other = script.instantiate(CaseData::default()).unwrap();
    assert!(n.resolve(&mut other, Value::num(1.0)).is_err());
}
