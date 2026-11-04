//! Values: what expressions compute, and what crosses between a script and its host.

use std::any::Any;
use std::collections::HashMap;
use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicI32, Ordering};

use crate::gofmt::format_number;
use crate::script::TypeMeta;

/// The kind of a [`Value`].
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Kind {
    /// No value at all: what a void function or native gives.
    Nothing,
    Number,
    String,
    Bool,
    /// Null, arrays, maps and objects are values of scripts only; expressions never see them.
    Null,
    Array,
    Map,
    Object,
}

impl fmt::Display for Kind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Kind::Nothing => "nothing",
            Kind::Number => "number",
            Kind::String => "string",
            Kind::Bool => "bool",
            Kind::Null => "null",
            Kind::Array => "array",
            Kind::Map => "map",
            Kind::Object => "object",
        })
    }
}

/// A number, a string or a bool, or in scripts null, an array, a map or an object (by reference). The default
/// value is none of them and only stands for "no value". Values are cheap to clone: strings, arrays and maps are
/// shared.
#[derive(Clone, Default)]
pub struct Value(pub(crate) Repr);

#[derive(Clone, Default)]
pub(crate) enum Repr {
    #[default]
    Nothing,
    /// A number, with the text it was written as when [`Value::literal`] read it
    Number(f64, Option<Arc<str>>),
    String(Arc<str>),
    Bool(bool),
    Null,
    Array(Arc<Vec<Value>>),
    Map(Arc<MapValue>),
    Native(NativeObject),
    Script(ScriptObject),
    Promise(ScriptPromise),
}

impl Value {
    /// A number.
    pub fn num(f: f64) -> Value {
        Value(Repr::Number(f, None))
    }

    /// A string.
    pub fn str(s: impl Into<Arc<str>>) -> Value {
        Value(Repr::String(s.into()))
    }

    /// A bool.
    pub fn bool(b: bool) -> Value {
        Value(Repr::Bool(b))
    }

    /// The null reference, for natives that answer no object.
    pub fn null() -> Value {
        Value(Repr::Null)
    }

    /// An array of items, for natives that return one.
    pub fn array(items: Vec<Value>) -> Value {
        Value(Repr::Array(Arc::new(items)))
    }

    /// A map of its entries in their order; a key given twice keeps its first place and its last value. Keys are
    /// numbers or strings.
    pub fn map(entries: impl IntoIterator<Item = (Value, Value)>) -> Value {
        let mut m = MapValue::default();
        for (k, v) in entries {
            m.insert(k, v);
        }
        Value(Repr::Map(Arc::new(m)))
    }

    /// A value written as plain text, as value lists in content are: a number when the text is one, `true` and
    /// `false` as bools, and a string otherwise. A number keeps its text, so `"01"` counts as 1 but is still
    /// written `01` (into a string, or joined to one).
    pub fn literal(s: &str) -> Value {
        if let Some(f) = crate::gofmt::parse_number(s.trim_matches(go_space))
            && f.is_finite()
        {
            return Value(Repr::Number(f, Some(s.into())));
        }
        match s {
            "true" => Value::bool(true),
            "false" => Value::bool(false),
            _ => Value::str(s),
        }
    }

    pub fn kind(&self) -> Kind {
        match &self.0 {
            Repr::Nothing => Kind::Nothing,
            Repr::Number(..) => Kind::Number,
            Repr::String(_) => Kind::String,
            Repr::Bool(_) => Kind::Bool,
            Repr::Null => Kind::Null,
            Repr::Array(_) => Kind::Array,
            Repr::Map(_) => Kind::Map,
            Repr::Native(_) | Repr::Script(_) | Repr::Promise(_) => Kind::Object,
        }
    }

    /// Whether it is a value at all (not the default "no value").
    pub fn is_valid(&self) -> bool {
        !matches!(self.0, Repr::Nothing)
    }

    pub fn as_number(&self) -> Option<f64> {
        match self.0 {
            Repr::Number(f, _) => Some(f),
            _ => None,
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match &self.0 {
            Repr::String(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self.0 {
            Repr::Bool(b) => Some(b),
            _ => None,
        }
    }

    /// An array's items (none for null and anything else).
    pub fn as_array(&self) -> Option<&[Value]> {
        match &self.0 {
            Repr::Array(items) => Some(items),
            _ => None,
        }
    }

    pub fn as_map(&self) -> Option<&MapValue> {
        match &self.0 {
            Repr::Map(m) => Some(m),
            _ => None,
        }
    }

    /// A host object, made by [`crate::HostModule::object`].
    pub fn as_native(&self) -> Option<&NativeObject> {
        match &self.0 {
            Repr::Native(o) => Some(o),
            _ => None,
        }
    }

    /// The payload of a host object, when it is one with a payload of type T.
    pub fn payload<T: Any>(&self) -> Option<&T> {
        self.as_native().and_then(|o| o.payload())
    }

    pub fn is_null(&self) -> bool {
        matches!(self.0, Repr::Null)
    }

    /// The name of an object's class: a host object's or a script object's.
    pub fn class_name(&self) -> Option<&str> {
        match &self.0 {
            Repr::Native(o) => Some(o.class()),
            Repr::Script(o) => Some(&o.0.class_name),
            _ => None,
        }
    }
}

/// The value's text form, as `str()` and string concatenation write it: numbers in the shortest form that reads
/// back the same ("3", "0.25") or, read by [`Value::literal`], as they were written ("01"); bools as "true" or
/// "false".
impl fmt::Display for Value {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.0 {
            Repr::Nothing => Ok(()),
            Repr::Number(_, Some(text)) => f.write_str(text),
            Repr::Number(n, None) => f.write_str(&format_number(*n)),
            Repr::String(s) => f.write_str(s),
            Repr::Bool(b) => write!(f, "{b}"),
            Repr::Null => f.write_str("null"),
            Repr::Array(_) => f.write_str("[array]"),
            Repr::Map(_) => f.write_str("[map]"),
            Repr::Native(o) => write!(f, "[{}]", o.class()),
            Repr::Script(o) => write!(f, "[{}]", o.0.class_name),
            Repr::Promise(_) => f.write_str("[Promise]"),
        }
    }
}

impl fmt::Debug for Value {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.0 {
            Repr::Nothing => f.write_str("nothing"),
            Repr::String(s) => write!(f, "{s:?}"),
            Repr::Array(items) => f.debug_list().entries(items.iter()).finish(),
            Repr::Map(m) => f.debug_map().entries(m.iter()).finish(),
            _ => write!(f, "{self}"),
        }
    }
}

/// Values are equal when they are of one kind and the same: numbers, strings and bools by what they are (not how
/// they were written), arrays and maps by their items, and objects by identity.
impl PartialEq for Value {
    fn eq(&self, other: &Value) -> bool {
        match (&self.0, &other.0) {
            (Repr::Nothing, Repr::Nothing) | (Repr::Null, Repr::Null) => true,
            (Repr::Number(a, _), Repr::Number(b, _)) => a == b,
            (Repr::String(a), Repr::String(b)) => a == b,
            (Repr::Bool(a), Repr::Bool(b)) => a == b,
            (Repr::Array(a), Repr::Array(b)) => a == b,
            (Repr::Map(a), Repr::Map(b)) => a.keys == b.keys && a.vals == b.vals,
            (Repr::Native(a), Repr::Native(b)) => a.same(b),
            (Repr::Script(a), Repr::Script(b)) => Arc::ptr_eq(&a.0, &b.0),
            (Repr::Promise(a), Repr::Promise(b)) => Arc::ptr_eq(&a.0.0, &b.0.0),
            _ => false,
        }
    }
}

impl From<f64> for Value {
    fn from(f: f64) -> Value {
        Value::num(f)
    }
}

impl From<&str> for Value {
    fn from(s: &str) -> Value {
        Value::str(s)
    }
}

impl From<String> for Value {
    fn from(s: String) -> Value {
        Value::str(s)
    }
}

impl From<bool> for Value {
    fn from(b: bool) -> Value {
        Value::bool(b)
    }
}

/// Go's unicode.IsSpace, which strings.TrimSpace trims: Unicode's White_Space.
pub(crate) fn go_space(c: char) -> bool {
    c.is_whitespace()
}

/// A map: its entries in insertion order, keyed by numbers or strings.
#[derive(Clone, Default)]
pub struct MapValue {
    keys: Vec<Value>,
    vals: Vec<Value>,
    index: HashMap<MapKey, usize>,
}

#[derive(Clone, PartialEq, Eq, Hash)]
enum MapKey {
    Number(u64),
    String(Arc<str>),
    Other(usize),
}

fn key_of(v: &Value) -> MapKey {
    match &v.0 {
        // -0 is the same key as 0, as in scripts
        Repr::Number(f, _) => MapKey::Number(if *f == 0.0 { 0 } else { f.to_bits() }),
        Repr::String(s) => MapKey::String(s.clone()),
        Repr::Bool(b) => MapKey::Other(*b as usize),
        _ => MapKey::Other(usize::MAX),
    }
}

impl MapValue {
    pub(crate) fn insert(&mut self, k: Value, v: Value) {
        let key = key_of(&k);
        if let Some(&i) = self.index.get(&key) {
            self.vals[i] = v;
            return;
        }
        self.index.insert(key, self.keys.len());
        self.keys.push(k);
        self.vals.push(v);
    }

    pub fn len(&self) -> usize {
        self.keys.len()
    }

    pub fn is_empty(&self) -> bool {
        self.keys.is_empty()
    }

    /// The value of key k.
    pub fn get(&self, k: &Value) -> Option<&Value> {
        self.index.get(&key_of(k)).map(|&i| &self.vals[i])
    }

    /// The entries in their order.
    pub fn iter(&self) -> impl Iterator<Item = (&Value, &Value)> {
        self.keys.iter().zip(self.vals.iter())
    }
}

/// An object of a host module's native class, with the host's payload. Clones are the same object, which keeps its
/// identity in scripts.
#[derive(Clone)]
pub struct NativeObject(pub(crate) Arc<NativeInner>);

pub(crate) struct NativeInner {
    pub(crate) host: Arc<str>,
    pub(crate) class: Arc<str>,
    payload: Box<dyn Any + Send + Sync>,
}

impl NativeObject {
    pub(crate) fn new(host: Arc<str>, class: &str, payload: Box<dyn Any + Send + Sync>) -> NativeObject {
        NativeObject(Arc::new(NativeInner {
            host,
            class: class.into(),
            payload,
        }))
    }

    /// The host module whose class it is.
    pub fn host(&self) -> &str {
        &self.0.host
    }

    pub fn class(&self) -> &str {
        &self.0.class
    }

    /// The host's payload, when it is a T.
    pub fn payload<T: Any>(&self) -> Option<&T> {
        self.0.payload.downcast_ref()
    }

    /// Whether the two are the same object.
    pub fn same(&self, other: &NativeObject) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }

    pub(crate) fn id(&self) -> usize {
        Arc::as_ptr(&self.0) as *const () as usize
    }
}

/// An object of a script the host holds, by its handle in its instance.
#[derive(Clone)]
pub(crate) struct ScriptObject(pub(crate) Arc<ScriptRef>);

pub(crate) struct ScriptRef {
    pub(crate) instance: u64,
    pub(crate) handle: AtomicI32,
    pub(crate) class: usize,
    pub(crate) class_name: Arc<str>,
}

impl ScriptObject {
    pub(crate) fn handle(&self) -> i32 {
        self.0.handle.load(Ordering::Relaxed)
    }
}

/// A promise a native made, as the script gets it.
#[derive(Clone)]
pub(crate) struct ScriptPromise(pub(crate) Resolver);

/// Settles a promise a native made ([`crate::Call::new_promise`]). Resolving and rejecting run, under fresh
/// budgets, what waited for it, so they are calls into the instance: they come between its other calls, never from
/// inside a native. A promise settles once; settling it again does nothing.
#[derive(Clone)]
pub struct Resolver(pub(crate) Arc<ResolverInner>);

pub(crate) struct ResolverInner {
    pub(crate) instance: u64,
    /// 0 once it settled
    pub(crate) handle: AtomicI32,
    pub(crate) value: TypeMeta,
    pub(crate) err: Option<TypeMeta>,
}

impl Resolver {
    pub(crate) fn handle(&self) -> i32 {
        self.0.handle.load(Ordering::Relaxed)
    }

    /// Whether the promise hasn't settled.
    pub fn pending(&self) -> bool {
        self.handle() != 0
    }
}
