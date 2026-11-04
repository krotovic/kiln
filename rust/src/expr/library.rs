//! Functions expressions call (language/library.go), and conversions between kinds (value.go).

use std::collections::HashMap;
use std::fmt;
use std::sync::Arc;

use crate::gofmt::{parse_number, quote};
use crate::value::{Repr, Value, go_space};

/// The code of a [`Func`].
pub type FuncCall = Arc<dyn Fn(&[Value]) -> Result<Value, String> + Send + Sync>;

/// A function expressions can call. It gets as many arguments as `min_args..=max_args` allow (`max_args` None:
/// any number) and must be pure and deterministic: the same arguments always give the same result, with no side
/// effects, so programs can be folded, cached and shared.
#[derive(Clone)]
pub struct Func {
    pub name: String,
    pub min_args: usize,
    pub max_args: Option<usize>,
    pub call: FuncCall,
}

impl Func {
    pub fn new(
        name: &str,
        min_args: usize,
        max_args: Option<usize>,
        call: impl Fn(&[Value]) -> Result<Value, String> + Send + Sync + 'static,
    ) -> Func {
        Func {
            name: name.into(),
            min_args,
            max_args,
            call: Arc::new(call),
        }
    }
}

impl fmt::Debug for Func {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Func({})", self.name)
    }
}

/// An immutable set of functions.
#[derive(Clone, Default, Debug)]
pub struct Library {
    funcs: Arc<HashMap<String, Func>>,
}

impl Library {
    /// A library of funcs.
    pub fn new(funcs: impl IntoIterator<Item = Func>) -> Library {
        Library::default().with(funcs)
    }

    /// A copy of the library with more functions, replacing any of the same name, so hosts extend [`Library::std`]
    /// without touching it.
    pub fn with(&self, funcs: impl IntoIterator<Item = Func>) -> Library {
        let mut next = (*self.funcs).clone();
        for f in funcs {
            next.insert(f.name.clone(), f);
        }
        Library { funcs: Arc::new(next) }
    }

    /// Finds a function by name.
    pub fn lookup(&self, name: &str) -> Option<&Func> {
        self.funcs.get(name)
    }

    /// The library's functions, sorted.
    pub fn names(&self) -> Vec<String> {
        let mut out: Vec<String> = self.funcs.keys().cloned().collect();
        out.sort();
        out
    }

    /// The standard library: min, max, floor, ceil, round, abs, clamp, num, int, str, bool, len and concat.
    pub fn std() -> Library {
        static STD: std::sync::OnceLock<Library> = std::sync::OnceLock::new();
        STD.get_or_init(|| {
            Library::new([
                numbers("min", 1, None, |xs| xs[1..].iter().fold(xs[0], |m, &x| go_min(m, x))),
                numbers("max", 1, None, |xs| xs[1..].iter().fold(xs[0], |m, &x| go_max(m, x))),
                numbers("floor", 1, Some(1), |xs| xs[0].floor()),
                numbers("ceil", 1, Some(1), |xs| xs[0].ceil()),
                numbers("round", 1, Some(1), |xs| xs[0].round()),
                numbers("abs", 1, Some(1), |xs| xs[0].abs()),
                numbers("clamp", 3, Some(3), |xs| go_max(xs[1], go_min(xs[2], xs[0]))),
                conversion("num", Conversion::ToNumber),
                conversion("int", Conversion::ToInteger),
                conversion("str", Conversion::ToString),
                conversion("bool", Conversion::ToBool),
                Func::new("len", 1, Some(1), |args| match args[0].as_str() {
                    Some(s) => Ok(Value::num(s.chars().count() as f64)),
                    None => Err(format!("len wants a string, got a {}", args[0].kind())),
                }),
                Func::new("concat", 1, None, |args| {
                    let mut out = String::new();
                    for a in args {
                        out.push_str(&a.to_string());
                    }
                    Ok(Value::str(out))
                }),
            ])
        })
        .clone()
    }
}

/// Go's math.Min: NaN wins, and -0 is less than 0.
fn go_min(x: f64, y: f64) -> f64 {
    if x.is_nan() || y.is_nan() {
        return f64::NAN;
    }
    if x == 0.0 && x == y {
        return if x.is_sign_negative() { x } else { y };
    }
    if x < y { x } else { y }
}

/// Go's math.Max: NaN wins, and 0 is more than -0.
fn go_max(x: f64, y: f64) -> f64 {
    if x.is_nan() || y.is_nan() {
        return f64::NAN;
    }
    if x == 0.0 && x == y {
        return if x.is_sign_negative() { y } else { x };
    }
    if x > y { x } else { y }
}

/// A function of number arguments that returns a number.
fn numbers(name: &'static str, min: usize, max: Option<usize>, f: fn(&[f64]) -> f64) -> Func {
    Func::new(name, min, max, move |args| {
        let mut xs = Vec::with_capacity(args.len());
        for a in args {
            match a.as_number() {
                Some(n) => xs.push(n),
                None => return Err(format!("{name} wants numbers, got a {}", a.kind())),
            }
        }
        Ok(Value::num(f(&xs)))
    })
}

fn conversion(name: &str, c: Conversion) -> Func {
    Func::new(name, 1, Some(1), move |args| convert(&args[0], c))
}

/// Turns a value into another kind, as num(), int(), str() and bool() do.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum Conversion {
    #[default]
    AsIs,
    ToNumber,
    ToInteger,
    ToString,
    ToBool,
}

impl Conversion {
    /// Reads a conversion's name: "" (as is), "number", "integer", "string" or "bool".
    pub fn parse(name: &str) -> Result<Conversion, String> {
        Ok(match name {
            "" => Conversion::AsIs,
            "number" => Conversion::ToNumber,
            "integer" => Conversion::ToInteger,
            "string" => Conversion::ToString,
            "bool" => Conversion::ToBool,
            _ => {
                return Err(format!(
                    "unknown type {} (want number, integer, string or bool)",
                    quote(name)
                ));
            }
        })
    }
}

/// Applies c to v. Numbers read from strings with surrounding spaces allowed; integers truncate toward zero;
/// numbers are true unless 0; only "true" and "false" read as bools.
pub fn convert(v: &Value, c: Conversion) -> Result<Value, String> {
    match c {
        Conversion::AsIs => Ok(v.clone()),
        Conversion::ToNumber | Conversion::ToInteger => {
            let f = match &v.0 {
                // a number made from it is written in its own form
                Repr::Number(f, _) => *f,
                Repr::Bool(b) => f64::from(u8::from(*b)),
                Repr::String(s) => match parse_number(s.trim_matches(go_space)) {
                    Some(f) if f.is_finite() => f,
                    _ => return Err(format!("{} is not a number", quote(s))),
                },
                _ => return Err("no value to convert".into()),
            };
            Ok(Value::num(if c == Conversion::ToInteger { f.trunc() } else { f }))
        }
        Conversion::ToString => {
            if !v.is_valid() {
                return Err("no value to convert".into());
            }
            Ok(Value::str(v.to_string()))
        }
        Conversion::ToBool => match &v.0 {
            Repr::Bool(_) => Ok(v.clone()),
            Repr::Number(f, _) => Ok(Value::bool(*f != 0.0)),
            Repr::String(s) => match &**s {
                "true" => Ok(Value::bool(true)),
                "false" => Ok(Value::bool(false)),
                _ => Err(format!("{} is not a bool", quote(s))),
            },
            _ => Err("no value to convert".into()),
        },
    }
}
