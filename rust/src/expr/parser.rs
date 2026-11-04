//! The grammar of expressions (language/parser.go), loosest binding first:
//!
//! ```text
//! cond    = or [ "?" cond ":" cond ]
//! or      = and { "||" and }
//! and     = eq { "&&" eq }
//! eq      = rel { ("==" | "!=") rel }
//! rel     = add { ("<" | "<=" | ">" | ">=") add }
//! add     = mul { ("+" | "-") mul }
//! mul     = unary { ("*" | "/" | "%") unary }
//! unary   = ("-" | "!") unary | primary
//! primary = number | string | "true" | "false" | name | name "(" [ cond { "," cond } ] ")" | "(" cond ")"
//! ```

use super::lexer::{Token, TokenKind, lex};
use super::{Error, Limits, Node};
use crate::value::Value;

struct Parser<'a> {
    toks: Vec<Token>,
    at: usize,
    depth: usize,
    nodes: usize,
    limits: &'a Limits,
}

pub(super) fn parse(src: &str, limits: &Limits) -> Result<Node, Error> {
    if src.len() > limits.max_length {
        return Err(Error::at(
            limits.max_length,
            format!("longer than {} characters", limits.max_length),
        ));
    }
    let toks = lex(src)?;
    let mut p = Parser {
        toks,
        at: 0,
        depth: 0,
        nodes: 0,
        limits,
    };
    let n = p.cond()?;
    let t = p.peek();
    if t.kind != TokenKind::Eof {
        return Err(Error::at(t.pos, format!("unexpected {}", describe(t))));
    }
    Ok(n)
}

fn describe(t: &Token) -> String {
    match t.kind {
        TokenKind::Eof => "the end".into(),
        TokenKind::Number => "a number".into(),
        TokenKind::String => "a string".into(),
        TokenKind::Ident | TokenKind::Op => format!("\"{}\"", t.text),
    }
}

impl Parser<'_> {
    fn peek(&self) -> &Token {
        &self.toks[self.at]
    }

    fn next(&mut self) -> Token {
        let t = self.toks[self.at].clone();
        if t.kind != TokenKind::Eof {
            self.at += 1;
        }
        t
    }

    fn is_op(&self, ops: &[&str]) -> bool {
        let t = self.peek();
        t.kind == TokenKind::Op && ops.contains(&t.text.as_str())
    }

    /// Tracks a new node against MaxNodes.
    fn count(&mut self, pos: usize) -> Result<(), Error> {
        self.nodes += 1;
        if self.nodes > self.limits.max_nodes {
            return Err(Error::at(pos, format!("more than {} parts", self.limits.max_nodes)));
        }
        Ok(())
    }

    /// Tracks nesting against MaxDepth; the caller leaves (depth -= 1) afterwards.
    fn enter(&mut self, pos: usize) -> Result<(), Error> {
        self.depth += 1;
        if self.depth > self.limits.max_depth {
            return Err(Error::at(pos, format!("nested deeper than {}", self.limits.max_depth)));
        }
        Ok(())
    }

    fn nested<T>(&mut self, pos: usize, f: impl FnOnce(&mut Self) -> Result<T, Error>) -> Result<T, Error> {
        let r = self.enter(pos).and_then(|()| f(self));
        self.depth -= 1;
        r
    }

    fn cond(&mut self) -> Result<Node, Error> {
        let pos = self.peek().pos;
        self.nested(pos, |p| {
            let c = p.or()?;
            if !p.is_op(&["?"]) {
                return Ok(c);
            }
            let q = p.next();
            let a = p.cond()?;
            if !p.is_op(&[":"]) {
                let t = p.peek();
                return Err(Error::at(t.pos, format!("expected : but found {}", describe(t))));
            }
            p.next();
            let b = p.cond()?;
            p.count(q.pos)?;
            Ok(Node::Cond(Box::new(c), Box::new(a), Box::new(b), q.pos))
        })
    }

    /// Parses one left-associative level of binary operators over the next tighter level.
    fn binary_level(&mut self, next: fn(&mut Self) -> Result<Node, Error>, ops: &[&str]) -> Result<Node, Error> {
        let mut l = next(self)?;
        while self.is_op(ops) {
            let op = self.next();
            let r = next(self)?;
            self.count(op.pos)?;
            l = match op.text.as_str() {
                "&&" | "||" => Node::Logical(op.text == "&&", Box::new(l), Box::new(r), op.pos),
                _ => Node::Binary(op.text, Box::new(l), Box::new(r), op.pos),
            };
        }
        Ok(l)
    }

    fn or(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::and, &["||"])
    }

    fn and(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::eq, &["&&"])
    }

    fn eq(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::rel, &["==", "!="])
    }

    fn rel(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::add, &["<", "<=", ">", ">="])
    }

    fn add(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::mul, &["+", "-"])
    }

    fn mul(&mut self) -> Result<Node, Error> {
        self.binary_level(Self::unary, &["*", "/", "%"])
    }

    fn unary(&mut self) -> Result<Node, Error> {
        if !self.is_op(&["-", "!"]) {
            return self.primary();
        }
        let op = self.next();
        self.nested(op.pos, |p| {
            let x = p.unary()?;
            p.count(op.pos)?;
            Ok(Node::Unary(op.text == "!", Box::new(x), op.pos))
        })
    }

    fn primary(&mut self) -> Result<Node, Error> {
        let t = self.next();
        match t.kind {
            TokenKind::Number => {
                self.count(t.pos)?;
                Ok(Node::Literal(Value::num(t.num), t.pos))
            }
            TokenKind::String => {
                self.count(t.pos)?;
                Ok(Node::Literal(Value::str(t.text), t.pos))
            }
            TokenKind::Ident => {
                if t.text == "true" || t.text == "false" {
                    self.count(t.pos)?;
                    return Ok(Node::Literal(Value::bool(t.text == "true"), t.pos));
                }
                if !self.is_op(&["("]) {
                    self.count(t.pos)?;
                    return Ok(Node::Name(t.text, t.pos));
                }
                self.call(t)
            }
            TokenKind::Op if t.text == "(" => self.nested(t.pos, |p| {
                let n = p.cond()?;
                if !p.is_op(&[")"]) {
                    let c = p.peek();
                    return Err(Error::at(c.pos, format!("expected ) but found {}", describe(c))));
                }
                p.next();
                Ok(n)
            }),
            _ => Err(Error::at(t.pos, format!("unexpected {}", describe(&t)))),
        }
    }

    fn call(&mut self, name: Token) -> Result<Node, Error> {
        self.next(); // (
        let args = self.nested(name.pos, |p| {
            let mut args = Vec::new();
            if !p.is_op(&[")"]) {
                loop {
                    args.push(p.cond()?);
                    if !p.is_op(&[","]) {
                        break;
                    }
                    p.next();
                }
            }
            if !p.is_op(&[")"]) {
                let t = p.peek();
                return Err(Error::at(t.pos, format!("expected , or ) but found {}", describe(t))));
            }
            p.next();
            Ok(args)
        })?;
        self.count(name.pos)?;
        Ok(Node::Call(name.text, None, args, name.pos))
    }
}
