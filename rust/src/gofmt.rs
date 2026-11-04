//! What a script asks its host to do the way Go does it, so a script gives the same results here as on Go's
//! runtime and in the browser: numbers in their shortest text form (`strconv.FormatFloat(x, 'f', -1, 64)`),
//! numbers read from text (`strconv.ParseFloat`), and strings quoted for messages (`strconv.Quote`). The tests
//! check them against what Go gives.

use unicode_properties::{GeneralCategoryGroup, UnicodeGeneralCategory};

/// A number's text form, as `str()` and `+` write it: the shortest digits that read back the same, never an
/// exponent.
pub fn format_number(x: f64) -> String {
    if x.is_nan() {
        return "NaN".into();
    }
    if x.is_infinite() {
        return if x > 0.0 { "+Inf".into() } else { "-Inf".into() };
    }
    if x == 0.0 {
        return if x.is_sign_negative() { "-0".into() } else { "0".into() };
    }
    // Rust's shortest digits that read back the same. When two such digit strings are as close to x as each other
    // (x's exact value ends in a 5 just past them), Go takes the even one, as rounding x to that many digits does.
    let shortest = format!("{:e}", x.abs());
    let (mantissa, _) = shortest.split_once('e').unwrap_or((&shortest, "0"));
    let n = mantissa.bytes().filter(u8::is_ascii_digit).count();
    let rounded = format!("{:.*e}", n - 1, x.abs());
    let text = if rounded.parse::<f64>() == Ok(x.abs()) {
        rounded
    } else {
        shortest
    };
    let (mantissa, exp) = text.split_once('e').unwrap_or((&text, "0"));
    let mut digits: String = mantissa.chars().filter(char::is_ascii_digit).collect();
    while digits.len() > 1 && digits.ends_with('0') {
        digits.pop();
    }
    // the decimal point comes after this many digits
    let point = exp.parse::<i64>().unwrap_or(0) + 1;
    let mut out = String::new();
    if x < 0.0 {
        out.push('-');
    }
    if point <= 0 {
        out.push_str("0.");
        out.extend(std::iter::repeat_n('0', (-point) as usize));
        out.push_str(&digits);
    } else if point as usize >= digits.len() {
        out.push_str(&digits);
        out.extend(std::iter::repeat_n('0', point as usize - digits.len()));
    } else {
        out.push_str(&digits[..point as usize]);
        out.push('.');
        out.push_str(&digits[point as usize..]);
    }
    out
}

fn is_hex(c: u8) -> bool {
    c.is_ascii_hexdigit()
}

/// Go's underscoreOK: underscores only between digits, or between a base prefix and a digit.
fn underscore_ok(s: &[u8]) -> bool {
    let mut saw = b'^';
    let mut i = 0;
    let s = match s.first() {
        Some(b'+' | b'-') => &s[1..],
        _ => s,
    };
    let mut hex = false;
    if s.len() >= 2 && s[0] == b'0' && matches!(s[1].to_ascii_lowercase(), b'b' | b'o' | b'x') {
        i = 2;
        saw = b'0';
        hex = s[1].eq_ignore_ascii_case(&b'x');
    }
    while i < s.len() {
        let c = s[i];
        if c.is_ascii_digit() || (hex && is_hex(c)) {
            saw = b'0';
        } else if c == b'_' {
            if saw != b'0' {
                return false;
            }
            saw = b'_';
        } else {
            if saw == b'_' {
                return false;
            }
            saw = b'!';
        }
        i += 1;
    }
    saw != b'_'
}

/// A number read as Go's `strconv.ParseFloat` reads it, or None when Go refuses it. "inf", "infinity" and "nan"
/// read as themselves, and numbers too large as infinities (Go refuses those): callers want finite numbers, so
/// they refuse both.
pub fn parse_number(s: &str) -> Option<f64> {
    let b = s.as_bytes();
    if b.is_empty() {
        return None;
    }
    // inf and infinity with a sign, nan without
    let (sign, rest) = match b[0] {
        b'+' => (1.0, &s[1..]),
        b'-' => (-1.0, &s[1..]),
        _ => (1.0, s),
    };
    if rest.eq_ignore_ascii_case("inf") || rest.eq_ignore_ascii_case("infinity") {
        return Some(sign * f64::INFINITY);
    }
    if s.eq_ignore_ascii_case("nan") {
        return Some(f64::NAN);
    }

    let mut i = 0;
    let mut neg = false;
    if b[i] == b'+' || b[i] == b'-' {
        neg = b[i] == b'-';
        i += 1;
    }
    let mut hex = false;
    if i + 2 < b.len() && b[i] == b'0' && b[i + 1].eq_ignore_ascii_case(&b'x') {
        hex = true;
        i += 2;
    }
    let base: u64 = if hex { 16 } else { 10 };
    let max_mant_digits = if hex { 16 } else { 19 };
    let (mut underscores, mut saw_dot, mut saw_digits) = (false, false, false);
    let (mut mantissa, mut trunc) = (0u64, false);
    let (mut nd, mut nd_mant, mut dp) = (0i64, 0i64, 0i64);
    while i < b.len() {
        let c = b[i];
        if c == b'_' {
            underscores = true;
        } else if c == b'.' {
            if saw_dot {
                break;
            }
            saw_dot = true;
            dp = nd;
        } else if c.is_ascii_digit() {
            saw_digits = true;
            if c == b'0' && nd == 0 {
                // leading zeros
                dp -= 1;
            } else {
                nd += 1;
                if nd_mant < max_mant_digits {
                    mantissa = mantissa * base + u64::from(c - b'0');
                    nd_mant += 1;
                } else if c != b'0' {
                    trunc = true;
                }
            }
        } else if hex && is_hex(c) {
            saw_digits = true;
            nd += 1;
            if nd_mant < max_mant_digits {
                mantissa = mantissa * 16 + u64::from(c.to_ascii_lowercase() - b'a' + 10);
                nd_mant += 1;
            } else {
                trunc = true;
            }
        } else {
            break;
        }
        i += 1;
    }
    if !saw_digits {
        return None;
    }
    if !saw_dot {
        dp = nd;
    }
    if hex {
        dp *= 4;
        nd_mant *= 4;
    }
    let exp_char = if hex { b'p' } else { b'e' };
    if i < b.len() && b[i].to_ascii_lowercase() == exp_char {
        i += 1;
        if i >= b.len() {
            return None;
        }
        let mut esign = 1;
        if b[i] == b'+' {
            i += 1;
        } else if b[i] == b'-' {
            i += 1;
            esign = -1;
        }
        if i >= b.len() || !b[i].is_ascii_digit() {
            return None;
        }
        let mut e = 0i64;
        while i < b.len() && (b[i].is_ascii_digit() || b[i] == b'_') {
            if b[i] == b'_' {
                underscores = true;
            } else if e < 10000 {
                e = e * 10 + i64::from(b[i] - b'0');
            }
            i += 1;
        }
        dp += e * esign;
    } else if hex {
        return None; // a hexadecimal mantissa needs a p exponent
    }
    if i != b.len() {
        return None;
    }
    if underscores && !underscore_ok(b) {
        return None;
    }
    if hex {
        let exp = if mantissa != 0 { dp - nd_mant } else { 0 };
        return Some(atof_hex(mantissa, exp, neg, trunc));
    }
    // A decimal number Go reads is one Rust reads the same way, correctly rounded, once its underscores are gone
    let text: String = s.chars().filter(|&c| c != '_').collect();
    text.parse::<f64>().ok()
}

/// Go's atofHex: mantissa × 2^exp, rounded to the nearest float64 (ties to even), denormal or infinite as it
/// must be. trunc says digits past the mantissa weren't all zero.
fn atof_hex(mut mantissa: u64, mut exp: i64, neg: bool, trunc: bool) -> f64 {
    const MANTBITS: u32 = 52;
    const EXPBITS: u32 = 11;
    const BIAS: i64 = -1023;
    let max_exp = (1i64 << EXPBITS) + BIAS - 2;
    let min_exp = BIAS + 1;
    exp += i64::from(MANTBITS);
    while mantissa != 0 && mantissa >> (MANTBITS + 2) == 0 {
        mantissa <<= 1;
        exp -= 1;
    }
    if trunc {
        mantissa |= 1;
    }
    while mantissa >> (1 + MANTBITS + 2) != 0 {
        mantissa = (mantissa >> 1) | (mantissa & 1);
        exp += 1;
    }
    while mantissa > 1 && exp < min_exp - 2 {
        mantissa = (mantissa >> 1) | (mantissa & 1);
        exp += 1;
    }
    let mut round = mantissa & 3;
    mantissa >>= 2;
    round |= mantissa & 1;
    exp += 2;
    if round == 3 {
        mantissa += 1;
        if mantissa == 1 << (1 + MANTBITS) {
            mantissa >>= 1;
            exp += 1;
        }
    }
    if mantissa >> MANTBITS == 0 {
        exp = BIAS;
    }
    if exp > max_exp {
        mantissa = 1 << MANTBITS;
        exp = max_exp + 1;
    }
    let mut bits = mantissa & ((1 << MANTBITS) - 1);
    bits |= (((exp - BIAS) as u64) & ((1 << EXPBITS) - 1)) << MANTBITS;
    if neg {
        bits |= 1 << 63;
    }
    f64::from_bits(bits)
}

/// Go's unicode.IsPrint: letters, marks, numbers, punctuation, symbols and the ASCII space.
fn printable(c: char) -> bool {
    c == ' '
        || matches!(
            c.general_category_group(),
            GeneralCategoryGroup::Letter
                | GeneralCategoryGroup::Mark
                | GeneralCategoryGroup::Number
                | GeneralCategoryGroup::Punctuation
                | GeneralCategoryGroup::Symbol
        )
}

/// A string quoted as Go's `strconv.Quote` quotes it, for messages.
pub fn quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' | '\\' => {
                out.push('\\');
                out.push(c);
            }
            _ if printable(c) => out.push(c),
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            _ => {
                let r = c as u32;
                if r < 0x20 || r == 0x7f {
                    out.push_str(&format!("\\x{r:02x}"));
                } else if r < 0x10000 {
                    out.push_str(&format!("\\u{r:04x}"));
                } else {
                    out.push_str(&format!("\\U{r:08x}"));
                }
            }
        }
    }
    out.push('"');
    out
}

/// Fills a message's arguments in, as abi.go's formatMessage does: `{0i}`, `{1n}`, `{0s}`, `{1q}`.
pub(crate) fn format_message(msg: &str, mut arg: impl FnMut(usize, u8) -> String) -> String {
    let b = msg.as_bytes();
    let mut out = String::with_capacity(msg.len());
    let mut i = 0;
    let mut start = 0;
    while i < b.len() {
        if b[i] == b'{' && i + 3 < b.len() && b[i + 3] == b'}' {
            out.push_str(&msg[start..i]);
            out.push_str(&arg(usize::from(b[i + 1].wrapping_sub(b'0')), b[i + 2]));
            i += 4;
            start = i;
            continue;
        }
        i += 1;
    }
    out.push_str(&msg[start..]);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn numbers_without_exponents() {
        assert_eq!(format_number(1e21), "1000000000000000000000");
        assert_eq!(format_number(-0.0), "-0");
        assert_eq!(format_number(0.1 + 0.2), "0.30000000000000004");
        assert_eq!(format_number(1e-7), "0.0000001");
        assert_eq!(format_number(f64::from_bits(0x43173ce9e5f1b8d5)), "1635224936803893.2");
        assert_eq!(format_number(120.0), "120");
    }

    #[test]
    fn go_number_syntax() {
        assert_eq!(parse_number("1_000"), Some(1000.0));
        assert_eq!(parse_number("0x1p-2"), Some(0.25));
        assert_eq!(parse_number("0x_1p0"), Some(1.0));
        assert_eq!(parse_number("1__0"), None);
        assert_eq!(parse_number("0x10"), None);
        assert_eq!(parse_number("."), None);
    }

    #[test]
    fn messages() {
        let m = format_message("index {0n} is out of range (length {1i})", |i, k| {
            format!("{i}{}", k as char)
        });
        assert_eq!(m, "index 0n is out of range (length 1i)");
    }
}
