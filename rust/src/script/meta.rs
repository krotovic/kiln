//! A compiled module's metadata (the `alo.script` section, abi.go's scriptMeta), and the interface between
//! modules and hosts.

use std::collections::HashMap;

use serde::{Deserialize, Deserializer};

use super::{Error, ScriptError};

pub(crate) const ABI_VERSION: i64 = 2;
pub(crate) const META_SECTION: &str = "alo.script";
pub(crate) const SCRATCH: u32 = 16;
pub(crate) const HEADER: u32 = 16;
pub(crate) const TAG_ARR_VALS: i32 = 2;
pub(crate) const TAG_ARR_REFS: i32 = 3;
pub(crate) const TAG_CLASS: i64 = 16;
pub(crate) const MAP_STR_KEYS: i32 = 1;
pub(crate) const MAP_REF_VALS: i32 = 2;
// The kinds of promises the runtime makes (gen_async.go)
pub(crate) const PROMISE_VALUE: i32 = 0;
pub(crate) const PROMISE_STRING: i32 = 1;
pub(crate) const PROMISE_REF: i32 = 2;

/// Go writes empty slices and maps as null.
fn nullable<'de, D: Deserializer<'de>, T: Default + Deserialize<'de>>(d: D) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

#[derive(Deserialize, Debug)]
pub(crate) struct ScriptMeta {
    pub abi: i64,
    pub entry: String,
    #[serde(default, deserialize_with = "nullable")]
    pub modules: Vec<String>,
    pub limits: LimitsMeta,
    #[serde(default, deserialize_with = "nullable")]
    pub messages: Vec<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub positions: Vec<i32>,
    #[serde(default, deserialize_with = "nullable")]
    pub classes: Vec<ClassMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub natives: Vec<NativeMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub hosts: Vec<HostMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub funcs: HashMap<String, FuncMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub exports: HashMap<String, usize>,
}

#[derive(Deserialize, Debug)]
pub(crate) struct LimitsMeta {
    #[serde(rename = "MaxHeap")]
    pub max_heap: i64,
}

#[derive(Deserialize, Debug)]
pub(crate) struct HostMeta {
    pub name: String,
    pub hash: String,
}

/// A type as the host sees it. K is n (number), s (string), b (bool), v (void), a (array of E), m (map of Key to
/// E), c (an object of class C), i (an object of an interface), or p (a promise of E, which fails with Err).
#[derive(Deserialize, Debug, Clone)]
pub struct TypeMeta {
    pub(crate) k: String,
    #[serde(default)]
    pub(crate) e: Option<Box<TypeMeta>>,
    #[serde(default)]
    pub(crate) key: Option<Box<TypeMeta>>,
    #[serde(default)]
    pub(crate) c: usize,
    #[serde(default)]
    pub(crate) err: Option<Box<TypeMeta>>,
    pub(crate) name: String,
}

impl TypeMeta {
    pub(crate) fn is(&self, k: &str) -> bool {
        self.k == k
    }

    pub(crate) fn is_ref(&self) -> bool {
        self.k != "n" && self.k != "b"
    }

    pub(crate) fn elem(&self) -> &TypeMeta {
        self.e
            .as_deref()
            .expect("an array, map or promise type has its elements' type")
    }

    pub(crate) fn key(&self) -> &TypeMeta {
        self.key.as_deref().expect("a map type has its keys' type")
    }
}

#[derive(Deserialize, Debug)]
pub(crate) struct ClassMeta {
    pub name: String,
    pub module: String,
    pub pos: i32,
    #[serde(default)]
    pub native: bool,
    #[serde(default, deserialize_with = "nullable")]
    pub fields: Vec<FieldMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub methods: HashMap<String, MethodMeta>,
    #[serde(default, deserialize_with = "nullable")]
    pub ctor: Vec<TypeMeta>,
}

#[derive(Deserialize, Debug)]
pub(crate) struct FieldMeta {
    pub name: String,
    #[serde(rename = "type")]
    pub typ: TypeMeta,
    pub canon: String,
    pub offset: u32,
    #[serde(default)]
    pub private: bool,
    #[serde(default)]
    pub readonly: bool,
}

#[derive(Deserialize, Debug)]
pub(crate) struct MethodMeta {
    #[serde(default, deserialize_with = "nullable")]
    pub params: Vec<TypeMeta>,
    pub result: TypeMeta,
    #[serde(default)]
    pub private: bool,
    /// The signature as messages write it
    pub sig: String,
    /// The signature as Satisfies compares it
    pub canon: String,
    /// The module export that runs it on an object of the class (none for a native class's)
    #[serde(default)]
    pub export: String,
}

#[derive(Deserialize, Debug)]
pub(crate) struct FuncMeta {
    #[serde(default, deserialize_with = "nullable")]
    pub params: Vec<TypeMeta>,
    pub result: TypeMeta,
    pub export: String,
}

/// A native the script calls: "f" for a function, "C.m" for a method and "C.f" for a field of a native class, of
/// the host module `host`.
#[derive(Deserialize, Debug)]
pub(crate) struct NativeMeta {
    pub host: String,
    pub name: String,
    #[serde(default, rename = "this")]
    pub this: bool,
    #[serde(default, deserialize_with = "nullable")]
    pub params: Vec<TypeMeta>,
    pub result: TypeMeta,
}

impl ScriptMeta {
    pub(crate) fn error_at(&self, pos: i32, msg: impl Into<String>) -> Error {
        let i = pos as i64 * 3;
        let msg = msg.into();
        if pos < 0 || i + 2 >= self.positions.len() as i64 {
            return Error::Script(ScriptError {
                module: self.entry.clone(),
                line: 1,
                col: 1,
                msg,
            });
        }
        let i = i as usize;
        let module = self
            .modules
            .get(self.positions[i] as usize)
            .cloned()
            .unwrap_or_default();
        Error::Script(ScriptError {
            module,
            line: self.positions[i + 1],
            col: self.positions[i + 2],
            msg,
        })
    }
}

/// The contents of a module's custom section, read from its binary.
pub(crate) fn custom_section<'a>(module: &'a [u8], name: &str) -> Option<&'a [u8]> {
    fn uleb(b: &[u8], at: &mut usize) -> Option<usize> {
        let (mut v, mut shift) = (0usize, 0);
        loop {
            let byte = *b.get(*at)?;
            *at += 1;
            v |= usize::from(byte & 0x7f).checked_shl(shift)?;
            if byte & 0x80 == 0 {
                return Some(v);
            }
            shift += 7;
            if shift > 35 {
                return None;
            }
        }
    }
    if module.len() < 8 || &module[..4] != b"\0asm" {
        return None;
    }
    let mut at = 8;
    while at < module.len() {
        let id = module[at];
        at += 1;
        let size = uleb(module, &mut at)?;
        let end = at.checked_add(size).filter(|&e| e <= module.len())?;
        if id == 0 {
            let mut p = at;
            let n = uleb(module, &mut p)?;
            if p + n <= end && &module[p..p + n] == name.as_bytes() {
                return Some(&module[p + n..end]);
            }
        }
        at = end;
    }
    None
}
