/**
 * What a compiled script asks its host to do the way Go does it, so a script gives the same results in the
 * browser as on the servers: numbers in their shortest text form (strconv.FormatFloat(x, 'f', -1, 64)), numbers
 * read from text (strconv.ParseFloat), and strings quoted for messages (strconv.Quote). The tests check them
 * against what Go gives (fixtures from server/internal/engine/language).
 */

/** A number's text form, as str() and + write it: the shortest digits that read back the same, never an exponent */
export function formatNumber(x: number): string {
  if (Object.is(x, -0)) return "-0";
  if (x === 0) return "0";
  const neg = x < 0;
  // toExponential() without digits gives the shortest ones that read back the same, as Go's shortest form does
  const [mantissa, exp] = Math.abs(x).toExponential().split("e");
  const digits = mantissa.replace(".", "");
  const point = Number(exp) + 1; // the decimal point comes after this many digits
  let out: string;
  if (point <= 0) out = "0." + "0".repeat(-point) + digits;
  else if (point >= digits.length) out = digits + "0".repeat(point - digits.length);
  else out = digits.slice(0, point) + "." + digits.slice(point);
  return neg ? "-" + out : out;
}

const lower = (c: string) => c.toLowerCase();
const isDigit = (c: string) => c >= "0" && c <= "9";
const isHex = (c: string) => isDigit(c) || (lower(c) >= "a" && lower(c) <= "f");

/** Go's underscoreOK: underscores only between digits, or between a base prefix and a digit */
function underscoreOK(s: string): boolean {
  let saw = "^";
  let i = 0;
  if (s.length >= 1 && (s[0] === "-" || s[0] === "+")) s = s.slice(1);
  let hex = false;
  if (s.length >= 2 && s[0] === "0" && "box".includes(lower(s[1]))) {
    i = 2;
    saw = "0";
    hex = lower(s[1]) === "x";
  }
  for (; i < s.length; i++) {
    if (isDigit(s[i]) || (hex && isHex(s[i]))) {
      saw = "0";
      continue;
    }
    if (s[i] === "_") {
      if (saw !== "0") return false;
      saw = "_";
      continue;
    }
    if (saw === "_") return false;
    saw = "!";
  }
  return saw !== "_";
}

/** x × 2^e, rounded once, without overflowing or underflowing on the way */
function scale2(x: number, e: number): number {
  while (e > 1000) {
    x *= 2 ** 1000;
    e -= 1000;
  }
  while (e < -1000) {
    x *= 2 ** -1000;
    e += 1000;
  }
  return x * 2 ** e;
}

/**
 * A number read as Go's strconv.ParseFloat reads it, or null when it isn't one (Infinity and NaN for "inf" and
 * "nan", and for numbers too large).
 */
export function parseNumber(s: string): number | null {
  // inf, infinity and nan, with a sign
  const special = /^([+-]?)(inf|infinity|nan)$/i.exec(s);
  if (special) {
    if (lower(special[2]) === "nan") return NaN;
    return special[1] === "-" ? -Infinity : Infinity;
  }
  let i = 0;
  let neg = false;
  if (s[i] === "+" || s[i] === "-") {
    neg = s[i] === "-";
    i++;
  }
  let hex = false;
  if (i + 2 < s.length && s[i] === "0" && lower(s[i + 1]) === "x") {
    hex = true;
    i += 2;
  }
  let underscores = false;
  let sawDot = false;
  let sawDigits = false;
  let mantissa = "";
  let fracDigits = 0;
  for (; i < s.length; i++) {
    const c = s[i];
    if (c === "_") {
      underscores = true;
      continue;
    }
    if (c === ".") {
      if (sawDot) break;
      sawDot = true;
      continue;
    }
    if (isDigit(c) || (hex && isHex(c))) {
      sawDigits = true;
      mantissa += c;
      if (sawDot) fracDigits++;
      continue;
    }
    break;
  }
  if (!sawDigits) return null;
  let exp = 0;
  let hasExp = false;
  if (i < s.length && lower(s[i]) === (hex ? "p" : "e")) {
    hasExp = true;
    i++;
    if (i >= s.length) return null;
    let sign = 1;
    if (s[i] === "+") i++;
    else if (s[i] === "-") {
      i++;
      sign = -1;
    }
    if (i >= s.length || !isDigit(s[i])) return null;
    let digits = "";
    for (; i < s.length && (isDigit(s[i]) || s[i] === "_"); i++) {
      if (s[i] === "_") {
        underscores = true;
        continue;
      }
      digits += s[i];
    }
    exp = sign * Number(digits);
  } else if (hex) {
    return null; // a hexadecimal mantissa needs a p exponent
  }
  if (i !== s.length) return null;
  if (underscores && !underscoreOK(s)) return null;
  let value: number;
  if (hex) {
    const m = BigInt("0x" + mantissa);
    value = m === 0n ? 0 : scale2(Number(m), exp - 4 * fracDigits);
  } else {
    const text = mantissa.slice(0, mantissa.length - fracDigits) + "." + mantissa.slice(mantissa.length - fracDigits);
    value = Number(text + (hasExp ? "e" + exp : ""));
  }
  return neg ? -value : value;
}

const printable = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]$/u;

/** A string quoted as Go's strconv.Quote quotes it, for messages */
export function quote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const r = ch.codePointAt(0)!;
    if (ch === '"' || ch === "\\") {
      out += "\\" + ch;
    } else if (printable.test(ch)) {
      out += ch;
    } else {
      const named: Record<number, string> = { 7: "\\a", 8: "\\b", 12: "\\f", 10: "\\n", 13: "\\r", 9: "\\t", 11: "\\v" };
      if (named[r]) out += named[r];
      else if (r < 0x20 || r === 0x7f) out += "\\x" + r.toString(16).padStart(2, "0");
      else if (r < 0x10000) out += "\\u" + r.toString(16).padStart(4, "0");
      else out += "\\U" + r.toString(16).padStart(8, "0");
    }
  }
  return out + '"';
}

/** Fills a message's arguments in, as abi.go's formatMessage does: {0i}, {1n}, {0s}, {1q} */
export function formatMessage(msg: string, arg: (i: number, kind: string) => string): string {
  return msg.replace(/\{(\d)([insq])\}/g, (_, i: string, kind: string) => arg(Number(i), kind));
}
