package expr

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
}

// operators, longest first so "<=" wins over "<".
var operators = []string{"&&", "||", "==", "!=", "<=", ">=", "+", "-", "*", "/", "%", "<", ">", "!", "?", ":", "(", ")", ","}

func lex(src string) ([]token, error) {
	var toks []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.') {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && isDigit(src[j]) {
					for i = j; i < len(src) && isDigit(src[i]); i++ {
					}
				}
			}
			f, err := strconv.ParseFloat(src[start:i], 64)
			if err != nil || math.IsInf(f, 0) {
				return nil, compileErr(start, "bad number %q", src[start:i])
			}
			if i < len(src) && isIdentStart(src[i]) {
				return nil, compileErr(start, "bad number %q", src[start:i+1])
			}
			toks = append(toks, token{kind: tokNumber, num: f, pos: start})
		case c == '"':
			start := i
			var b strings.Builder
			i++
			for {
				if i >= len(src) {
					return nil, compileErr(start, "unterminated string")
				}
				if src[i] == '"' {
					i++
					break
				}
				if src[i] == '\\' {
					if i+1 >= len(src) || (src[i+1] != '"' && src[i+1] != '\\') {
						return nil, compileErr(i, `only \" and \\ escapes are allowed`)
					}
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(src[i])
				i++
			}
			toks = append(toks, token{kind: tokString, text: b.String(), pos: start})
		case isIdentStart(c):
			start := i
			for i < len(src) && (isIdentStart(src[i]) || isDigit(src[i])) {
				i++
			}
			toks = append(toks, token{kind: tokIdent, text: src[start:i], pos: start})
		default:
			op := ""
			for _, o := range operators {
				if strings.HasPrefix(src[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, compileErr(i, "unexpected %q", string(src[i]))
			}
			toks = append(toks, token{kind: tokOp, text: op, pos: i})
			i += len(op)
		}
	}
	return append(toks, token{kind: tokEOF, pos: len(src)}), nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
