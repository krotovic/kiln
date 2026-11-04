//! The expression cases every implementation of the expression language shares (language/testdata/cases.json).

use std::collections::HashMap;

use kiln::Value;
use kiln::expr::{Library, Limits, Scope, compile};

#[derive(serde::Deserialize)]
struct Case {
    src: String,
    #[serde(default)]
    env: HashMap<String, serde_json::Value>,
    #[serde(default)]
    scope: Vec<String>,
    #[serde(default)]
    want: Option<serde_json::Value>,
    #[serde(default)]
    error: Option<String>,
}

fn value_of(v: &serde_json::Value) -> Value {
    match v {
        serde_json::Value::Number(n) => Value::num(n.as_f64().expect("a number")),
        serde_json::Value::String(s) => Value::str(s.as_str()),
        serde_json::Value::Bool(b) => Value::bool(*b),
        _ => panic!("unsupported JSON value {v}"),
    }
}

#[test]
fn shared_cases() {
    #[derive(serde::Deserialize)]
    struct File {
        cases: Vec<Case>,
    }
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../language/testdata/cases.json");
    let file: File = serde_json::from_str(&std::fs::read_to_string(path).expect("cases.json")).expect("its JSON");
    let mut failures = Vec::new();
    for c in &file.cases {
        let env: HashMap<String, Value> = c.env.iter().map(|(k, v)| (k.clone(), value_of(v))).collect();
        let scope = Scope::new(&c.scope).with(c.env.keys());
        let prog = compile(&c.src, &Library::std(), &scope, &Limits::default());
        if c.error.as_deref() == Some("compile") {
            if let Ok(p) = prog {
                failures.push(format!("{}: compiled ({p:?}), want a compile error", c.src));
            }
            continue;
        }
        let prog = match prog {
            Ok(p) => p,
            Err(e) => {
                failures.push(format!("{}: compile: {e}", c.src));
                continue;
            }
        };
        let got = prog.eval(&env);
        if c.error.as_deref() == Some("eval") {
            if let Ok(v) = got {
                failures.push(format!("{}: = {v:?}, want an evaluation error", c.src));
            }
            continue;
        }
        match got {
            Err(e) => failures.push(format!("{}: eval: {e}", c.src)),
            Ok(v) => {
                let want = value_of(c.want.as_ref().expect("a want"));
                if v != want || v.kind() != want.kind() {
                    failures.push(format!(
                        "{}: = {v:?} ({}), want {want:?} ({})",
                        c.src,
                        v.kind(),
                        want.kind()
                    ));
                }
            }
        }
    }
    assert!(
        failures.is_empty(),
        "{} of {} cases failed:\n{}",
        failures.len(),
        file.cases.len(),
        failures.join("\n")
    );
}

#[test]
fn names_folding_and_literals() {
    let scope = Scope::new(["count", "name"]);
    let p = compile(
        "name + str(count * 2) + str(1 + 2)",
        &Library::std(),
        &scope,
        &Limits::default(),
    )
    .unwrap();
    assert_eq!(p.names(), ["count", "name"]);
    let env: HashMap<String, Value> = [
        ("count".to_string(), Value::literal("01")),
        ("name".to_string(), Value::literal("slime")),
    ]
    .into();
    assert_eq!(p.eval(&env).unwrap(), Value::str("slime23"));
    // a literal number keeps its text when it is joined to a string
    let p = compile("name + count", &Library::std(), &scope, &Limits::default()).unwrap();
    assert_eq!(p.eval(&env).unwrap(), Value::str("slime01"));
    let err = compile("nmae", &Library::std(), &scope, &Limits::default()).unwrap_err();
    assert_eq!(err.to_string(), "column 1: unknown name \"nmae\"");
    let err = compile("1 / 0", &Library::std(), &scope, &Limits::default())
        .unwrap()
        .eval(&env)
        .unwrap_err();
    assert_eq!(err.to_string(), "column 3: division by zero");
}
