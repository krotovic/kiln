package language

import (
	"math"
	"strconv"
	"strings"
)

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokNumber
	tokString
	tokIdent
	tokOp // operators and punctuation; the text says which
)

type token struct {
	kind tokenKind
	text string  // the operator, identifier or decoded string
	num  float64 // for numbers
	pos  int     // 0-based byte offset
	end  int     // the byte offset after it
	nl   bool    // a line break comes before it (scripts end statements there)
}

// comment is a // or /* */ comment of a script, kept for doc comments and the formatter.
type comment struct {
	start, end int
	text       string
}

// operators, longest first so "<=" wins over "<".
var operators = []string{"&&", "||", "==", "!=", "<=", ">=", "+", "-", "*", "/", "%", "<", ">", "!", "?", ":", "(", ")", ","}

// scriptOperators are the operators and punctuation of scripts, longest first.
var scriptOperators = []string{
	"===", "!==",
	"&&", "||", "==", "!=", "<=", ">=", "+=", "-=", "*=", "/=", "%=", "++", "--",
	"+", "-", "*", "/", "%", "<", ">", "!", "?", ":", "(", ")", ",", ".", "[", "]", "{", "}", ";", "=",
}

// lex reads an expression's tokens. Punctuation only scripts use is refused with a message that says so.
func lex(src string) ([]token, error) {
	toks, _, err := lexMode(src, false)
	return toks, err
}

// lexScript reads a script's tokens: it also skips // and /* */ comments, reads strings in single quotes and the
// escapes \n, \t, \r and \', and knows the punctuation of statements.
func lexScript(src string) ([]token, []comment, error) { return lexMode(src, true) }

func lexMode(src string, script bool) ([]token, []comment, error) {
	ops := operators
	if script {
		ops = scriptOperators
	}
	var toks []token
	var comments []comment
	nl, emitted := false, 0
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			nl = true
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case script && c == '/' && i+1 < len(src) && src[i+1] == '/':
			start := i
			for i < len(src) && src[i] != '\n' {
				i++
			}
			comments = append(comments, comment{start: start, end: i, text: src[start:i]})
		case script && c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, nil, compileErr(i, "unterminated comment")
			}
			if strings.Contains(src[i:i+2+end], "\n") {
				nl = true
			}
			comments = append(comments, comment{start: i, end: i + end + 4, text: src[i : i+end+4]})
			i += end + 4
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.' || digitSeparator(src, i)) {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && isDigit(src[j]) {
					for i = j; i < len(src) && (isDigit(src[i]) || digitSeparator(src, i)); i++ {
					}
				}
			}
			f, err := strconv.ParseFloat(strings.ReplaceAll(src[start:i], "_", ""), 64)
			if err != nil || math.IsInf(f, 0) {
				return nil, nil, compileErr(start, "bad number %q", src[start:i])
			}
			if i < len(src) && src[i] == '_' {
				return nil, nil, compileErr(i, "a _ in a number goes between two digits, as in 1_000_000")
			}
			if i < len(src) && isIdentStart(src[i]) {
				return nil, nil, compileErr(start, "bad number %q", src[start:i+1])
			}
			toks = append(toks, token{kind: tokNumber, num: f, pos: start, nl: nl})
		case c == '"' || (script && c == '\''):
			start := i
			text, next, err := lexString(src, i, script)
			if err != nil {
				return nil, nil, err
			}
			i = next
			toks = append(toks, token{kind: tokString, text: text, pos: start, nl: nl})
		case isIdentStart(c):
			start := i
			for i < len(src) && (isIdentStart(src[i]) || isDigit(src[i])) {
				i++
			}
			toks = append(toks, token{kind: tokIdent, text: src[start:i], pos: start, nl: nl})
		default:
			op := ""
			for _, o := range ops {
				if strings.HasPrefix(src[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				if !script && strings.ContainsRune(".[]{};='", rune(c)) {
					return nil, nil, compileErr(i, "%q is not allowed in an expression", string(c))
				}
				return nil, nil, compileErr(i, "unexpected %q", string(src[i]))
			}
			toks = append(toks, token{kind: tokOp, text: op, pos: i, nl: nl})
			i += len(op)
		}
		if len(toks) > emitted {
			toks[len(toks)-1].end = i
			emitted, nl = len(toks), false
		}
	}
	return append(toks, token{kind: tokEOF, pos: len(src), end: len(src), nl: nl}), comments, nil
}

// lexString reads the string literal starting at src[i] and returns its text and the offset after it.
// Expressions allow only \" and \\; scripts also \', \n, \t and \r, and strings in single quotes.
func lexString(src string, i int, script bool) (string, int, error) {
	start := i
	quote := src[i]
	var b strings.Builder
	i++
	for {
		if i >= len(src) || (script && src[i] == '\n') {
			return "", 0, compileErr(start, "unterminated string")
		}
		if src[i] == quote {
			return b.String(), i + 1, nil
		}
		if src[i] == '\\' {
			if i+1 >= len(src) {
				return "", 0, compileErr(start, "unterminated string")
			}
			e := src[i+1]
			switch {
			case e == '"' || e == '\\':
				b.WriteByte(e)
			case script && e == '\'':
				b.WriteByte(e)
			case script && e == 'n':
				b.WriteByte('\n')
			case script && e == 't':
				b.WriteByte('\t')
			case script && e == 'r':
				b.WriteByte('\r')
			case script:
				return "", 0, compileErr(i, `unknown escape \%c (use \", \', \\, \n, \t or \r)`, e)
			default:
				return "", 0, compileErr(i, `only \" and \\ escapes are allowed`)
			}
			i += 2
			continue
		}
		b.WriteByte(src[i])
		i++
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digitSeparator reports whether src[i] is a _ between two digits, which numbers may have for readability:
// 1_000_000.
func digitSeparator(src string, i int) bool {
	return src[i] == '_' && i > 0 && isDigit(src[i-1]) && i+1 < len(src) && isDigit(src[i+1])
}
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
