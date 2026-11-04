//! The tokens of expressions (language/lexer.go, in its expression mode).

use super::Error;
use crate::gofmt::parse_number;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(super) enum TokenKind {
    Eof,
    Number,
    String,
    Ident,
    /// Operators and punctuation; the text says which
    Op,
}

#[derive(Clone, Debug)]
pub(super) struct Token {
    pub kind: TokenKind,
    /// The operator, identifier or decoded string
    pub text: String,
    pub num: f64,
    /// 0-based byte offset
    pub pos: usize,
}

/// Operators, longest first so "<=" wins over "<".
const OPERATORS: &[&str] = &[
    "&&", "||", "==", "!=", "<=", ">=", "+", "-", "*", "/", "%", "<", ">", "!", "?", ":", "(", ")", ",",
];

fn is_digit(c: u8) -> bool {
    c.is_ascii_digit()
}

fn is_ident_start(c: u8) -> bool {
    c == b'_' || c.is_ascii_alphabetic()
}

/// Whether src[i] is a _ between two digits, which numbers may have for readability: 1_000_000.
fn digit_separator(src: &[u8], i: usize) -> bool {
    src[i] == b'_' && i > 0 && is_digit(src[i - 1]) && i + 1 < src.len() && is_digit(src[i + 1])
}

/// Reads an expression's tokens. Punctuation only scripts use is refused with a message that says so.
pub(super) fn lex(src: &str) -> Result<Vec<Token>, Error> {
    let b = src.as_bytes();
    let mut toks = Vec::new();
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        if matches!(c, b'\n' | b' ' | b'\t' | b'\r') {
            i += 1;
        } else if is_digit(c) || (c == b'.' && i + 1 < b.len() && is_digit(b[i + 1])) {
            let start = i;
            while i < b.len() && (is_digit(b[i]) || b[i] == b'.' || digit_separator(b, i)) {
                i += 1;
            }
            if i < b.len() && (b[i] == b'e' || b[i] == b'E') {
                let mut j = i + 1;
                if j < b.len() && (b[j] == b'+' || b[j] == b'-') {
                    j += 1;
                }
                if j < b.len() && is_digit(b[j]) {
                    i = j;
                    while i < b.len() && (is_digit(b[i]) || digit_separator(b, i)) {
                        i += 1;
                    }
                }
            }
            let text = &src[start..i];
            let f = match parse_number(&text.replace('_', "")) {
                Some(f) if !f.is_infinite() => f,
                _ => return Err(Error::at(start, format!("bad number {}", crate::gofmt::quote(text)))),
            };
            if i < b.len() && b[i] == b'_' {
                return Err(Error::at(i, "a _ in a number goes between two digits, as in 1_000_000"));
            }
            if i < b.len() && is_ident_start(b[i]) {
                return Err(Error::at(
                    start,
                    format!("bad number {}", crate::gofmt::quote(&src[start..=i])),
                ));
            }
            toks.push(Token {
                kind: TokenKind::Number,
                text: String::new(),
                num: f,
                pos: start,
            });
        } else if c == b'"' {
            let start = i;
            let (text, next) = lex_string(src, i)?;
            i = next;
            toks.push(Token {
                kind: TokenKind::String,
                text,
                num: 0.0,
                pos: start,
            });
        } else if is_ident_start(c) {
            let start = i;
            while i < b.len() && (is_ident_start(b[i]) || is_digit(b[i])) {
                i += 1;
            }
            toks.push(Token {
                kind: TokenKind::Ident,
                text: src[start..i].into(),
                num: 0.0,
                pos: start,
            });
        } else {
            let Some(op) = OPERATORS.iter().find(|o| b[i..].starts_with(o.as_bytes())) else {
                let ch = src[i..].chars().next().unwrap_or('\u{fffd}');
                if ".[]{};='".contains(ch) {
                    return Err(Error::at(
                        i,
                        format!("{} is not allowed in an expression", quote_char(ch)),
                    ));
                }
                return Err(Error::at(i, format!("unexpected {}", quote_byte(c))));
            };
            toks.push(Token {
                kind: TokenKind::Op,
                text: (*op).into(),
                num: 0.0,
                pos: i,
            });
            i += op.len();
        }
    }
    toks.push(Token {
        kind: TokenKind::Eof,
        text: String::new(),
        num: 0.0,
        pos: b.len(),
    });
    Ok(toks)
}

fn quote_char(c: char) -> String {
    crate::gofmt::quote(&c.to_string())
}

/// Go's %q of string(src[i]): a byte past ASCII is quoted as the rune it is (Latin-1).
fn quote_byte(c: u8) -> String {
    quote_char(char::from(c))
}

/// Reads the string literal starting at src[i] and returns its text and the offset after it. Expressions allow
/// only \" and \\.
fn lex_string(src: &str, start: usize) -> Result<(String, usize), Error> {
    let b = src.as_bytes();
    let mut out: Vec<u8> = Vec::new();
    let mut i = start + 1;
    loop {
        if i >= b.len() {
            return Err(Error::at(start, "unterminated string"));
        }
        if b[i] == b'"' {
            // the bytes between quotes are whole characters of src, and the escapes ASCII
            return Ok((String::from_utf8(out).unwrap_or_default(), i + 1));
        }
        if b[i] == b'\\' {
            if i + 1 >= b.len() {
                return Err(Error::at(start, "unterminated string"));
            }
            let e = b[i + 1];
            if e != b'"' && e != b'\\' {
                return Err(Error::at(i, r#"only \" and \\ escapes are allowed"#));
            }
            out.push(e);
            i += 2;
            continue;
        }
        out.push(b[i]);
        i += 1;
    }
}
