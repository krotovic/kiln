//! Instances of compiled scripts: a wasmtime store each, the imports every script's module takes (module "alo"),
//! and values across the boundary (host.go and js/src/script.ts do the same).

use std::any::Any;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicI32, AtomicU64, Ordering};

use wasmtime::{AsContext, AsContextMut, Caller, Engine, Func, Linker, Memory, Store, TypedFunc, Val};

use super::meta::{
    HEADER, MAP_REF_VALS, MAP_STR_KEYS, PROMISE_REF, PROMISE_STRING, PROMISE_VALUE, SCRATCH, TAG_ARR_REFS,
    TAG_ARR_VALS, TAG_CLASS, TypeMeta,
};
use super::{Error, Script, ScriptInner, plural};
use crate::gofmt::{format_message, format_number, parse_number, quote};
use crate::value::{Kind, NativeObject, Repr, Resolver, ResolverInner, ScriptObject, ScriptPromise, ScriptRef, Value};

/// The most promises an instance's natives made that the host hasn't settled yet.
pub const MAX_PENDING_PROMISES: usize = 64;

static NEXT_INSTANCE: AtomicU64 = AtomicU64::new(1);

/// The runtime's exports a host calls, found once an instance is made.
#[derive(Clone)]
struct Runtime {
    instance: wasmtime::Instance,
    memory: Memory,
    begin: TypedFunc<(), ()>,
    end: TypedFunc<(), i32>,
    init: TypedFunc<(), ()>,
    str_new: TypedFunc<i32, i32>,
    arr_new: TypedFunc<(i32, i32), i32>,
    map_new: TypedFunc<i32, i32>,
    map_put: TypedFunc<i32, ()>,
    handle_new: TypedFunc<i32, i32>,
    handle_get: TypedFunc<i32, i32>,
    handle_drop: TypedFunc<i32, ()>,
    native_new: TypedFunc<(i32, i32), i32>,
    native_get: TypedFunc<i32, i32>,
    promise_new: Option<TypedFunc<i32, i32>>,
}

/// What a store holds: the instance's state as host.go's Instance has it.
pub(crate) struct State {
    script: Arc<ScriptInner>,
    data: Box<dyn Any + Send>,
    /// The error the script reported (fail, or a native's) before it trapped
    err: Option<Error>,
    rt: Option<Runtime>,
    id: u64,
    /// The class of its object; None for an instance of functions
    class: Option<usize>,
    this: i32,
    /// Handles of objects handed out during the last call
    temp: Vec<i32>,
    kept: Vec<i32>,
    /// Every handle the host holds: this, temp and kept
    live: HashSet<i32>,
    nat_objs: HashMap<i32, NativeObject>,
    nat_idx: HashMap<usize, i32>,
    nat_free: Vec<i32>,
    nat_next: i32,
    /// The promises natives made that the host hasn't settled yet
    pending: usize,
    /// The script's own exports, with how many results each gives
    fns: HashMap<String, (Func, usize)>,
}

/// The "alo" module every script imports.
pub(crate) fn imports(engine: &Engine) -> Linker<State> {
    let mut l = Linker::new(engine);
    l.func_wrap("alo", "fail", |mut c: Caller<'_, State>, code: i32, pos: i32| {
        let err = failure(&mut c, code, pos);
        c.data_mut().err = Some(err);
    })
    .expect("alo.fail");
    l.func_wrap("alo", "native", |mut c: Caller<'_, State>, id: i32, pos: i32| -> i32 {
        match native(&mut c, id, pos) {
            Ok(()) => 0,
            Err(e) => {
                c.data_mut().err = Some(e);
                1
            }
        }
    })
    .expect("alo.native");
    l.func_wrap(
        "alo",
        "fmt_num",
        |mut c: Caller<'_, State>, x: f64| -> wasmtime::Result<i32> {
            // a script out of memory unwinds its call
            new_string(&mut c, &format_number(x))
                .map(|p| p as i32)
                .map_err(wasmtime::Error::new)
        },
    )
    .expect("alo.fmt_num");
    l.func_wrap(
        "alo",
        "parse_num",
        |mut c: Caller<'_, State>, ptr: i32, len: i32| -> i32 {
            let text = read_bytes(&c, ptr as u32, len as u32);
            match parse_number(&text) {
                Some(f) if f.is_finite() => {
                    write_bytes(&mut c, SCRATCH, &f.to_le_bytes());
                    1
                }
                _ => 0,
            }
        },
    )
    .expect("alo.parse_num");
    l.func_wrap("alo", "fmod", |_: Caller<'_, State>, a: f64, b: f64| a % b)
        .expect("alo.fmod");
    l.func_wrap("alo", "release", |mut c: Caller<'_, State>, nh: i32| {
        let st = c.data_mut();
        if let Some(obj) = st.nat_objs.remove(&nh) {
            st.nat_idx.remove(&obj.id());
            st.nat_free.push(nh);
        }
    })
    .expect("alo.release");
    l
}

// ---- memory ----

fn rt(cx: &impl AsContext<Data = State>) -> Runtime {
    cx.as_context().data().rt.clone().expect("an instance's runtime")
}

fn read_bytes(cx: &impl AsContext<Data = State>, addr: u32, n: u32) -> String {
    let mem = rt(cx).memory;
    let data = mem.data(cx.as_context());
    let (a, n) = (addr as usize, n as usize);
    match data.get(a..a.saturating_add(n)) {
        Some(b) => String::from_utf8_lossy(b).into_owned(),
        None => String::new(),
    }
}

fn read<const N: usize>(cx: &impl AsContext<Data = State>, addr: u32) -> [u8; N] {
    let mem = rt(cx).memory;
    let data = mem.data(cx.as_context());
    let a = addr as usize;
    data.get(a..a + N).and_then(|b| b.try_into().ok()).unwrap_or([0; N])
}

fn u32_at(cx: &impl AsContext<Data = State>, addr: u32) -> u32 {
    u32::from_le_bytes(read(cx, addr))
}

fn write_bytes(cx: &mut impl AsContextMut<Data = State>, addr: u32, b: &[u8]) {
    let mem = rt(cx).memory;
    let data = mem.data_mut(cx.as_context_mut());
    let a = addr as usize;
    if let Some(dst) = data.get_mut(a..a + b.len()) {
        dst.copy_from_slice(b);
    }
}

fn read_string(cx: &impl AsContext<Data = State>, p: u32) -> String {
    let n = u32_at(cx, p + 12);
    read_bytes(cx, p + HEADER, n)
}

fn write_slot(cx: &mut impl AsContextMut<Data = State>, addr: u32, raw: u64, t: &TypeMeta) {
    if t.is("n") {
        write_bytes(cx, addr, &raw.to_le_bytes());
    } else {
        write_bytes(cx, addr, &(raw as u32).to_le_bytes());
    }
}

fn read_slot(cx: &impl AsContext<Data = State>, addr: u32, t: &TypeMeta) -> u64 {
    if t.is("n") {
        u64::from_le_bytes(read(cx, addr))
    } else {
        u64::from(u32_at(cx, addr))
    }
}

// ---- calls into the module ----

/// The error a call stopped with: the script's own, which the runtime reported before it trapped, or the trap.
fn trapped(cx: &impl AsContext<Data = State>, e: wasmtime::Error) -> Error {
    let st = cx.as_context().data();
    if let Some(err) = &st.err {
        return err.clone();
    }
    if let Some(err) = e.downcast_ref::<Error>() {
        return err.clone();
    }
    st.script.meta.error_at(0, format!("the script stopped: {e}"))
}

/// Calls one of the runtime's exports from the host, while a call may be under way: an error is this call's own,
/// never one an earlier call left.
fn helper<P: wasmtime::WasmParams, R: wasmtime::WasmResults>(
    cx: &mut impl AsContextMut<Data = State>,
    f: &TypedFunc<P, R>,
    args: P,
) -> Result<R, Error> {
    let saved = cx.as_context_mut().data_mut().err.take();
    match f.call(&mut *cx, args) {
        Ok(r) => {
            let mut ctx = cx.as_context_mut();
            let st = ctx.data_mut();
            if st.err.is_none() {
                st.err = saved;
            }
            Ok(r)
        }
        Err(e) => Err(trapped(cx, e)),
    }
}

fn new_string(cx: &mut impl AsContextMut<Data = State>, s: &str) -> Result<u32, Error> {
    let f = rt(cx).str_new;
    let p = helper(cx, &f, s.len() as i32)? as u32;
    write_bytes(cx, p + HEADER, s.as_bytes());
    Ok(p)
}

fn handle_get(cx: &mut impl AsContextMut<Data = State>, h: i32) -> Result<u32, Error> {
    let f = rt(cx).handle_get;
    Ok(helper(cx, &f, h)? as u32)
}

fn handle_new(cx: &mut impl AsContextMut<Data = State>, p: u32) -> Result<i32, Error> {
    let f = rt(cx).handle_new;
    helper(cx, &f, p as i32)
}

fn handle_drop(cx: &mut impl AsContextMut<Data = State>, h: i32) {
    let f = rt(cx).handle_drop;
    let _ = helper(cx, &f, h);
}

fn raw_val(raw: u64, t: &TypeMeta) -> Val {
    if t.is("n") {
        Val::F64(raw)
    } else {
        Val::I32(raw as u32 as i32)
    }
}

fn val_raw(v: &Val) -> u64 {
    match v {
        Val::F64(bits) => *bits,
        Val::I32(i) => u64::from(*i as u32),
        Val::I64(i) => *i as u64,
        Val::F32(bits) => u64::from(*bits),
        _ => 0,
    }
}

/// Calls a script's export with its arguments, giving its result (0 for none).
fn call_export(cx: &mut impl AsContextMut<Data = State>, name: &str, args: &[Val]) -> Result<u64, Error> {
    let found = cx.as_context().data().fns.get(name).cloned();
    let (f, results) = match found {
        Some(f) => f,
        None => {
            let inst = rt(cx).instance;
            let f = inst
                .get_func(&mut *cx, name)
                .ok_or_else(|| Error::host(format!("the compiled script has no {name}")))?;
            let n = f.ty(&*cx).results().len();
            cx.as_context_mut().data_mut().fns.insert(name.into(), (f, n));
            (f, n)
        }
    };
    let mut out = vec![Val::I32(0); results];
    if let Err(e) = f.call(&mut *cx, args, &mut out) {
        return Err(trapped(cx, e));
    }
    Ok(out.first().map(val_raw).unwrap_or(0))
}

// ---- values across the boundary ----

fn fits(v: &Value, t: &TypeMeta) -> bool {
    match t.k.as_str() {
        "p" => matches!(v.0, Repr::Promise(_)),
        "n" => v.kind() == Kind::Number,
        "s" => v.kind() == Kind::String,
        "b" => v.kind() == Kind::Bool,
        "a" => matches!(v.kind(), Kind::Array | Kind::Null),
        "m" => matches!(v.kind(), Kind::Map | Kind::Null),
        "c" | "i" => matches!(v.kind(), Kind::Object | Kind::Null),
        "v" => true,
        _ => false,
    }
}

/// Turns a host value of type t into the script's representation, putting strings, arrays and maps into its
/// memory.
fn to_wasm(cx: &mut impl AsContextMut<Data = State>, v: &Value, t: &TypeMeta) -> Result<u64, Error> {
    if !fits(v, t) {
        return Err(Error::host(format!("a {} is no {}", v.kind(), t.name)));
    }
    match &v.0 {
        Repr::Number(f, _) if t.is("n") => return Ok(f.to_bits()),
        Repr::Bool(b) if t.is("b") => return Ok(u64::from(*b)),
        Repr::String(s) if t.is("s") => return new_string(cx, s).map(u64::from),
        Repr::Null => return Ok(0),
        _ => {}
    }
    let r = rt(cx);
    match &v.0 {
        Repr::Array(items) if t.is("a") => {
            let e = t.elem();
            let raw = items
                .iter()
                .map(|item| to_wasm(cx, item, e))
                .collect::<Result<Vec<_>, _>>()?;
            let tag = if e.is_ref() { TAG_ARR_REFS } else { TAG_ARR_VALS };
            let p = helper(cx, &r.arr_new, (tag, items.len() as i32))? as u32;
            let st = u32_at(cx, p + 16);
            for (i, raw) in raw.into_iter().enumerate() {
                write_slot(cx, st + HEADER + 8 * i as u32, raw, e);
            }
            Ok(u64::from(p))
        }
        Repr::Map(m) if t.is("m") => {
            let (kt, vt) = (t.key(), t.elem());
            let mut flags = 0;
            if kt.is("s") {
                flags |= MAP_STR_KEYS;
            }
            if vt.is_ref() {
                flags |= MAP_REF_VALS;
            }
            let p = helper(cx, &r.map_new, flags)?;
            for (k, val) in m.iter() {
                let k = to_wasm(cx, k, kt)?;
                let val = to_wasm(cx, val, vt)?;
                write_slot(cx, SCRATCH, k, kt);
                write_slot(cx, SCRATCH + 8, val, vt);
                helper(cx, &r.map_put, p)?;
            }
            Ok(u64::from(p as u32))
        }
        Repr::Promise(ScriptPromise(res)) => {
            let id = cx.as_context().data().id;
            if res.0.instance != id || res.handle() == 0 {
                return Err(Error::host("a promise of another instance, or one settled already"));
            }
            handle_get(cx, res.handle()).map(u64::from)
        }
        Repr::Script(o) => {
            let st = cx.as_context().data();
            if o.0.instance != st.id {
                return Err(Error::host("an object of another instance of the script"));
            }
            if !st.live.contains(&o.handle()) {
                return Err(Error::host(
                    "an object of the script from an earlier call, which the host didn't keep",
                ));
            }
            handle_get(cx, o.handle()).map(u64::from)
        }
        Repr::Native(obj) => {
            let mut ctx = cx.as_context_mut();
            let st = ctx.data_mut();
            let Some(&cls) = st.script.native_classes.get(&format!("{}:{}", obj.host(), obj.class())) else {
                return Err(Error::host(format!(
                    "the script doesn't know the host's class {}",
                    obj.class()
                )));
            };
            if let Some(&nh) = st.nat_idx.get(&obj.id()) {
                return Ok(u64::from(helper(cx, &r.native_get, nh)? as u32));
            }
            let nh = match st.nat_free.pop() {
                Some(nh) => nh,
                None => {
                    st.nat_next += 1;
                    st.nat_next - 1
                }
            };
            st.nat_idx.insert(obj.id(), nh);
            st.nat_objs.insert(nh, obj.clone());
            Ok(u64::from(helper(cx, &r.native_new, (cls as i32, nh))? as u32))
        }
        _ => Err(Error::host(format!("a {} is no {}", v.kind(), t.name))),
    }
}

/// Turns the script's value raw of type t into a host value: strings, arrays and maps copied out, and objects by
/// handle (valid until the next call, unless kept).
fn from_wasm(cx: &mut impl AsContextMut<Data = State>, raw: u64, t: &TypeMeta) -> Result<Value, Error> {
    match t.k.as_str() {
        "n" => return Ok(Value::num(f64::from_bits(raw))),
        "b" => return Ok(Value::bool(raw as u32 != 0)),
        "s" => return Ok(Value::str(read_string(cx, raw as u32))),
        // the host doesn't hold the script's promises: an async method's promise is ignored
        "v" | "p" => return Ok(Value::default()),
        _ => {}
    }
    let p = raw as u32;
    if p == 0 {
        return Ok(Value::null());
    }
    match t.k.as_str() {
        "a" => {
            let (n, st) = (u32_at(cx, p + 12), u32_at(cx, p + 16));
            let e = t.elem();
            let mut items = Vec::with_capacity(n as usize);
            for i in 0..n {
                let raw = read_slot(cx, st + HEADER + 8 * i, e);
                items.push(from_wasm(cx, raw, e)?);
            }
            return Ok(Value::array(items));
        }
        "m" => {
            let (n, ks, vs) = (u32_at(cx, p + 12), u32_at(cx, p + 16), u32_at(cx, p + 20));
            let (kt, vt) = (t.key(), t.elem());
            let mut entries = Vec::with_capacity(n as usize);
            for i in 0..n {
                let k = read_slot(cx, ks + HEADER + 8 * i, kt);
                let v = read_slot(cx, vs + HEADER + 8 * i, vt);
                entries.push((from_wasm(cx, k, kt)?, from_wasm(cx, v, vt)?));
            }
            return Ok(Value::map(entries));
        }
        _ => {}
    }
    let script = cx.as_context().data().script.clone();
    let cls = i64::from(u32_at(cx, p) & 0x7fff_ffff) - TAG_CLASS;
    let Some(cm) = usize::try_from(cls).ok().and_then(|c| script.meta.classes.get(c)) else {
        return Err(Error::host("the script gave something that isn't an object"));
    };
    if cm.native {
        let nh = u32_at(cx, p + 16) as i32;
        return match cx.as_context().data().nat_objs.get(&nh) {
            Some(obj) => Ok(Value(Repr::Native(obj.clone()))),
            None => Err(Error::host("a host object the instance no longer knows")),
        };
    }
    let h = handle_new(cx, p)?;
    let mut ctx = cx.as_context_mut();
    let st = ctx.data_mut();
    st.temp.push(h);
    st.live.insert(h);
    Ok(Value(Repr::Script(ScriptObject(Arc::new(ScriptRef {
        instance: st.id,
        handle: AtomicI32::new(h),
        class: cls as usize,
        class_name: cm.name.as_str().into(),
    })))))
}

// ---- the imports ----

/// The error the runtime reported with code, its arguments in the scratch slots.
fn failure(cx: &mut Caller<'_, State>, code: i32, pos: i32) -> Error {
    let script = cx.data().script.clone();
    let meta = &script.meta;
    let msg = match usize::try_from(code).ok().and_then(|c| meta.messages.get(c)) {
        None => "the script failed".to_string(),
        Some(msg) => format_message(msg, |i, kind| {
            let addr = SCRATCH + 8 * i as u32;
            match kind {
                b'i' => (u32_at(cx, addr) as i32).to_string(),
                b'n' => format_number(f64::from_le_bytes(read(cx, addr))),
                b's' => read_string(cx, u32_at(cx, addr)),
                b'q' => quote(&read_string(cx, u32_at(cx, addr))),
                _ => String::new(),
            }
        }),
    };
    meta.error_at(pos, msg)
}

/// Runs native id with what the script put in the scratch slots, and puts its result in slot 0.
fn native(cx: &mut Caller<'_, State>, id: i32, pos: i32) -> Result<(), Error> {
    let script = cx.data().script.clone();
    let meta = &script.meta;
    let Some(i) = usize::try_from(id).ok().filter(|&i| i < script.natives.len()) else {
        return Err(meta.error_at(pos, format!("no native {id}")));
    };
    let nm = &meta.natives[i];
    let member = &script.native_names[i];
    let first = u32::from(nm.this);
    let raw: Vec<u64> = nm
        .params
        .iter()
        .enumerate()
        .map(|(j, p)| read_slot(cx, SCRATCH + 8 * (first + j as u32), p))
        .collect();
    let mut this = Value::default();
    if nm.this {
        let obj = u32_at(cx, SCRATCH);
        let nh = u32_at(cx, obj + 16) as i32;
        match cx.data().nat_objs.get(&nh) {
            Some(o) => this = Value(Repr::Native(o.clone())),
            None => return Err(meta.error_at(pos, format!("{member}: a host object the instance no longer knows"))),
        }
    }
    let mut args = Vec::with_capacity(raw.len());
    for (raw, p) in raw.into_iter().zip(&nm.params) {
        args.push(from_wasm(cx, raw, p).map_err(|e| meta.error_at(pos, format!("{member}: {e}")))?);
    }
    let f = script.natives[i].clone();
    let out = f(
        &mut Call {
            caller: cx,
            result: &nm.result,
        },
        &this,
        &args,
    )
    .map_err(|e| {
        if let Some(Error::Script(se)) = e.downcast_ref::<Error>() {
            return Error::Script(se.clone());
        }
        if let Some(se) = e.downcast_ref::<super::ScriptError>() {
            return Error::Script(se.clone());
        }
        meta.error_at(pos, format!("{member}: {e}"))
    })?;
    if nm.result.is("v") {
        return Ok(());
    }
    if !fits(&out, &nm.result) {
        return Err(meta.error_at(pos, format!("{member} gave a {}, not a {}", out.kind(), nm.result.name)));
    }
    let r = to_wasm(cx, &out, &nm.result).map_err(|e| meta.error_at(pos, format!("{member}: {e}")))?;
    write_slot(cx, SCRATCH, r, &nm.result);
    Ok(())
}

/// What a native knows about the call it serves.
pub struct Call<'a, 'b> {
    caller: &'a mut Caller<'b, State>,
    result: &'a TypeMeta,
}

impl Call<'_, '_> {
    /// What the host gave the instance ([`Script::new_instance`]) this call came from, when it is a T.
    pub fn data<T: Any>(&self) -> Option<&T> {
        self.caller.data().data.downcast_ref()
    }

    /// The same, to change.
    pub fn data_mut<T: Any>(&mut self) -> Option<&mut T> {
        self.caller.data_mut().data.downcast_mut()
    }

    /// Makes a pending promise for the native's result, which must be a Promise: the native returns the promise
    /// and keeps the resolver to settle it later ([`Resolver::resolve`], [`Resolver::reject`]). It fails when the
    /// instance has [`MAX_PENDING_PROMISES`] already.
    pub fn new_promise(&mut self) -> Result<(Value, Resolver), Error> {
        if !self.result.is("p") {
            return Err(Error::host("only a native that returns a Promise makes one"));
        }
        if self.caller.data().pending >= MAX_PENDING_PROMISES {
            return Err(Error::host(format!(
                "more than {MAX_PENDING_PROMISES} promises are waiting for the host"
            )));
        }
        let e = self.result.elem();
        let kind = if e.is("s") {
            PROMISE_STRING
        } else if e.is_ref() && !e.is("v") {
            PROMISE_REF
        } else {
            PROMISE_VALUE
        };
        let Some(f) = rt(self.caller).promise_new else {
            return Err(Error::host("the script makes no promises"));
        };
        let p = helper(self.caller, &f, kind)? as u32;
        let h = handle_new(self.caller, p)?;
        let st = self.caller.data_mut();
        st.pending += 1;
        let r = Resolver(Arc::new(ResolverInner {
            instance: st.id,
            handle: AtomicI32::new(h),
            value: e.clone(),
            err: self.result.err.as_deref().cloned(),
        }));
        Ok((Value(Repr::Promise(ScriptPromise(r.clone()))), r))
    }
}

/// An object of a script's class that the host keeps and calls into, with a module instance (a wasmtime store) of
/// its own. Objects of the script it hands the host (results, natives' arguments) stay valid until the host's next
/// call into it, unless the host keeps them ([`Instance::keep`]). Dropping it frees it.
pub struct Instance {
    store: Store<State>,
    script: Script,
}

impl std::fmt::Debug for Instance {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Instance({})", self.script.0.meta.entry)
    }
}

impl Instance {
    fn make(script: &Script, data: Box<dyn Any + Send>, class: Option<usize>) -> Result<Instance, Error> {
        let state = State {
            script: script.0.clone(),
            data,
            err: None,
            rt: None,
            id: NEXT_INSTANCE.fetch_add(1, Ordering::Relaxed),
            class,
            this: 0,
            temp: Vec::new(),
            kept: Vec::new(),
            live: HashSet::new(),
            nat_objs: HashMap::new(),
            nat_idx: HashMap::new(),
            nat_free: Vec::new(),
            nat_next: 1,
            pending: 0,
            fns: HashMap::new(),
        };
        let mut store = Store::new(super::engine(), state);
        let inst = script
            .0
            .pre
            .instantiate(&mut store)
            .map_err(|e| Error::host(format!("instantiating: {e}")))?;
        let missing = |name: &str| Error::host(format!("the compiled script has no {name}"));
        macro_rules! typed {
            ($name:literal) => {
                inst.get_typed_func(&mut store, $name)
                    .map_err(|_| missing($name))?
            };
        }
        let rt = Runtime {
            instance: inst,
            memory: inst.get_memory(&mut store, "memory").ok_or_else(|| missing("memory"))?,
            begin: typed!("__begin"),
            end: typed!("__end"),
            init: typed!("__init"),
            str_new: typed!("__str_new"),
            arr_new: typed!("__arr_new"),
            map_new: typed!("__map_new"),
            map_put: typed!("__map_put"),
            handle_new: typed!("__handle_new"),
            handle_get: typed!("__handle_get"),
            handle_drop: typed!("__handle_drop"),
            native_new: typed!("__native_new"),
            native_get: typed!("__native_get"),
            promise_new: inst.get_typed_func(&mut store, "__promise_new").ok(),
        };
        store.data_mut().rt = Some(rt);
        let mut inst = Instance {
            store,
            script: script.clone(),
        };
        inst.init_consts()?;
        Ok(inst)
    }

    pub(crate) fn functions(script: &Script, data: Box<dyn Any + Send>) -> Result<Instance, Error> {
        Instance::make(script, data, None)
    }

    pub(crate) fn construct(script: &Script, class: usize, data: Box<dyn Any + Send>) -> Result<Instance, Error> {
        let mut inst = Instance::make(script, data, Some(class))?;
        inst.begin();
        let cx = &mut inst.store;
        let obj = call_export(cx, &format!("n{class}"), &[])? as u32;
        let this = handle_new(cx, obj)?;
        cx.data_mut().this = this;
        cx.data_mut().live.insert(this);
        call_export(cx, &format!("c{class}"), &[Val::I32(obj as i32)])?;
        inst.end()?;
        Ok(inst)
    }

    /// Computes the module's constants.
    fn init_consts(&mut self) -> Result<(), Error> {
        self.begin();
        let f = rt(&self.store).init;
        if let Err(e) = f.call(&mut self.store, ()) {
            return Err(trapped(&self.store, e));
        }
        self.end()
    }

    fn begin(&mut self) {
        self.store.data_mut().err = None;
        let f = rt(&self.store).begin;
        let _ = f.call(&mut self.store, ());
    }

    /// Ends a call: it collects when it is time, and fails when the instance keeps more than MaxHeap.
    fn end(&mut self) -> Result<(), Error> {
        let f = rt(&self.store).end;
        let live = helper(&mut self.store, &f, ())?;
        let st = self.store.data();
        let limit = st.script.meta.limits.max_heap;
        if let Some(class) = st.class
            && live >= 0
            && i64::from(live) > limit
        {
            let cm = &st.script.meta.classes[class];
            let msg = format!("{} keeps more than {limit} bytes of memory", cm.name);
            return Err(st.script.meta.error_at(cm.pos, msg));
        }
        Ok(())
    }

    /// Forgets the handles of the objects the last call handed out, unless they were kept.
    fn drop_temps(&mut self) {
        let temp = std::mem::take(&mut self.store.data_mut().temp);
        for h in temp {
            self.store.data_mut().live.remove(&h);
            handle_drop(&mut self.store, h);
        }
    }

    /// Runs an export: a method of the object with handle this (None: a function), with arguments of the given
    /// types and a result of type result.
    fn invoke(
        &mut self,
        export: &str,
        this: Option<i32>,
        args: &[Value],
        params: &[TypeMeta],
        result: &TypeMeta,
    ) -> Result<Value, Error> {
        self.begin();
        let mut raw = Vec::with_capacity(args.len() + 1);
        for (a, p) in args.iter().zip(params) {
            let r = to_wasm(&mut self.store, a, p)?;
            raw.push(raw_val(r, p));
        }
        if let Some(h) = this {
            raw.insert(0, Val::I32(handle_get(&mut self.store, h)? as i32));
        }
        self.drop_temps();
        let out = call_export(&mut self.store, export, &raw)?;
        let value = if result.is("v") {
            Value::default()
        } else {
            from_wasm(&mut self.store, out, result)?
        };
        self.end()?;
        Ok(value)
    }

    fn meta(&self) -> &super::ScriptMeta {
        &self.script.0.meta
    }

    /// The class of the instance's object.
    fn class_meta(&self) -> Option<&super::meta::ClassMeta> {
        self.store.data().class.map(|c| &self.script.0.meta.classes[c])
    }

    /// Whether the instance's class has a public method name.
    pub fn has_method(&self, name: &str) -> bool {
        self.class_meta()
            .and_then(|cm| cm.methods.get(name))
            .is_some_and(|m| !m.private)
    }

    /// Calls the instance's public method with args, which must have the method's parameter types.
    pub fn call(&mut self, method: &str, args: &[Value]) -> Result<Value, Error> {
        let st = self.store.data();
        let Some(class) = st.class else {
            return Err(Error::host("the instance has no object of its own"));
        };
        let this = st.this;
        self.call_on_handle(this, class, method, args)
    }

    /// Whether obj, an object of this instance's script, has a public method name.
    pub fn has_method_on(&self, obj: &Value, name: &str) -> bool {
        self.own(obj).is_some_and(|o| {
            self.meta().classes[o.0.class]
                .methods
                .get(name)
                .is_some_and(|m| !m.private)
        })
    }

    /// Calls the public method of obj, an object of this instance's script that it handed out, under the same
    /// budgets as one call from the host.
    pub fn call_on(&mut self, obj: &Value, method: &str, args: &[Value]) -> Result<Value, Error> {
        let Some(o) = self.own(obj) else {
            return Err(Error::host(format!("{obj} isn't an object of this script")));
        };
        self.call_on_handle(o.handle(), o.0.class, method, args)
    }

    /// Calls a function the entry exports on this instance.
    pub fn call_function(&mut self, name: &str, args: &[Value]) -> Result<Value, Error> {
        let script = self.script.clone();
        let meta = &script.0.meta;
        let fm = meta
            .funcs
            .get(name)
            .ok_or_else(|| Error::host(format!("{} exports no function {name}", meta.entry)))?;
        if args.len() != fm.params.len() {
            return Err(Error::host(format!("{name} takes {}", plural(fm.params.len()))));
        }
        for (a, p) in args.iter().zip(&fm.params) {
            if !fits(a, p) {
                return Err(Error::host(format!("{name} wants a {}, not a {}", p.name, a.kind())));
            }
        }
        self.invoke(&fm.export, None, args, &fm.params, &fm.result)
    }

    fn call_on_handle(&mut self, this: i32, class: usize, method: &str, args: &[Value]) -> Result<Value, Error> {
        let script = self.script.clone();
        let cm = &script.0.meta.classes[class];
        let m = match cm.methods.get(method) {
            Some(m) if !m.private && !m.export.is_empty() => m,
            _ => return Err(Error::host(format!("{} has no method {method}", cm.name))),
        };
        if args.len() != m.params.len() {
            return Err(Error::host(format!(
                "{}.{method} takes {}",
                cm.name,
                plural(m.params.len())
            )));
        }
        for (a, p) in args.iter().zip(&m.params) {
            if !fits(a, p) {
                return Err(Error::host(format!(
                    "{}.{method} wants a {}, not a {}",
                    cm.name,
                    p.name,
                    a.kind()
                )));
            }
        }
        self.invoke(&m.export, Some(this), args, &m.params, &m.result)
    }

    /// The instance's object (None for an instance of functions).
    pub fn self_object(&self) -> Option<Value> {
        let st = self.store.data();
        let class = st.class?;
        Some(Value(Repr::Script(ScriptObject(Arc::new(ScriptRef {
            instance: st.id,
            handle: AtomicI32::new(st.this),
            class,
            class_name: self.meta().classes[class].name.as_str().into(),
        })))))
    }

    /// Tells the instance which more objects of the script the host holds on to: they stay valid past the next
    /// call, and their memory counts as the instance's own. It replaces what keep was given before.
    pub fn keep(&mut self, objs: &[Value]) {
        let this = self.store.data().this;
        let mut kept = Vec::new();
        let mut refs = Vec::new();
        for o in objs {
            let Some(o) = self.own(o) else { continue };
            if o.handle() == this && self.store.data().class.is_some() {
                continue;
            }
            let Ok(p) = handle_get(&mut self.store, o.handle()) else {
                continue;
            };
            if let Ok(h) = handle_new(&mut self.store, p) {
                kept.push(h);
                refs.push(o);
            }
        }
        let old = std::mem::take(&mut self.store.data_mut().kept);
        for h in old {
            self.store.data_mut().live.remove(&h);
            handle_drop(&mut self.store, h);
        }
        let st = self.store.data_mut();
        for &h in &kept {
            st.live.insert(h);
        }
        for (o, &h) in refs.iter().zip(&kept) {
            o.0.handle.store(h, Ordering::Relaxed);
        }
        st.kept = kept;
    }

    /// The handle of obj, an object of this instance the host still holds.
    fn own(&self, obj: &Value) -> Option<ScriptObject> {
        let st = self.store.data();
        match &obj.0 {
            Repr::Script(o) if o.0.instance == st.id && st.live.contains(&o.handle()) => Some(o.clone()),
            _ => None,
        }
    }

    fn field(&self, name: &str) -> Option<&super::meta::FieldMeta> {
        self.class_meta()?.fields.iter().find(|f| f.name == name)
    }

    /// The kind of the instance's public field name, when it is a number, a string or a bool, so a host can set it
    /// from text.
    pub fn field_kind(&self, name: &str) -> Option<Kind> {
        let f = self.field(name).filter(|f| !f.private)?;
        match f.typ.k.as_str() {
            "n" => Some(Kind::Number),
            "s" => Some(Kind::String),
            "b" => Some(Kind::Bool),
            _ => None,
        }
    }

    /// Sets the instance's public field name (a number, string or bool field, readonly ones included) to v.
    pub fn set(&mut self, name: &str, v: Value) -> Result<(), Error> {
        let cname = self.class_meta().map(|cm| cm.name.clone()).unwrap_or_default();
        let Some(kind) = self.field_kind(name) else {
            return Err(Error::host(format!(
                "{cname} has no field {name} of a number, string or bool"
            )));
        };
        if v.kind() != kind {
            return Err(Error::host(format!("{cname}.{name} is a {kind}, not a {}", v.kind())));
        }
        let script = self.script.clone();
        let f = script.0.meta.classes[self.store.data().class.unwrap_or_default()]
            .fields
            .iter()
            .find(|f| f.name == name)
            .expect("the field");
        self.begin();
        let raw = to_wasm(&mut self.store, &v, &f.typ)?;
        let this = self.store.data().this;
        let p = handle_get(&mut self.store, this)?;
        write_slot(&mut self.store, p + f.offset, raw, &f.typ);
        self.end()
    }

    /// Reads the instance's field name (tests and tools use it).
    pub fn get(&mut self, name: &str) -> Option<Value> {
        let script = self.script.clone();
        let class = self.store.data().class?;
        let f = script.0.meta.classes[class].fields.iter().find(|f| f.name == name)?;
        let this = self.store.data().this;
        let p = handle_get(&mut self.store, this).ok()?;
        let raw = read_slot(&self.store, p + f.offset, &f.typ);
        from_wasm(&mut self.store, raw, &f.typ).ok()
    }

    /// What the host gave the instance, when it is a T.
    pub fn data<T: Any>(&self) -> Option<&T> {
        self.store.data().data.downcast_ref()
    }

    /// The same, to change.
    pub fn data_mut<T: Any>(&mut self) -> Option<&mut T> {
        self.store.data_mut().data.downcast_mut()
    }

    /// How many host objects the instance holds for the script.
    pub fn native_count(&self) -> usize {
        self.store.data().nat_objs.len()
    }

    /// Settles r's promise with v (or, rejecting, the error v), as one call from the host. A value that can't go
    /// into the script gives the promise up: it never settles, and no longer counts as pending.
    pub(crate) fn settle(&mut self, r: &Resolver, v: &Value, reject: bool) -> Result<(), Error> {
        if r.0.instance != self.store.data().id {
            return Err(Error::host("a promise of another instance"));
        }
        if !r.pending() {
            return Ok(());
        }
        let (export, typ) = if reject {
            let Some(err) = &r.0.err else {
                return Err(Error::host("the promise can't fail"));
            };
            let cls = v.as_native().and_then(|o| {
                self.script
                    .0
                    .native_classes
                    .get(&format!("{}:{}", o.host(), o.class()))
                    .copied()
            });
            if cls != Some(err.c) {
                let what = v
                    .class_name()
                    .map(str::to_string)
                    .unwrap_or_else(|| v.kind().to_string());
                return Err(Error::host(format!("a {what} is no {}", err.name)));
            }
            ("__promise_reject", err.clone())
        } else {
            if !r.0.value.is("v") && !fits(v, &r.0.value) {
                return Err(Error::host(format!("a {} is no {}", v.kind(), r.0.value.name)));
            }
            (
                if r.0.value.is("n") {
                    "__promise_resolve_f"
                } else {
                    "__promise_resolve_i"
                },
                r.0.value.clone(),
            )
        };
        self.begin();
        let raw = if !reject && typ.is("v") {
            Ok(0)
        } else {
            to_wasm(&mut self.store, v, &typ)
        };
        let p = handle_get(&mut self.store, r.handle());
        handle_drop(&mut self.store, r.handle());
        r.0.handle.store(0, Ordering::Relaxed);
        self.store.data_mut().pending -= 1;
        let raw = raw?;
        let p = p?;
        self.drop_temps();
        let value = if export == "__promise_resolve_f" {
            Val::F64(raw)
        } else {
            Val::I32(raw as u32 as i32)
        };
        call_export(&mut self.store, export, &[Val::I32(p as i32), value])?;
        self.end()
    }
}

impl Resolver {
    /// Fulfils the promise with v, a value of its type (anything for a `Promise<void>`), and runs what waited for
    /// it on inst, the instance whose native made it. The error is the script's, when what ran failed.
    pub fn resolve(&self, inst: &mut Instance, v: Value) -> Result<(), Error> {
        inst.settle(self, &v, false)
    }

    /// Fails the promise with e, an object of its error class (a native class of its host module), and runs what
    /// waited for it.
    pub fn reject(&self, inst: &mut Instance, e: Value) -> Result<(), Error> {
        inst.settle(self, &e, true)
    }
}
