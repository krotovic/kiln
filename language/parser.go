package language

// Limits bound what Compile accepts, so content can't make expressions that are slow to compile or run.
type Limits struct {
	MaxLength int // bytes of source
	MaxDepth  int // nesting of operators, calls and parentheses
	MaxNodes  int // literals, names, operators and calls; evaluation visits each at most once
	// MaxString bounds every string an evaluation makes or reads (0: no limit), so joining strings can't use up
	// memory; a longer one is an evaluation error
	MaxString int
}

// DefaultLimits suit expressions written by hand in content.
var DefaultLimits = Limits{MaxLength: 1024, MaxDepth: 32, MaxNodes: 256, MaxString: 4096}

// The grammar, loosest binding first:
//
//	cond    = or [ "?" cond ":" cond ]
//	or      = and { "||" and }
//	and     = eq { "&&" eq }
//	eq      = rel { ("==" | "!=") rel }
//	rel     = add { ("<" | "<=" | ">" | ">=") add }
//	add     = mul { ("+" | "-") mul }
//	mul     = unary { ("*" | "/" | "%") unary }
//	unary   = ("-" | "!") unary | primary
//	primary = number | string | "true" | "false" | name | name "(" [ cond { "," cond } ] ")" | "(" cond ")"
//
// Scripts parse the same grammar (parse_script.go), with primary extended by null, this, super, new, array
// literals, and postfix member access, method calls and indexing.
type parser struct {
	toks   []token
	at     int
	depth  int
	nodes  int
	limits Limits
	script bool
	native bool // a host module's source, which may declare native classes and functions
	// src and comments are a script's, for doc comments
	src      string
	comments []comment
}

func parse(src string, limits Limits) (node, error) {
	if len(src) > limits.MaxLength {
		return nil, compileErr(limits.MaxLength, "longer than %d characters", limits.MaxLength)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, limits: limits}
	n, err := p.cond()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, compileErr(t.pos, "unexpected %s", describe(t))
	}
	return n, nil
}

func (p *parser) peek() token { return p.toks[p.at] }

func (p *parser) next() token {
	t := p.toks[p.at]
	if t.kind != tokEOF {
		p.at++
	}
	return t
}

func (p *parser) isOp(ops ...string) bool {
	t := p.peek()
	if t.kind != tokOp {
		return false
	}
	for _, o := range ops {
		if t.text == o {
			return true
		}
	}
	return false
}

// count tracks a new node against MaxNodes.
func (p *parser) count(pos int) error {
	p.nodes++
	if p.nodes > p.limits.MaxNodes {
		return compileErr(pos, "more than %d parts", p.limits.MaxNodes)
	}
	return nil
}

// enter tracks nesting against MaxDepth; leave undoes it.
func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > p.limits.MaxDepth {
		return compileErr(pos, "nested deeper than %d", p.limits.MaxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) cond() (node, error) {
	if err := p.enter(p.peek().pos); err != nil {
		return nil, err
	}
	defer p.leave()
	c, err := p.or()
	if err != nil || !p.isOp("?") {
		return c, err
	}
	q := p.next()
	a, err := p.cond()
	if err != nil {
		return nil, err
	}
	if !p.isOp(":") {
		t := p.peek()
		return nil, compileErr(t.pos, "expected : but found %s", describe(t))
	}
	p.next()
	b, err := p.cond()
	if err != nil {
		return nil, err
	}
	if err := p.count(q.pos); err != nil {
		return nil, err
	}
	return &condNode{c: c, a: a, b: b, pos: q.pos}, nil
}

// binaryLevel parses one left-associative level of binary operators over the next tighter level.
func (p *parser) binaryLevel(next func() (node, error), ops ...string) (node, error) {
	l, err := next()
	if err != nil {
		return nil, err
	}
	for p.isOp(ops...) {
		op := p.next()
		r, err := next()
		if err != nil {
			return nil, err
		}
		if err := p.count(op.pos); err != nil {
			return nil, err
		}
		if op.text == "===" || op.text == "!==" {
			op.text = op.text[:2] // the same as == and !=: kinds never mix
		}
		if op.text == "&&" || op.text == "||" {
			l = &logicalNode{op: op.text, l: l, r: r, pos: op.pos}
		} else {
			l = &binaryNode{op: op.text, l: l, r: r, pos: op.pos}
		}
	}
	return l, nil
}

func (p *parser) or() (node, error)  { return p.binaryLevel(p.and, "||") }
func (p *parser) and() (node, error) { return p.binaryLevel(p.eq, "&&") }
func (p *parser) eq() (node, error)  { return p.binaryLevel(p.rel, "==", "!=", "===", "!==") }
func (p *parser) rel() (node, error) { return p.binaryLevel(p.add, "<", "<=", ">", ">=") }
func (p *parser) add() (node, error) { return p.binaryLevel(p.mul, "+", "-") }
func (p *parser) mul() (node, error) { return p.binaryLevel(p.unary, "*", "/", "%") }

func (p *parser) unary() (node, error) {
	if p.script && p.isWord("await") {
		t := p.next()
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		defer p.leave()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &awaitNode{scriptExpr{t.pos}, x}, p.count(t.pos)
	}
	if !p.isOp("-", "!") {
		if p.script {
			return p.postfix()
		}
		return p.primary()
	}
	op := p.next()
	if err := p.enter(op.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	x, err := p.unary()
	if err != nil {
		return nil, err
	}
	if err := p.count(op.pos); err != nil {
		return nil, err
	}
	return &unaryNode{op: op.text, x: x, pos: op.pos}, nil
}

func (p *parser) primary() (node, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return &literalNode{v: Num(t.num), pos: t.pos}, p.count(t.pos)
	case tokString:
		return &literalNode{v: Str(t.text), pos: t.pos}, p.count(t.pos)
	case tokIdent:
		switch t.text {
		case "true":
			return &literalNode{v: Boolean(true), pos: t.pos}, p.count(t.pos)
		case "false":
			return &literalNode{v: Boolean(false), pos: t.pos}, p.count(t.pos)
		}
		if p.script {
			switch t.text {
			case "null":
				return &nullNode{scriptExpr{t.pos}}, p.count(t.pos)
			case "this":
				return &thisNode{scriptExpr{t.pos}}, p.count(t.pos)
			case "super":
				return p.super(t)
			case "new":
				return p.newExpr(t)
			}
			if keywords[t.text] {
				return nil, compileErr(t.pos, "unexpected %q", t.text)
			}
		}
		if !p.isOp("(") {
			return &nameNode{name: t.text, pos: t.pos}, p.count(t.pos)
		}
		return p.call(t)
	case tokOp:
		if p.script && t.text == "[" {
			if err := p.enter(t.pos); err != nil {
				return nil, err
			}
			defer p.leave()
			items, err := p.list("]")
			if err != nil {
				return nil, err
			}
			return &arrayNode{scriptExpr{t.pos}, items}, p.count(t.pos)
		}
		if t.text == "(" {
			if err := p.enter(t.pos); err != nil {
				return nil, err
			}
			defer p.leave()
			n, err := p.cond()
			if err != nil {
				return nil, err
			}
			if !p.isOp(")") {
				c := p.peek()
				return nil, compileErr(c.pos, "expected ) but found %s", describe(c))
			}
			p.next()
			return n, nil
		}
	}
	return nil, compileErr(t.pos, "unexpected %s", describe(t))
}

func (p *parser) call(name token) (node, error) {
	p.next() // (
	if err := p.enter(name.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	n := &callNode{name: name.text, pos: name.pos}
	if !p.isOp(")") {
		for {
			arg, err := p.cond()
			if err != nil {
				return nil, err
			}
			n.args = append(n.args, arg)
			if !p.isOp(",") {
				break
			}
			p.next()
		}
	}
	if !p.isOp(")") {
		t := p.peek()
		return nil, compileErr(t.pos, "expected , or ) but found %s", describe(t))
	}
	p.next()
	return n, p.count(name.pos)
}

// list parses expressions separated by commas up to the closing op, and the op.
func (p *parser) list(closing string) ([]node, error) {
	var items []node
	for !p.isOp(closing) {
		item, err := p.cond()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if !p.isOp(closing) {
		t := p.peek()
		return nil, compileErr(t.pos, "expected , or %s but found %s", closing, describe(t))
	}
	p.next()
	return items, nil
}

// postfix parses a primary followed by any number of .name, .name(args) and [index] (the [ on the same line).
func (p *parser) postfix() (node, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isOp("."):
			p.next()
			name := p.next()
			if name.kind != tokIdent {
				return nil, compileErr(name.pos, "expected a name after . but found %s", describe(name))
			}
			if p.isOp("(") {
				open := p.next()
				if err := p.enter(open.pos); err != nil {
					return nil, err
				}
				args, err := p.list(")")
				p.leave()
				if err != nil {
					return nil, err
				}
				x = &methodCallNode{scriptExpr{name.pos}, x, name.text, args}
			} else {
				x = &memberNode{scriptExpr{name.pos}, x, name.text}
			}
		case p.isOp("[") && !p.peek().nl:
			// a [ that starts a line starts a statement ([v, err] = await p), not an index
			open := p.next()
			if err := p.enter(open.pos); err != nil {
				return nil, err
			}
			i, err := p.cond()
			p.leave()
			if err != nil {
				return nil, err
			}
			if !p.isOp("]") {
				t := p.peek()
				return nil, compileErr(t.pos, "expected ] but found %s", describe(t))
			}
			p.next()
			x = &indexNode{scriptExpr{open.pos}, x, i}
		case p.isOp("("):
			return nil, compileErr(p.peek().pos, "only functions and methods can be called")
		default:
			return x, nil
		}
		if err := p.count(x.position()); err != nil {
			return nil, err
		}
	}
}

// super parses super(args) and super.name(args).
func (p *parser) super(t token) (node, error) {
	if p.isOp("(") {
		p.next()
		args, err := p.list(")")
		if err != nil {
			return nil, err
		}
		return &superCallNode{scriptExpr{t.pos}, args}, p.count(t.pos)
	}
	if !p.isOp(".") {
		return nil, compileErr(t.pos, "super must be called, or a method called on it")
	}
	p.next()
	name := p.next()
	if name.kind != tokIdent || !p.isOp("(") {
		return nil, compileErr(name.pos, "super must be called, or a method called on it")
	}
	p.next()
	args, err := p.list(")")
	if err != nil {
		return nil, err
	}
	return &superMethodNode{scriptExpr{name.pos}, name.text, args}, p.count(name.pos)
}

// newExpr parses new T(args).
func (p *parser) newExpr(t token) (node, error) {
	typ, err := p.typeExpr()
	if err != nil {
		return nil, err
	}
	if typ.dims > 0 {
		return nil, compileErr(typ.pos, "make an array with [ ], not new")
	}
	if !p.isOp("(") {
		c := p.peek()
		return nil, compileErr(c.pos, "expected ( but found %s", describe(c))
	}
	p.next()
	args, err := p.list(")")
	if err != nil {
		return nil, err
	}
	return &newNode{scriptExpr{t.pos}, typ, args}, p.count(t.pos)
}

// typeExpr parses a type: a name, type arguments in < >, and any number of [].
func (p *parser) typeExpr() (*typeExpr, error) {
	t := p.next()
	if t.kind != tokIdent || (keywords[t.text] && t.text != "void") {
		return nil, compileErr(t.pos, "expected a type but found %s", describe(t))
	}
	if err := p.enter(t.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	te := &typeExpr{pos: t.pos, name: t.text}
	if p.isOp("<") {
		p.next()
		for {
			arg, err := p.typeExpr()
			if err != nil {
				return nil, err
			}
			te.args = append(te.args, arg)
			if !p.isOp(",") {
				break
			}
			p.next()
		}
		if !p.isOp(">") {
			c := p.peek()
			return nil, compileErr(c.pos, "expected , or > but found %s", describe(c))
		}
		p.next()
	}
	for p.isOp("[") && p.toks[p.at+1].kind == tokOp && p.toks[p.at+1].text == "]" {
		p.next()
		p.next()
		te.dims++
	}
	return te, nil
}

func describe(t token) string {
	switch t.kind {
	case tokEOF:
		return "the end"
	case tokNumber:
		return "a number"
	case tokString:
		return "a string"
	case tokIdent:
		return "\"" + t.text + "\""
	}
	return "\"" + t.text + "\""
}
