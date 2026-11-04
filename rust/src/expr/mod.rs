//! Expressions compute a value from named inputs (language/program.go). An expression is compiled once against
//! a [`Library`] of functions and a [`Scope`] of the names it may use, then evaluated any number of times against
//! an [`Env`]. Values are numbers, strings or bools, with strict typing: the only implicit conversion is string
//! concatenation with `+`. There is no assignment, looping, recursion, I/O or randomness, so a compiled
//! [`Program`] is deterministic and safe to share between threads.

mod lexer;
mod library;
mod parser;

use std::collections::{BTreeSet, HashMap, HashSet};
use std::fmt;
use std::sync::Arc;

pub use library::{Conversion, Func, Library, convert};

use crate::value::{Kind, Repr, Value};

/// A compile error: the expression can't be read, or uses a name or function it may not. `pos` is the 1-based
/// column it starts at.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Error {
    pub pos: usize,
    pub msg: String,
}

impl Error {
    pub(crate) fn at(pos: usize, msg: impl Into<String>) -> Error {
        Error {
            pos: pos + 1,
            msg: msg.into(),
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "column {}: {}", self.pos, self.msg)
    }
}

impl std::error::Error for Error {}

/// An error while evaluating: division by zero, a name missing from the [`Env`], a value of the wrong kind, or a
/// function that failed. `pos` is the 1-based column.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EvalError {
    pub pos: usize,
    pub msg: String,
}

fn eval_err(pos: usize, msg: impl Into<String>) -> EvalError {
    EvalError {
        pos: pos + 1,
        msg: msg.into(),
    }
}

impl fmt::Display for EvalError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "column {}: {}", self.pos, self.msg)
    }
}

impl std::error::Error for EvalError {}

/// What [`compile`] accepts, so content can't make expressions that are slow to compile or run.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Limits {
    /// Bytes of source
    pub max_length: usize,
    /// Nesting of operators, calls and parentheses
    pub max_depth: usize,
    /// Literals, names, operators and calls; evaluation visits each at most once
    pub max_nodes: usize,
    /// Bounds every string an evaluation makes or reads (0: no limit), so joining strings can't use up memory; a
    /// longer one is an evaluation error
    pub max_string: usize,
}

impl Default for Limits {
    /// Limits that suit expressions written by hand in content.
    fn default() -> Limits {
        Limits {
            max_length: 1024,
            max_depth: 32,
            max_nodes: 256,
            max_string: 4096,
        }
    }
}

/// The names a host lets its expressions use. [`compile`] refuses any other name, so a typo is caught when the
/// expression is saved, not when it runs.
#[derive(Clone, Debug, Default)]
pub struct Scope {
    names: Arc<HashSet<String>>,
}

impl Scope {
    /// Declares names.
    pub fn new<S: AsRef<str>>(names: impl IntoIterator<Item = S>) -> Scope {
        Scope::default().with(names)
    }

    /// A copy of the scope with more names.
    pub fn with<S: AsRef<str>>(&self, names: impl IntoIterator<Item = S>) -> Scope {
        let mut next = (*self.names).clone();
        next.extend(names.into_iter().map(|n| n.as_ref().to_string()));
        Scope { names: Arc::new(next) }
    }

    pub fn has(&self, name: &str) -> bool {
        self.names.contains(name)
    }

    /// The names, sorted.
    pub fn names(&self) -> Vec<String> {
        let mut out: Vec<String> = self.names.iter().cloned().collect();
        out.sort();
        out
    }
}

/// The values of the names an expression uses, when it is evaluated.
pub trait Env {
    fn lookup(&self, name: &str) -> Option<Value>;
}

impl Env for HashMap<String, Value> {
    fn lookup(&self, name: &str) -> Option<Value> {
        self.get(name).cloned()
    }
}

/// An [`Env`] that asks a function.
pub struct FnEnv<F>(pub F);

impl<F: Fn(&str) -> Option<Value>> Env for FnEnv<F> {
    fn lookup(&self, name: &str) -> Option<Value> {
        (self.0)(name)
    }
}

struct NoEnv;

impl Env for NoEnv {
    fn lookup(&self, _: &str) -> Option<Value> {
        None
    }
}

#[derive(Clone)]
pub(crate) enum Node {
    Literal(Value, usize),
    Name(String, usize),
    /// ! (true) or - (false)
    Unary(bool, Box<Node>, usize),
    Binary(String, Box<Node>, Box<Node>, usize),
    /// && (true) or || (false)
    Logical(bool, Box<Node>, Box<Node>, usize),
    Cond(Box<Node>, Box<Node>, Box<Node>, usize),
    Call(String, Option<Func>, Vec<Node>, usize),
}

impl Node {
    fn pos(&self) -> usize {
        match self {
            Node::Literal(_, p)
            | Node::Name(_, p)
            | Node::Unary(_, _, p)
            | Node::Binary(_, _, _, p)
            | Node::Logical(_, _, _, p)
            | Node::Cond(_, _, _, p)
            | Node::Call(_, _, _, p) => *p,
        }
    }
}

/// A compiled expression. It is immutable and safe to evaluate from many threads at once.
#[derive(Clone)]
pub struct Program {
    src: String,
    root: Node,
    names: Vec<String>,
    max_string: usize,
}

impl fmt::Debug for Program {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Program({:?})", self.src)
    }
}

/// Reads src and checks it against lib and scope: every function must be in lib and called with an allowed
/// number of arguments, and every name must be in scope. Parts that use no names are computed once here.
pub fn compile(src: &str, lib: &Library, scope: &Scope, limits: &Limits) -> Result<Program, Error> {
    let root = parser::parse(src, limits)?;
    let mut used = BTreeSet::new();
    let root = bind(root, lib, scope, &mut used, limits.max_string)?;
    Ok(Program {
        src: src.into(),
        root,
        names: used.into_iter().collect(),
        max_string: limits.max_string,
    })
}

impl Program {
    /// Computes the expression's value with env's names.
    pub fn eval(&self, env: &dyn Env) -> Result<Value, EvalError> {
        eval(
            &self.root,
            &Ctx {
                env,
                max_string: self.max_string,
            },
        )
    }

    /// The names the expression uses, sorted, so a host knows which values it needs.
    pub fn names(&self) -> &[String] {
        &self.names
    }

    /// The text the program was compiled from.
    pub fn source(&self) -> &str {
        &self.src
    }
}

fn plural(n: usize) -> String {
    if n == 1 {
        "1 argument".into()
    } else {
        format!("{n} arguments")
    }
}

fn arity(f: &Func) -> String {
    match f.max_args {
        None => plural(f.min_args) + " or more",
        Some(max) if max == f.min_args => plural(max),
        Some(max) => plural(f.min_args) + " to " + &plural(max),
    }
}

/// Resolves names and functions, and folds parts without names into literals.
fn bind(n: Node, lib: &Library, scope: &Scope, used: &mut BTreeSet<String>, max_string: usize) -> Result<Node, Error> {
    let b = |n: Box<Node>, used: &mut BTreeSet<String>| bind(*n, lib, scope, used, max_string).map(Box::new);
    let n = match n {
        Node::Name(name, pos) => {
            if !scope.has(&name) {
                return Err(Error::at(pos, format!("unknown name {}", crate::gofmt::quote(&name))));
            }
            used.insert(name.clone());
            return Ok(Node::Name(name, pos));
        }
        Node::Unary(not, x, pos) => Node::Unary(not, b(x, used)?, pos),
        Node::Binary(op, l, r, pos) => {
            let l = b(l, used)?;
            Node::Binary(op, l, b(r, used)?, pos)
        }
        Node::Logical(and, l, r, pos) => {
            let l = b(l, used)?;
            Node::Logical(and, l, b(r, used)?, pos)
        }
        Node::Cond(c, x, y, pos) => {
            let c = b(c, used)?;
            let x = b(x, used)?;
            Node::Cond(c, x, b(y, used)?, pos)
        }
        Node::Call(name, _, args, pos) => {
            let Some(f) = lib.lookup(&name) else {
                return Err(Error::at(
                    pos,
                    format!("unknown function {}", crate::gofmt::quote(&name)),
                ));
            };
            if args.len() < f.min_args || f.max_args.is_some_and(|max| args.len() > max) {
                return Err(Error::at(pos, format!("{name} takes {}", arity(f))));
            }
            let f = f.clone();
            let args = args
                .into_iter()
                .map(|a| bind(a, lib, scope, used, max_string))
                .collect::<Result<_, _>>()?;
            Node::Call(name, Some(f), args, pos)
        }
        lit @ Node::Literal(..) => return Ok(lit),
    };
    Ok(fold(n, max_string))
}

/// Replaces a node whose children are all literals with its value. A part that fails (say, 1 / 0) stays, so it
/// fails when evaluated, like any other evaluation error.
fn fold(n: Node, max_string: usize) -> Node {
    let lit = |c: &Node| matches!(c, Node::Literal(..));
    let all = match &n {
        Node::Unary(_, x, _) => lit(x),
        Node::Binary(_, l, r, _) | Node::Logical(_, l, r, _) => lit(l) && lit(r),
        Node::Cond(c, a, b, _) => lit(c) && lit(a) && lit(b),
        Node::Call(_, _, args, _) => args.iter().all(lit),
        _ => false,
    };
    if !all {
        return n;
    }
    match eval(
        &n,
        &Ctx {
            env: &NoEnv,
            max_string,
        },
    ) {
        Ok(v) => Node::Literal(v, n.pos()),
        Err(_) => n,
    }
}

/// What a node evaluates against: the names' values and the string length limit.
struct Ctx<'a> {
    env: &'a dyn Env,
    max_string: usize,
}

impl Ctx<'_> {
    /// Refuses a string longer than the limit, so expressions that join strings (perhaps of other params, each
    /// joining the one before) can't grow them without bound.
    fn check_string(&self, pos: usize, v: Value) -> Result<Value, EvalError> {
        if let Some(s) = v.as_str()
            && self.max_string > 0
            && s.len() > self.max_string
        {
            return Err(eval_err(
                pos,
                format!("a string longer than {} characters", self.max_string),
            ));
        }
        Ok(v)
    }
}

fn eval(n: &Node, c: &Ctx<'_>) -> Result<Value, EvalError> {
    match n {
        Node::Literal(v, _) => Ok(v.clone()),
        Node::Name(name, pos) => match c.env.lookup(name) {
            Some(v) if v.is_valid() => c.check_string(*pos, v),
            _ => Err(eval_err(*pos, format!("{name} has no value"))),
        },
        Node::Unary(not, x, pos) => {
            let x = eval(x, c)?;
            if *not {
                return match x.as_bool() {
                    Some(b) => Ok(Value::bool(!b)),
                    None => Err(eval_err(*pos, format!("! wants a bool, got a {}", x.kind()))),
                };
            }
            match x.as_number() {
                Some(f) => Ok(Value::num(-f)),
                None => Err(eval_err(*pos, format!("- wants a number, got a {}", x.kind()))),
            }
        }
        Node::Binary(op, l, r, pos) => binary(op, eval(l, c)?, eval(r, c)?, *pos, c),
        Node::Logical(and, l, r, pos) => {
            let op = if *and { "&&" } else { "||" };
            let l = eval(l, c)?;
            let Some(a) = l.as_bool() else {
                return Err(eval_err(*pos, format!("{op} wants bools, got a {}", l.kind())));
            };
            if a != *and {
                return Ok(Value::bool(a));
            }
            let r = eval(r, c)?;
            match r.as_bool() {
                Some(b) => Ok(Value::bool(b)),
                None => Err(eval_err(*pos, format!("{op} wants bools, got a {}", r.kind()))),
            }
        }
        Node::Cond(cond, a, b, pos) => {
            let v = eval(cond, c)?;
            match v.as_bool() {
                Some(true) => eval(a, c),
                Some(false) => eval(b, c),
                None => Err(eval_err(*pos, format!("? wants a bool, got a {}", v.kind()))),
            }
        }
        Node::Call(name, f, args, pos) => {
            let f = f.as_ref().expect("a bound call");
            let args = args.iter().map(|a| eval(a, c)).collect::<Result<Vec<_>, _>>()?;
            let v = (f.call)(&args).map_err(|e| eval_err(*pos, format!("{name}: {e}")))?;
            if !v.is_valid() {
                return Err(eval_err(*pos, format!("{name} gave no value")));
            }
            if v.as_number().is_some_and(|f| !f.is_finite()) {
                return Err(eval_err(*pos, format!("{name} gives a number too large")));
            }
            c.check_string(*pos, v)
        }
    }
}

fn binary(op: &str, l: Value, r: Value, pos: usize, c: &Ctx<'_>) -> Result<Value, EvalError> {
    match op {
        "==" | "!=" => {
            if l.kind() != r.kind() {
                return Err(eval_err(
                    pos,
                    format!("{op} compares a {} with a {}", l.kind(), r.kind()),
                ));
            }
            let same = match (&l.0, &r.0) {
                (Repr::Number(a, _), Repr::Number(b, _)) => a == b,
                (Repr::String(a), Repr::String(b)) => a == b,
                (Repr::Bool(a), Repr::Bool(b)) => a == b,
                _ => false,
            };
            return Ok(Value::bool(same == (op == "==")));
        }
        "<" | "<=" | ">" | ">=" => {
            let ord = match (&l.0, &r.0) {
                (Repr::Number(a, _), Repr::Number(b, _)) => cmp(a, b),
                (Repr::String(a), Repr::String(b)) => cmp(a, b),
                _ => {
                    return Err(eval_err(
                        pos,
                        format!("{op} compares a {} with a {}", l.kind(), r.kind()),
                    ));
                }
            };
            return Ok(Value::bool(match op {
                "<" => ord < 0,
                "<=" => ord <= 0,
                ">" => ord > 0,
                _ => ord >= 0,
            }));
        }
        "+" if l.kind() == Kind::String || r.kind() == Kind::String => {
            let (ls, rs) = (l.to_string(), r.to_string());
            if c.max_string > 0 && ls.len() + rs.len() > c.max_string {
                return Err(eval_err(
                    pos,
                    format!("a string longer than {} characters", c.max_string),
                ));
            }
            return Ok(Value::str(ls + &rs));
        }
        _ => {}
    }
    let (Some(a), Some(b)) = (l.as_number(), r.as_number()) else {
        return Err(eval_err(
            pos,
            format!("{op} wants numbers, got a {} and a {}", l.kind(), r.kind()),
        ));
    };
    let f = match op {
        "+" => a + b,
        "-" => a - b,
        "*" => a * b,
        "/" | "%" if b == 0.0 => return Err(eval_err(pos, "division by zero")),
        "/" => a / b,
        _ => a % b,
    };
    if !f.is_finite() {
        return Err(eval_err(pos, format!("{op} gives a number too large")));
    }
    Ok(Value::num(f))
}

/// Go's cmp of ordered values: NaN compares as neither less nor more.
fn cmp<T: PartialOrd>(a: &T, b: &T) -> i32 {
    if a < b {
        -1
    } else if a > b {
        1
    } else {
        0
    }
}
