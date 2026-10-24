package language

import (
	"sort"
	"strings"
)

// keywords can't be names in scripts. void is a type; constructor, of, from, as, readonly, private, native and
// default are words only where the grammar expects them.
var keywords = map[string]bool{
	"let": true, "const": true, "function": true, "class": true, "interface": true, "type": true,
	"extends": true, "implements": true, "new": true, "this": true, "super": true, "return": true, "if": true,
	"else": true, "while": true, "for": true, "break": true, "continue": true, "import": true, "export": true,
	"null": true, "true": true, "false": true, "void": true, "undefined": true,
}

// The grammar of a module, on top of the expression grammar (parser.go):
//
//	module  = { import } { [ "export" [ "default" ] ] decl }
//	import  = "import" ( name [ "," names ] | names ) "from" string [ ";" ]   (a lone name is the default export)
//	names   = "{" name [ "as" name ] { "," name [ "as" name ] } "}"
//	decl    = class | interface | "type" name "=" type | function | "const" name [ ":" type ] "=" cond
//	class   = [ "native" ] "class" name [ "extends" name ] [ "implements" name { "," name } ] "{" { member } "}"
//	member  = { "private" | "readonly" } ( name [ "?" ] ":" type [ "=" cond ] | name params [ ":" type ] block
//	          | "constructor" params block )
//	function = [ "native" ] "function" name params [ ":" type ] ( block | ";" )
//	params  = "(" [ name ":" type { "," name ":" type } ] ")"
//	stmt    = block | "let" | "const" ... | "if" | "while" | "for" | "break" | "continue" | "return" [ cond ]
//	          | target ( "=" | "+=" | "-=" | "*=" | "/=" | "%=" ) cond | target ( "++" | "--" ) | cond
//
// Statements end with ; or at a line break, a } or the end, so semicolons are optional as in TypeScript; a return
// followed by a line break returns nothing.

// parseModule reads a script module. native allows native classes and functions (host modules only).
func parseModule(src string, limits ScriptLimits, native bool) (*moduleAST, error) {
	if len(src) > limits.MaxSource {
		return nil, compileErr(0, "longer than %d bytes", limits.MaxSource)
	}
	toks, comments, err := lexScript(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, script: true, native: native, src: src, comments: comments,
		limits: Limits{MaxDepth: limits.MaxNesting, MaxNodes: int(^uint(0) >> 1), MaxString: limits.MaxString}}
	m := &moduleAST{}
	for p.isWord("import") {
		imp, err := p.importDecl()
		if err != nil {
			return nil, err
		}
		m.imports = append(m.imports, imp)
	}
	for p.peek().kind != tokEOF {
		d, err := p.decl()
		if err != nil {
			return nil, err
		}
		m.decls = append(m.decls, d)
	}
	return m, nil
}

// ModuleImports lists what a script module imports, in order, without checking it, so a host can find a
// module's dependencies (and the modules that depend on one) from its source alone.
func ModuleImports(src string) ([]string, error) {
	m, err := parseModule(src, DefaultScriptLimits, false)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m.imports))
	for _, imp := range m.imports {
		out = append(out, imp.from)
	}
	return out, nil
}

func (p *parser) isWord(w string) bool {
	t := p.peek()
	return t.kind == tokIdent && t.text == w
}

// word consumes the keyword w or fails.
func (p *parser) word(w string) (token, error) {
	t := p.next()
	if t.kind != tokIdent || t.text != w {
		return t, compileErr(t.pos, "expected %s but found %s", w, describe(t))
	}
	return t, nil
}

// op consumes the operator o or fails.
func (p *parser) op(o string) (token, error) {
	t := p.next()
	if t.kind != tokOp || t.text != o {
		return t, compileErr(t.pos, "expected %s but found %s", o, describe(t))
	}
	return t, nil
}

// name consumes a name that isn't a keyword.
func (p *parser) name(what string) (token, error) {
	t := p.next()
	if t.kind != tokIdent || keywords[t.text] {
		return t, compileErr(t.pos, "expected %s but found %s", what, describe(t))
	}
	return t, nil
}

// end finishes a statement: a ;, or nothing before a line break, a } or the end.
func (p *parser) end() error {
	t := p.peek()
	if t.kind == tokOp && t.text == ";" {
		p.next()
		return nil
	}
	if t.nl || t.kind == tokEOF || (t.kind == tokOp && t.text == "}") {
		return nil
	}
	return compileErr(t.pos, "expected ; or a new line but found %s", describe(t))
}

func (p *parser) importDecl() (*importDecl, error) {
	start := p.next()
	imp := &importDecl{pos: start.pos}
	if p.peek().kind == tokIdent {
		n, err := p.name("a name to import")
		if err != nil {
			return nil, err
		}
		imp.names = append(imp.names, importName{pos: n.pos, namePos: -1, name: "default", alias: n.text})
		if !p.isOp(",") {
			return p.importFrom(imp)
		}
		p.next()
	}
	if _, err := p.op("{"); err != nil {
		return nil, err
	}
	for !p.isOp("}") {
		n, err := p.name("a name to import")
		if err != nil {
			return nil, err
		}
		in := importName{pos: n.pos, namePos: n.pos, name: n.text, alias: n.text}
		if p.isWord("as") {
			p.next()
			a, err := p.name("a name after as")
			if err != nil {
				return nil, err
			}
			in.pos, in.alias = a.pos, a.text
		}
		imp.names = append(imp.names, in)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if _, err := p.op("}"); err != nil {
		return nil, err
	}
	if len(imp.names) == 0 {
		return nil, compileErr(start.pos, "import names nothing")
	}
	return p.importFrom(imp)
}

func (p *parser) importFrom(imp *importDecl) (*importDecl, error) {
	if _, err := p.word("from"); err != nil {
		return nil, err
	}
	from := p.next()
	if from.kind != tokString {
		return nil, compileErr(from.pos, "expected the module's name in quotes but found %s", describe(from))
	}
	imp.from, imp.fromPos = from.text, from.pos
	return imp, p.end()
}

func (p *parser) decl() (*decl, error) {
	d := &decl{start: p.peek().pos}
	doc := p.docBefore(p.peek().pos)
	if p.isWord("import") {
		return nil, compileErr(p.peek().pos, "imports come before everything else")
	}
	if p.isWord("export") {
		p.next()
		d.exported = true
		if p.isWord("default") {
			p.next()
			d.isDefault = true
			if !p.isWord("class") {
				return nil, compileErr(p.peek().pos, "only a class can be the default export")
			}
		}
	}
	native := false
	if p.isWord("native") {
		t := p.next()
		if !p.native {
			return nil, compileErr(t.pos, "only the host's modules declare native code")
		}
		native = true
	}
	t := p.peek()
	// A host module is a definition file: it declares, and the host's code does the rest
	if p.native && !native && !p.isWord("interface") && !p.isWord("type") {
		return nil, compileErr(t.pos, "a host module is a definition file: interfaces, types, and native classes and functions only")
	}
	var err error
	switch {
	case p.isWord("class"):
		d.class, err = p.classDecl(native)
	case native && !p.isWord("function"):
		return nil, compileErr(t.pos, "expected class or function after native")
	case p.isWord("interface"):
		d.iface, err = p.interfaceDecl()
	case p.isWord("type"):
		d.alias, err = p.aliasDecl()
	case p.isWord("function"):
		d.native = native
		d.fn, err = p.function(native)
	case p.isWord("const"):
		d.konst, err = p.constDecl()
	case p.isWord("let"):
		return nil, compileErr(t.pos, "a module can't have variables; use const, or a field of a class")
	default:
		return nil, compileErr(t.pos, "expected a declaration (class, interface, type, function or const) but found %s", describe(t))
	}
	if err != nil {
		return nil, err
	}
	d.end = p.lastEnd()
	switch {
	case d.class != nil:
		d.class.doc = doc
	case d.iface != nil:
		d.iface.doc = doc
	case d.alias != nil:
		d.alias.doc = doc
	case d.fn != nil:
		d.fn.doc = doc
	case d.konst != nil:
		d.konst.doc = doc
	}
	return d, nil
}

func (p *parser) aliasDecl() (*aliasDecl, error) {
	p.next()
	n, err := p.name("the type's name")
	if err != nil {
		return nil, err
	}
	if _, err := p.op("="); err != nil {
		return nil, err
	}
	typ, err := p.typeExpr()
	if err != nil {
		return nil, err
	}
	return &aliasDecl{pos: n.pos, name: n.text, typ: typ}, p.end()
}

func (p *parser) constDecl() (*constDecl, error) {
	p.next()
	n, err := p.name("the constant's name")
	if err != nil {
		return nil, err
	}
	c := &constDecl{pos: n.pos, name: n.text}
	if p.isOp(":") {
		p.next()
		if c.typ, err = p.typeExpr(); err != nil {
			return nil, err
		}
	}
	if !p.isOp("=") {
		return nil, compileErr(p.peek().pos, "a const needs a value")
	}
	p.next()
	if c.init, err = p.cond(); err != nil {
		return nil, err
	}
	return c, p.end()
}

// typeList parses names of classes or interfaces separated by commas.
func (p *parser) typeList() ([]*typeExpr, error) {
	var out []*typeExpr
	for {
		t, err := p.typeExpr()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if !p.isOp(",") {
			return out, nil
		}
		p.next()
	}
}

func (p *parser) classDecl(native bool) (*classDecl, error) {
	p.next()
	n, err := p.name("the class's name")
	if err != nil {
		return nil, err
	}
	c := &classDecl{pos: n.pos, name: n.text, native: native}
	if p.isWord("extends") {
		p.next()
		if c.extends, err = p.typeExpr(); err != nil {
			return nil, err
		}
	}
	if p.isWord("implements") {
		p.next()
		if c.implements, err = p.typeList(); err != nil {
			return nil, err
		}
	}
	if _, err := p.op("{"); err != nil {
		return nil, err
	}
	for !p.isOp("}") {
		if p.peek().kind == tokEOF {
			return nil, compileErr(p.peek().pos, "the class has no closing }")
		}
		if p.isOp(";") {
			p.next()
			continue
		}
		doc := p.docBefore(p.peek().pos)
		var private, readonly bool
		for p.isWord("private") || p.isWord("readonly") {
			if p.next().text == "private" {
				private = true
			} else {
				readonly = true
			}
		}
		if p.isWord("constructor") {
			t := p.next()
			if c.ctor != nil {
				return nil, compileErr(t.pos, "the class already has a constructor")
			}
			if private || readonly {
				return nil, compileErr(t.pos, "a constructor can't be private or readonly")
			}
			if c.ctor, err = p.funcRest(t, native); err != nil {
				return nil, err
			}
			if c.ctor.result != nil {
				return nil, compileErr(c.ctor.result.pos, "a constructor returns nothing")
			}
			c.ctor.doc, c.ctor.end = doc, p.lastEnd()
			continue
		}
		mn, err := p.name("a field or method")
		if err != nil {
			return nil, err
		}
		if p.isOp("(") {
			if readonly {
				return nil, compileErr(mn.pos, "a method can't be readonly")
			}
			m, err := p.funcRest(mn, native)
			if err != nil {
				return nil, err
			}
			m.private, m.doc, m.end = private, doc, p.lastEnd()
			c.methods = append(c.methods, m)
			continue
		}
		f, err := p.fieldRest(mn, private, readonly)
		if err != nil {
			return nil, err
		}
		if native && f.init != nil {
			return nil, compileErr(f.pos, "a native class's fields come from the host")
		}
		f.doc, f.end = doc, p.lastEnd()
		c.fields = append(c.fields, f)
	}
	p.next()
	return c, nil
}

func (p *parser) fieldRest(n token, private, readonly bool) (*fieldDecl, error) {
	f := &fieldDecl{pos: n.pos, name: n.text, private: private, readonly: readonly}
	if p.isOp("?") {
		return nil, compileErr(p.peek().pos, "fields can't be optional: every field has a value (its type's zero value when not given one)")
	}
	var err error
	if p.isOp(":") {
		p.next()
		if f.typ, err = p.typeExpr(); err != nil {
			return nil, err
		}
	}
	if p.isOp("=") {
		p.next()
		if f.init, err = p.cond(); err != nil {
			return nil, err
		}
	}
	if f.typ == nil && f.init == nil {
		return nil, compileErr(n.pos, "the field %s needs a type or a value", n.text)
	}
	return f, p.end()
}

func (p *parser) function(native bool) (*funcDecl, error) {
	p.next()
	n, err := p.name("the function's name")
	if err != nil {
		return nil, err
	}
	return p.funcRest(n, native)
}

// funcRest parses a function's params, result and body after its name. Native functions end with ; instead of
// a body.
func (p *parser) funcRest(n token, native bool) (*funcDecl, error) {
	f := &funcDecl{pos: n.pos, name: n.text}
	var err error
	if f.params, err = p.params(); err != nil {
		return nil, err
	}
	if p.isOp(":") {
		p.next()
		if f.result, err = p.typeExpr(); err != nil {
			return nil, err
		}
		if f.result.name == "void" && f.result.dims == 0 {
			f.result = nil
		}
	}
	if native {
		return f, p.end()
	}
	if !p.isOp("{") {
		return nil, compileErr(p.peek().pos, "expected { but found %s", describe(p.peek()))
	}
	f.body, err = p.block()
	return f, err
}

func (p *parser) params() ([]paramDecl, error) {
	if _, err := p.op("("); err != nil {
		return nil, err
	}
	var out []paramDecl
	for !p.isOp(")") {
		n, err := p.name("a parameter")
		if err != nil {
			return nil, err
		}
		if p.isOp("?") {
			return nil, compileErr(p.peek().pos, "parameters can't be optional")
		}
		if !p.isOp(":") {
			return nil, compileErr(p.peek().pos, "the parameter %s needs a type", n.text)
		}
		p.next()
		typ, err := p.typeExpr()
		if err != nil {
			return nil, err
		}
		out = append(out, paramDecl{pos: n.pos, name: n.text, typ: typ})
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	_, err := p.op(")")
	return out, err
}

func (p *parser) interfaceDecl() (*interfaceDecl, error) {
	p.next()
	n, err := p.name("the interface's name")
	if err != nil {
		return nil, err
	}
	d := &interfaceDecl{pos: n.pos, name: n.text}
	if p.isWord("extends") {
		p.next()
		if d.extends, err = p.typeList(); err != nil {
			return nil, err
		}
	}
	if _, err := p.op("{"); err != nil {
		return nil, err
	}
	for !p.isOp("}") {
		if p.peek().kind == tokEOF {
			return nil, compileErr(p.peek().pos, "the interface has no closing }")
		}
		if p.isOp(";") || p.isOp(",") {
			p.next()
			continue
		}
		doc := p.docBefore(p.peek().pos)
		readonly := false
		if p.isWord("readonly") {
			p.next()
			readonly = true
		}
		mn, err := p.name("a field or method")
		if err != nil {
			return nil, err
		}
		optional := false
		if p.isOp("?") {
			p.next()
			optional = true
		}
		if p.isOp("(") {
			if readonly {
				return nil, compileErr(mn.pos, "a method can't be readonly")
			}
			m := &funcDecl{pos: mn.pos, name: mn.text, optional: optional, doc: doc}
			if m.params, err = p.params(); err != nil {
				return nil, err
			}
			if p.isOp(":") {
				p.next()
				if m.result, err = p.typeExpr(); err != nil {
					return nil, err
				}
				if m.result.name == "void" && m.result.dims == 0 {
					m.result = nil
				}
			}
			m.end = p.lastEnd()
			d.methods = append(d.methods, m)
		} else {
			if optional {
				return nil, compileErr(mn.pos, "fields can't be optional: every field has a value")
			}
			if _, err := p.op(":"); err != nil {
				return nil, err
			}
			typ, err := p.typeExpr()
			if err != nil {
				return nil, err
			}
			d.fields = append(d.fields, &fieldDecl{pos: mn.pos, name: mn.text, typ: typ, readonly: readonly, doc: doc, end: p.lastEnd()})
		}
		if t := p.peek(); t.kind == tokOp && (t.text == ";" || t.text == ",") {
			p.next()
		} else if !t.nl && !(t.kind == tokOp && t.text == "}") {
			return nil, compileErr(t.pos, "expected ; or a new line but found %s", describe(t))
		}
	}
	p.next()
	return d, nil
}

func (p *parser) block() (*blockStmt, error) {
	open, err := p.op("{")
	if err != nil {
		return nil, err
	}
	if err := p.enter(open.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	b := &blockStmt{stmtBase: stmtBase{open.pos}}
	for !p.isOp("}") {
		if p.peek().kind == tokEOF {
			return nil, compileErr(open.pos, "the block has no closing }")
		}
		if p.isOp(";") {
			p.next()
			continue
		}
		s, err := p.stmt()
		if err != nil {
			return nil, err
		}
		b.stmts = append(b.stmts, s)
	}
	p.next()
	return b, nil
}

func (p *parser) stmt() (stmt, error) {
	t := p.peek()
	if t.kind == tokOp && t.text == "{" {
		return p.block()
	}
	if t.kind == tokIdent {
		switch t.text {
		case "let", "const":
			s, err := p.letStmt()
			if err != nil {
				return nil, err
			}
			return s, p.end()
		case "if":
			return p.ifStmt()
		case "while":
			p.next()
			cond, err := p.parenCond()
			if err != nil {
				return nil, err
			}
			body, err := p.block()
			if err != nil {
				return nil, err
			}
			return &whileStmt{stmtBase{t.pos}, cond, body}, nil
		case "for":
			return p.forStmt()
		case "break":
			p.next()
			return &breakStmt{stmtBase{t.pos}}, p.end()
		case "continue":
			p.next()
			return &continueStmt{stmtBase{t.pos}}, p.end()
		case "return":
			p.next()
			r := &returnStmt{stmtBase: stmtBase{t.pos}}
			if n := p.peek(); !n.nl && n.kind != tokEOF && !(n.kind == tokOp && (n.text == ";" || n.text == "}")) {
				v, err := p.cond()
				if err != nil {
					return nil, err
				}
				r.value = v
			}
			return r, p.end()
		case "function", "class", "interface", "type", "import", "export":
			return nil, compileErr(t.pos, "%s only goes at the top of a module", t.text)
		}
	}
	s, err := p.simpleStmt()
	if err != nil {
		return nil, err
	}
	return s, p.end()
}

// parenCond parses ( cond ).
func (p *parser) parenCond() (node, error) {
	if _, err := p.op("("); err != nil {
		return nil, err
	}
	c, err := p.cond()
	if err != nil {
		return nil, err
	}
	_, err = p.op(")")
	return c, err
}

func (p *parser) letStmt() (*letStmt, error) {
	kw := p.next()
	n, err := p.name("a variable's name")
	if err != nil {
		return nil, err
	}
	s := &letStmt{stmtBase: stmtBase{n.pos}, name: n.text, constant: kw.text == "const"}
	if p.isOp(":") {
		p.next()
		if s.typ, err = p.typeExpr(); err != nil {
			return nil, err
		}
	}
	if p.isOp("=") {
		p.next()
		if s.init, err = p.cond(); err != nil {
			return nil, err
		}
	}
	if s.init == nil {
		if s.constant {
			return nil, compileErr(n.pos, "a const needs a value")
		}
		if s.typ == nil {
			return nil, compileErr(n.pos, "the variable %s needs a type or a value", n.text)
		}
	}
	return s, nil
}

// simpleStmt parses an assignment, an increment or an expression statement.
func (p *parser) simpleStmt() (stmt, error) {
	if p.isOp("++", "--") {
		op := p.next()
		target, err := p.postfix()
		if err != nil {
			return nil, err
		}
		return &assignStmt{stmtBase{op.pos}, target, op.text, nil}, nil
	}
	start := p.peek().pos
	x, err := p.cond()
	if err != nil {
		return nil, err
	}
	switch {
	case p.isOp("=", "+=", "-=", "*=", "/=", "%="):
		op := p.next()
		v, err := p.cond()
		if err != nil {
			return nil, err
		}
		return &assignStmt{stmtBase{op.pos}, x, op.text, v}, nil
	case p.isOp("++", "--"):
		op := p.next()
		return &assignStmt{stmtBase{op.pos}, x, op.text, nil}, nil
	}
	return &exprStmt{stmtBase{start}, x}, nil
}

func (p *parser) ifStmt() (stmt, error) {
	t := p.next()
	cond, err := p.parenCond()
	if err != nil {
		return nil, err
	}
	then, err := p.block()
	if err != nil {
		return nil, err
	}
	s := &ifStmt{stmtBase{t.pos}, cond, then, nil}
	if p.isWord("else") {
		p.next()
		if p.isWord("if") {
			if s.els, err = p.ifStmt(); err != nil {
				return nil, err
			}
		} else if s.els, err = p.block(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (p *parser) forStmt() (stmt, error) {
	t := p.next()
	if _, err := p.op("("); err != nil {
		return nil, err
	}
	// for (let|const name of iter)
	if (p.isWord("let") || p.isWord("const")) && p.at+2 < len(p.toks) && p.toks[p.at+1].kind == tokIdent &&
		p.toks[p.at+2].kind == tokIdent && p.toks[p.at+2].text == "of" {
		kw := p.next()
		n := p.next()
		p.next()
		iter, err := p.cond()
		if err != nil {
			return nil, err
		}
		if _, err := p.op(")"); err != nil {
			return nil, err
		}
		body, err := p.block()
		if err != nil {
			return nil, err
		}
		if keywords[n.text] {
			return nil, compileErr(n.pos, "expected a variable's name but found %s", describe(n))
		}
		return &forOfStmt{stmtBase{n.pos}, n.text, kw.text == "const", iter, body}, nil
	}
	s := &forStmt{stmtBase: stmtBase{t.pos}}
	var err error
	if !p.isOp(";") {
		if p.isWord("let") || p.isWord("const") {
			s.init, err = p.letStmt()
		} else {
			s.init, err = p.simpleStmt()
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.op(";"); err != nil {
		return nil, err
	}
	if !p.isOp(";") {
		if s.cond, err = p.cond(); err != nil {
			return nil, err
		}
	}
	if _, err := p.op(";"); err != nil {
		return nil, err
	}
	if !p.isOp(")") {
		if s.step, err = p.simpleStmt(); err != nil {
			return nil, err
		}
	}
	if _, err := p.op(")"); err != nil {
		return nil, err
	}
	if s.body, err = p.block(); err != nil {
		return nil, err
	}
	return s, nil
}

// lastEnd is the byte offset after the last token read.
func (p *parser) lastEnd() int {
	if p.at == 0 {
		return 0
	}
	return p.toks[p.at-1].end
}

// docBefore is the doc comment right above the token at pos: the comments that end just before it (with nothing
// but one line break between) and the comments right above those, without their markers.
func (p *parser) docBefore(pos int) string {
	i := sort.Search(len(p.comments), func(i int) bool { return p.comments[i].end > pos }) - 1
	var lines []string
	for next := pos; i >= 0; i-- {
		c := p.comments[i]
		gap := p.src[c.end:next]
		if strings.TrimSpace(gap) != "" || strings.Count(gap, "\n") > 1 {
			break
		}
		// A comment at the end of a line of code belongs to that code, not to what follows
		lineStart := strings.LastIndexByte(p.src[:c.start], '\n') + 1
		if strings.TrimSpace(p.src[lineStart:c.start]) != "" {
			break
		}
		lines = append([]string{commentText(c.text)}, lines...)
		next = c.start
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// commentText strips a comment's markers: //, /*, */, /** and the * that starts a line of a block comment.
func commentText(c string) string {
	if strings.HasPrefix(c, "//") {
		return strings.TrimSpace(strings.TrimPrefix(c, "//"))
	}
	c = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c, "/*"), "*"), "*/")
	lines := strings.Split(c, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		lines[i] = strings.TrimSpace(strings.TrimPrefix(l, "*"))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
