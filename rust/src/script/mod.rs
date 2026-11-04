//! Compiled scripts on wasmtime: the Rust host of the modules Kiln's compiler makes, through the interface
//! `language/abi.go` defines (the imports, exports, memory layout, errors and metadata), as the Go host
//! (`language/host.go`) and the browser's (`js/src/script.ts`) are.

mod instance;
mod meta;

use std::any::Any;
use std::collections::HashMap;
use std::fmt;
use std::sync::{Arc, OnceLock};

use serde::Deserialize;
use sha2::{Digest, Sha256};
use wasmtime::{Engine, InstancePre, Linker, Module};

use instance::State;
pub use instance::{Call, Instance, MAX_PENDING_PROMISES};
pub(crate) use meta::ScriptMeta;
pub use meta::TypeMeta;

use crate::value::{NativeObject, Repr, Value};

/// An error while a script runs, naming where: a 1-based line and column (in bytes) of a module.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ScriptError {
    pub module: String,
    pub line: i32,
    pub col: i32,
    pub msg: String,
}

impl fmt::Display for ScriptError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}:{}:{}: {}", self.module, self.line, self.col, self.msg)
    }
}

impl std::error::Error for ScriptError {}

/// What a script or the runtime fails with: the script's own error while it runs ([`ScriptError`]), or the host's
/// mistake (a module that can't be loaded, a method that isn't there, an argument of the wrong kind).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Error {
    Script(ScriptError),
    Host(String),
}

impl Error {
    pub(crate) fn host(msg: impl Into<String>) -> Error {
        Error::Host(msg.into())
    }

    /// The script's error, when it is one.
    pub fn as_script(&self) -> Option<&ScriptError> {
        match self {
            Error::Script(e) => Some(e),
            Error::Host(_) => None,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Script(e) => e.fmt(f),
            Error::Host(msg) => f.write_str(msg),
        }
    }
}

impl std::error::Error for Error {}

/// What a native fails with: any error. A [`ScriptError`] (or an [`Error`] holding one) fails the script's call
/// as it is; any other fails it at the place it called from, as "member: error".
pub type NativeError = Box<dyn std::error::Error + Send + Sync>;

/// What a native gives: a value of its declared type (anything, the default value say, for void).
pub type NativeResult = Result<Value, NativeError>;

pub(crate) type NativeFunc = Arc<dyn Fn(&mut Call<'_, '_>, &Value, &[Value]) -> NativeResult + Send + Sync>;

/// A module the host offers scripts, imported by its name (such as `"game"`), with the natives that are its code:
/// `"f"` for a native function, `"C.m"` for a native class C's method m, and `"C.f"` for its readonly field f. Its
/// source is its definition file, which must be the one the script was compiled against.
#[derive(Clone)]
pub struct HostModule {
    name: Arc<str>,
    source: String,
    natives: HashMap<String, NativeFunc>,
}

impl fmt::Debug for HostModule {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "HostModule({})", self.name)
    }
}

impl HostModule {
    /// A host module named name, with its definition file.
    pub fn new(name: &str, source: impl Into<String>) -> HostModule {
        HostModule {
            name: name.into(),
            source: source.into(),
            natives: HashMap::new(),
        }
    }

    pub fn name(&self) -> &str {
        &self.name
    }

    /// Gives the code of a native: this is the object (for methods and fields), and args already have the declared
    /// types. A field gets no args.
    pub fn native(
        mut self,
        name: &str,
        f: impl Fn(&mut Call<'_, '_>, &Value, &[Value]) -> NativeResult + Send + Sync + 'static,
    ) -> HostModule {
        self.natives.insert(name.into(), Arc::new(f));
        self
    }

    /// An object of the module's native class with the host's payload, for passing to scripts.
    pub fn object(&self, class: &str, payload: impl Any + Send + Sync) -> Value {
        Value(Repr::Native(NativeObject::new(
            self.name.clone(),
            class,
            Box::new(payload),
        )))
    }

    /// What a module records of the definition file: the first 8 bytes of its SHA-256, in hex.
    fn hash(&self) -> String {
        let sum = Sha256::digest(self.source.as_bytes());
        sum[..8].iter().map(|b| format!("{b:02x}")).collect()
    }
}

/// An interface a script's class may satisfy, as Go's `Interface.Meta` describes it (Go's `HostModule.Interfaces`
/// gives a host module's, as JSON).
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Interface {
    pub name: String,
    pub members: Vec<InterfaceMember>,
}

/// A method or field of an interface: canon is its type as the satisfies check compares it, text as messages
/// write it.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct InterfaceMember {
    pub name: String,
    #[serde(default)]
    pub method: bool,
    #[serde(default)]
    pub optional: bool,
    #[serde(default)]
    pub readonly: bool,
    pub canon: String,
    pub text: String,
}

impl Interface {
    /// Reads interfaces by name, as `HostModule.Interfaces` writes them.
    pub fn parse_all(json: &str) -> Result<HashMap<String, Interface>, Error> {
        serde_json::from_str(json).map_err(|e| Error::host(format!("broken interfaces: {e}")))
    }
}

/// asyncForVoid: whether a method of signature have may stand for an optional method of signature want that
/// returns void, by returning a promise of void.
fn async_for_void(have: &str, want: &str) -> bool {
    let Some(params) = want.strip_suffix(": void") else {
        return false;
    };
    match have.strip_prefix(&format!("{params}: Promise<void")) {
        Some(rest) => rest == ">" || rest.starts_with(", "),
        None => false,
    }
}

fn engine() -> &'static Engine {
    static ENGINE: OnceLock<Engine> = OnceLock::new();
    ENGINE.get_or_init(Engine::default)
}

fn linker() -> &'static Linker<State> {
    static LINKER: OnceLock<Linker<State>> = OnceLock::new();
    LINKER.get_or_init(|| instance::imports(engine()))
}

pub(crate) struct ScriptInner {
    pub(crate) meta: ScriptMeta,
    pub(crate) natives: Vec<NativeFunc>,
    /// The member's own name, for errors
    pub(crate) native_names: Vec<String>,
    /// The native classes' indices by "module:name"
    pub(crate) native_classes: HashMap<String, usize>,
    pre: InstancePre<State>,
}

/// A compiled script, whose module the host instantiates for each [`Instance`]. It is cheap to clone and safe to
/// share between threads; its machine code is made when it is loaded.
#[derive(Clone)]
pub struct Script(pub(crate) Arc<ScriptInner>);

impl fmt::Debug for Script {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Script({})", self.0.meta.entry)
    }
}

/// Loads a module Kiln's compiler made (Go's `Script.Module`) against the host modules it was compiled with, and
/// compiles it to machine code. It refuses one made for another version of the runtime, one that imports a host
/// module that isn't given or was compiled against another definition file of one, and one whose natives the host
/// doesn't give.
pub fn load_script(module: &[u8], hosts: &[&HostModule]) -> Result<Script, Error> {
    let raw = meta::custom_section(module, meta::META_SECTION).ok_or_else(|| {
        Error::host(format!(
            "not a compiled script: it has no {} section",
            meta::META_SECTION
        ))
    })?;
    let meta: ScriptMeta =
        serde_json::from_slice(raw).map_err(|e| Error::host(format!("a compiled script with broken metadata: {e}")))?;
    if meta.abi != meta::ABI_VERSION {
        return Err(Error::host(format!(
            "the script was compiled for runtime version {}, not {}: compile it again",
            meta.abi,
            meta::ABI_VERSION
        )));
    }
    let by_name: HashMap<&str, &HostModule> = hosts.iter().map(|h| (h.name(), *h)).collect();
    for hm in &meta.hosts {
        let Some(h) = by_name.get(hm.name.as_str()) else {
            return Err(Error::host(format!(
                "the script imports the host module {}, which this host doesn't offer",
                crate::gofmt::quote(&hm.name)
            )));
        };
        if h.hash() != hm.hash {
            return Err(Error::host(format!(
                "the script was compiled against another version of the host module {}: compile it again",
                crate::gofmt::quote(&hm.name)
            )));
        }
    }
    let mut native_classes = HashMap::new();
    for (i, cm) in meta.classes.iter().enumerate() {
        if cm.native {
            if !by_name.contains_key(cm.module.as_str()) {
                return Err(Error::host(format!(
                    "no host module {} for the native class {}",
                    crate::gofmt::quote(&cm.module),
                    cm.name
                )));
            }
            native_classes.insert(format!("{}:{}", cm.module, cm.name), i);
        }
    }
    let mut natives = Vec::with_capacity(meta.natives.len());
    let mut native_names = Vec::with_capacity(meta.natives.len());
    for nm in &meta.natives {
        let Some(f) = by_name.get(nm.host.as_str()).and_then(|h| h.natives.get(&nm.name)) else {
            return Err(Error::host(format!(
                "the host gives no code for the native {} of {}",
                nm.name,
                crate::gofmt::quote(&nm.host)
            )));
        };
        natives.push(f.clone());
        native_names.push(nm.name.rsplit('.').next().unwrap_or(&nm.name).to_string());
    }
    let compiled =
        Module::new(engine(), module).map_err(|e| Error::host(format!("the module doesn't compile: {e}")))?;
    let pre = linker()
        .instantiate_pre(&compiled)
        .map_err(|e| Error::host(format!("the module doesn't link: {e}")))?;
    Ok(Script(Arc::new(ScriptInner {
        meta,
        natives,
        native_names,
        native_classes,
        pre,
    })))
}

/// A class a script exports.
#[derive(Clone)]
pub struct Class {
    script: Script,
    idx: usize,
}

impl fmt::Debug for Class {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Class({})", self.name())
    }
}

impl Class {
    pub fn name(&self) -> &str {
        &self.script.0.meta.classes[self.idx].name
    }

    /// How many arguments its constructor takes.
    pub fn constructor_params(&self) -> usize {
        self.script.0.meta.classes[self.idx].ctor.len()
    }

    /// Whether the class satisfies i, by the checker's rules: every method of i it has, or must have (one that
    /// isn't optional), public with the same signature (an async one returning a promise of void may stand for an
    /// optional void one), and every field public, of the same type, and not readonly when i's isn't. Err says
    /// why it doesn't.
    pub fn satisfies(&self, i: &Interface) -> Result<(), String> {
        let cm = &self.script.0.meta.classes[self.idx];
        for m in &i.members {
            let method = cm.methods.get(&m.name);
            let field = cm.fields.iter().find(|f| f.name == m.name);
            if m.method {
                let Some(cmm) = method else {
                    if field.is_some() {
                        return Err(format!("{} is a field, not a method", m.name));
                    }
                    if m.optional {
                        continue;
                    }
                    return Err(format!("it lacks the method {}", m.name));
                };
                if cmm.private {
                    return Err(format!("{} is private", m.name));
                }
                if cmm.canon != m.canon && !(m.optional && async_for_void(&cmm.canon, &m.canon)) {
                    return Err(format!("{} is {}, not {}", m.name, cmm.sig, m.text));
                }
                continue;
            }
            let Some(f) = field.filter(|_| method.is_none()) else {
                return Err(format!("it lacks the field {}", m.name));
            };
            if f.private {
                return Err(format!("{} is private", m.name));
            }
            if f.canon != m.canon {
                return Err(format!("{} is {}, not {}", m.name, f.typ.name, m.text));
            }
            if f.readonly && !m.readonly {
                return Err(format!("{} is readonly", m.name));
            }
        }
        Ok(())
    }
}

impl Script {
    /// The script's modules: the entry and everything it imports, host modules aside.
    pub fn modules(&self) -> &[String] {
        &self.0.meta.modules
    }

    /// The entry module's ID.
    pub fn entry(&self) -> &str {
        &self.0.meta.entry
    }

    /// A class the entry module exports.
    pub fn class(&self, name: &str) -> Option<Class> {
        let idx = *self.0.meta.exports.get(name)?;
        Some(Class {
            script: self.clone(),
            idx,
        })
    }

    /// The class the entry module exports as default.
    pub fn default_class(&self) -> Option<Class> {
        self.class("default")
    }

    /// Makes an object of c with its constructor, which must take no arguments, on an instance of the script's
    /// module of its own. data is what the host's natives get ([`Call::data`]) whenever this instance calls them.
    pub fn new_instance(&self, c: &Class, data: impl Any + Send) -> Result<Instance, Error> {
        if !Arc::ptr_eq(&c.script.0, &self.0) {
            return Err(Error::host(format!("{} is a class of another script", c.name())));
        }
        if c.constructor_params() != 0 {
            return Err(Error::host(format!("{}'s constructor takes arguments", c.name())));
        }
        Instance::construct(self, c.idx, Box::new(data))
    }

    /// An instance of the script's module without an object of its own, for calling the functions the entry
    /// exports ([`Instance::call_function`]), and settling the promises they wait for.
    pub fn instantiate(&self, data: impl Any + Send) -> Result<Instance, Error> {
        Instance::functions(self, Box::new(data))
    }

    /// Calls a function the entry module exports, with data for the host's natives, on an instance of the
    /// script's module of its own.
    pub fn call(&self, name: &str, data: impl Any + Send, args: &[Value]) -> Result<Value, Error> {
        let fm = self
            .0
            .meta
            .funcs
            .get(name)
            .ok_or_else(|| Error::host(format!("{} exports no function {name}", self.0.meta.entry)))?;
        if args.len() != fm.params.len() {
            return Err(Error::host(format!("{name} takes {}", plural(fm.params.len()))));
        }
        self.instantiate(data)?.call_function(name, args)
    }
}

pub(crate) fn plural(n: usize) -> String {
    if n == 1 {
        "1 argument".into()
    } else {
        format!("{n} arguments")
    }
}
