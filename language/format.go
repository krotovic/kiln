package language

import (
	"sort"
	"strings"
)

// Format rewrites a script module's source in the one style: two spaces of indentation per open bracket, the
// line breaks as written (at most one blank line in a row), a space around binary operators and after commas,
// colons and keywords, and none inside brackets or around dots. Comments stay where they are. Source that doesn't
// lex is refused; formatting formatted source changes nothing.
func Format(src string) (string, error) {
	toks, comments, err := lexScript(src)
	if err != nil {
		return "", err
	}
	toks = toks[:len(toks)-1] // EOF
	roles := classify(toks)
	items := make([]fmtItem, 0, len(toks)+len(comments))
	for i := range toks {
		items = append(items, fmtItem{start: toks[i].pos, end: toks[i].end, tok: i})
	}
	for _, c := range comments {
		items = append(items, fmtItem{start: c.start, end: c.end, tok: -1})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].start < items[j].start })

	var b strings.Builder
	depth := 0
	for i, it := range items {
		if i > 0 {
			gap := src[items[i-1].end:it.start]
			if lines := strings.Count(gap, "\n"); lines > 0 {
				b.WriteString("\n")
				if lines > 1 {
					b.WriteString("\n")
				}
				level := depth
				if it.tok >= 0 && toks[it.tok].kind == tokOp && strings.Contains(")]}", toks[it.tok].text) {
					level--
				}
				b.WriteString(strings.Repeat("  ", max(level, 0)))
			} else if items[i-1].tok < 0 || it.tok < 0 || spaceBetween(toks, roles, items[i-1].tok, it.tok) {
				b.WriteString(" ")
			}
		}
		b.WriteString(src[it.start:it.end])
		if it.tok >= 0 && toks[it.tok].kind == tokOp {
			switch t := toks[it.tok].text; {
			case t == "(" || t == "[" || t == "{":
				depth++
			case t == ")" || t == "]" || t == "}":
				depth--
			}
		}
	}
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if out == "" {
		return "", nil
	}
	return out + "\n", nil
}

// fmtItem is a token (tok its index) or a comment (tok -1).
type fmtItem struct {
	start, end int
	tok        int
}

// role is what a token does where it stands, which decides the spaces around it.
type role uint8

const (
	roleNone      role = iota
	roleUnary          // - + ! before an operand
	rolePrefix         // ++ -- before an operand
	roleOptional       // the ? of an optional method: name?()
	roleTypeOpen       // the < of Map<, Array< or Promise<
	roleTypeClose      // its >
	roleTernary        // the : of a conditional
)

// classify works out each token's role in one pass.
func classify(toks []token) []role {
	roles := make([]role, len(toks))
	typeOpen := 0
	pending := map[int]int{} // conditionals' ? waiting for their :, by bracket depth
	depth := 0
	for i, t := range toks {
		var prev *token
		if i > 0 {
			prev = &toks[i-1]
		}
		if t.kind != tokOp {
			continue
		}
		switch t.text {
		case "-", "+", "!":
			if !endsOperand(prev) {
				roles[i] = roleUnary
			}
		case "++", "--":
			if !endsOperand(prev) {
				roles[i] = rolePrefix
			}
		case "?":
			if i+1 < len(toks) && toks[i+1].kind == tokOp && toks[i+1].text == "(" {
				roles[i] = roleOptional
			} else {
				pending[depth]++
			}
		case ":":
			if pending[depth] > 0 {
				pending[depth]--
				roles[i] = roleTernary
			}
		case "<":
			if prev != nil && prev.kind == tokIdent && (prev.text == "Map" || prev.text == "Array" || prev.text == "Promise") {
				roles[i] = roleTypeOpen
				typeOpen++
			}
		case ">":
			if typeOpen > 0 {
				roles[i] = roleTypeClose
				typeOpen--
			}
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			delete(pending, depth)
			depth--
		}
	}
	return roles
}

// endsOperand reports whether a token ends an operand, so an operator after it is binary or postfix.
func endsOperand(t *token) bool {
	if t == nil {
		return false
	}
	switch t.kind {
	case tokNumber, tokString:
		return true
	case tokIdent:
		switch t.text {
		case "this", "null", "true", "false", "super":
			return true
		}
		return !keywords[t.text]
	}
	return t.text == ")" || t.text == "]"
}

// controlWords take a space before a ( after them.
var controlWords = map[string]bool{"if": true, "while": true, "for": true, "return": true, "await": true}

// spaceBetween says whether a space goes between tokens a and b on one line.
func spaceBetween(toks []token, roles []role, a, b int) bool {
	p, c := toks[a], toks[b]
	if p.kind == tokOp {
		if p.text == "(" || p.text == "[" || p.text == "." {
			return false
		}
		switch roles[a] {
		case roleUnary, rolePrefix:
			// - -x must not become --x, nor ! ==, a longer operator
			return c.kind == tokOp && strings.ContainsAny(c.text[:1], "+-=")
		case roleOptional, roleTypeOpen:
			return false
		case roleTypeClose:
			if c.kind == tokOp && (c.text == "(" || c.text == "[") {
				return false
			}
		}
	}
	if c.kind != tokOp {
		return true
	}
	switch c.text {
	case ",", ";", ".", ")", "]":
		return false
	case ":":
		return roles[b] == roleTernary
	case "?":
		return roles[b] != roleOptional
	case "++", "--":
		return roles[b] == rolePrefix
	case "(":
		if p.kind == tokIdent {
			return controlWords[p.text]
		}
		return p.kind == tokOp && p.text != ")" && p.text != "]" && roles[a] != roleTypeClose
	case "[":
		return !endsOperand(&p) && !(p.kind == tokOp && (p.text == "(" || p.text == "["))
	case "<":
		return roles[b] != roleTypeOpen
	case ">":
		return roles[b] != roleTypeClose
	case "}":
		return !(p.kind == tokOp && p.text == "{")
	}
	return true
}
