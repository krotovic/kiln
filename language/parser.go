package expr

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
type parser struct {
	toks   []token
	at     int
	depth  int
	nodes  int
	limits Limits
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
func (p *parser) eq() (node, error)  { return p.binaryLevel(p.rel, "==", "!=") }
func (p *parser) rel() (node, error) { return p.binaryLevel(p.add, "<", "<=", ">", ">=") }
func (p *parser) add() (node, error) { return p.binaryLevel(p.mul, "+", "-") }
func (p *parser) mul() (node, error) { return p.binaryLevel(p.unary, "*", "/", "%") }

func (p *parser) unary() (node, error) {
	if !p.isOp("-", "!") {
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
		if !p.isOp("(") {
			return &nameNode{name: t.text, pos: t.pos}, p.count(t.pos)
		}
		return p.call(t)
	case tokOp:
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
