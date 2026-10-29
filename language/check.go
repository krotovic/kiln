package language

import (
	"errors"
	"strings"
)

// checker loads a script's modules and checks them, turning each function into IR (ir.go).
type checker struct {
	limits  ScriptLimits
	hosts   map[string]*HostModule
	modules map[string]*moduleInfo
	order   []*moduleInfo // dependencies first
	diags   []Diagnostic
	rec     *recorder // what Analyze records; nil for CompileScript
}

type symKind uint8

const (
	symClass symKind = iota + 1
	symIface
	symAlias
	symFunc
	symConst
	symInvalid // imported from a module that failed to load: anything goes, silently
)

// symbol is a name declared at the top of a module, or imported into it.
type symbol struct {
	kind  symKind
	name  string
	mod   *moduleInfo // where it is declared
	pos   int
	class *classInfo
	iface *ifaceInfo
	alias *aliasInfo
	fn    *funcInfo
	konst *constInfo
}

type aliasInfo struct {
	decl  *aliasDecl
	typ   *Type
	state uint8
}

// constInfo is a module constant: a number, string or bool computed once when the script compiles.
type constInfo struct {
	decl *constDecl
	typ  *Type
	slot int
	init irExpr
	mod  *moduleInfo
}

func newChecker(opts ScriptOptions) *checker {
	limits := opts.Limits
	if limits == (ScriptLimits{}) {
		limits = DefaultScriptLimits
	}
	c := &checker{limits: limits, hosts: map[string]*HostModule{}, modules: map[string]*moduleInfo{}}
	for _, h := range opts.Hosts {
		c.hosts[h.Name] = h
	}
	return c
}

func (c *checker) errorf(m *moduleInfo, pos int, format string, args ...any) {
	if len(c.diags) >= maxDiagnostics {
		return
	}
	line, col := m.lineCol(pos)
	c.diags = append(c.diags, Diagnostic{Module: m.id, Line: line, Col: col, Msg: sprintf(format, args...)})
}

// compileError reports a parse error, whose Pos is the 1-based byte offset.
func (c *checker) compileError(m *moduleInfo, err error) {
	var ce *Error
	if errors.As(err, &ce) {
		c.errorf(m, ce.Pos-1, "%s", ce.Msg)
		return
	}
	c.errorf(m, 0, "%v", err)
}

// loadAll loads entry and everything it imports, depth first, so c.order lists every module after the ones it
// imports. Import cycles are refused.
func (c *checker) loadAll(entry string, src Source) {
	state := map[string]uint8{} // 1 loading, 2 loaded
	var visit func(id string, from *moduleInfo, pos int, path []string)
	visit = func(id string, from *moduleInfo, pos int, path []string) {
		if h, ok := c.hosts[id]; ok {
			if _, done := c.modules[id]; done {
				return
			}
			m, diags := h.module()
			c.diags = append(c.diags, diags...)
			if m != nil {
				c.modules[id] = m
			}
			return
		}
		switch state[id] {
		case 1:
			cycle := append(path[indexOf(path, id):], id)
			c.errorf(from, pos, "import cycle: %s", strings.Join(cycle, " → "))
			return
		case 2:
			return
		}
		if len(state) >= c.limits.MaxModules {
			c.errorf(from, pos, "more than %d modules", c.limits.MaxModules)
			return
		}
		state[id] = 1
		defer func() { state[id] = 2 }()
		text, err := src.Load(id)
		if err != nil {
			if from == nil {
				c.diags = append(c.diags, Diagnostic{Module: id, Line: 1, Col: 1, Msg: err.Error()})
			} else {
				c.errorf(from, pos, "can't import %q: %v", id, err)
			}
			return
		}
		m := newModule(id, text)
		ast, err := parseModule(text, c.limits, false)
		if err != nil {
			c.compileError(m, err)
			return
		}
		m.ast = ast
		c.modules[id] = m
		for _, imp := range ast.imports {
			m.imports = append(m.imports, imp.from)
			visit(imp.from, m, imp.pos, append(path, id))
		}
		c.order = append(c.order, m)
	}
	visit(entry, nil, 0, nil)
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return 0
}

// checkModule checks one module whose imports are checked already.
func (c *checker) checkModule(m *moduleInfo) {
	c.bindImports(m)
	c.declare(m)
	for _, d := range m.ast.decls {
		switch {
		case d.sym == nil:
		case d.alias != nil:
			c.resolveAlias(d.sym.alias, m)
		case d.iface != nil:
			c.resolveIface(d.sym.iface)
		case d.class != nil:
			c.resolveClass(d.sym.class)
		}
	}
	for _, d := range m.ast.decls {
		if d.sym != nil && d.fn != nil {
			c.resolveSig(d.sym.fn, d.fn, m)
		}
	}
	for _, d := range m.ast.decls {
		if d.sym != nil && d.konst != nil {
			c.checkConst(d.sym.konst)
		}
	}
	for _, d := range m.ast.decls {
		switch {
		case d.sym == nil:
		case d.fn != nil && !d.native:
			c.compileFunc(d.sym.fn, d.fn, nil)
		case d.class != nil:
			c.compileClass(d.sym.class)
		}
	}
	if c.rec != nil {
		c.recordDecls(m)
	}
}

// recordDecls records where m's declarations and their members are declared, once they are resolved.
func (c *checker) recordDecls(m *moduleInfo) {
	for _, d := range m.ast.decls {
		if d.sym == nil {
			continue
		}
		name, pos := d.name()
		c.rec.add(m, pos, name, c.symbolRef(d.sym, true, nil))
		switch {
		case d.class != nil:
			ci := d.sym.class
			for _, f := range d.class.fields {
				if fi := ci.fieldByName[f.name]; fi != nil && fi.owner == ci {
					c.rec.add(m, f.pos, f.name, fieldRef(ci.name, fi, true))
				}
			}
			for _, md := range d.class.methods {
				if fn := ci.methods[md.name]; fn != nil && fn.owner == ci {
					c.rec.add(m, md.pos, md.name, methodRef(ci.name, fn, true))
				}
			}
		case d.iface != nil:
			ii := d.sym.iface
			for _, f := range d.iface.fields {
				if fi := ii.fields[f.name]; fi != nil {
					c.rec.add(m, f.pos, f.name, fieldRef(ii.name, fi, true))
				}
			}
			for _, md := range d.iface.methods {
				if fn := ii.methods[md.name]; fn != nil {
					c.rec.add(m, md.pos, md.name, methodRef(ii.name, fn, true))
				}
			}
		}
	}
}

func (c *checker) bindImports(m *moduleInfo) {
	for _, imp := range m.ast.imports {
		target := c.modules[imp.from]
		if c.rec != nil {
			ref := Ref{Kind: RefModule, Detail: "module \"" + imp.from + "\""}
			if target != nil {
				ref.Def = &Location{Module: imp.from}
			}
			c.rec.add(m, imp.fromPos, `"`+imp.from+`"`, ref)
		}
		for _, n := range imp.names {
			if target == nil {
				// Its loading failed, and said why; its names go unchecked rather than unknown
				if _, dup := m.symbols[n.alias]; !dup {
					m.symbols[n.alias] = &symbol{kind: symInvalid, name: n.alias, mod: m, pos: n.pos}
				}
				continue
			}
			sym := target.exports[n.name]
			if sym == nil {
				c.errorf(m, n.pos, "%s doesn't export %s", imp.from, n.name)
				continue
			}
			if _, dup := m.symbols[n.alias]; dup {
				c.errorf(m, n.pos, "%s is imported twice", n.alias)
				continue
			}
			m.symbols[n.alias] = sym
			if n.alias != sym.name || n.namePos != n.pos {
				if m.aliases == nil {
					m.aliases = map[string]Location{}
				}
				m.aliases[n.alias] = Location{Module: m.id, Start: n.pos, End: n.pos + len(n.alias)}
			}
			if c.rec != nil {
				if n.namePos >= 0 {
					c.rec.add(m, n.namePos, n.name, c.symbolRef(sym, false, nil))
				}
				if n.namePos != n.pos {
					c.rec.add(m, n.pos, n.alias, c.symbolRef(sym, true, aliasLoc(m, sym, n.alias)))
				}
			}
		}
	}
}

// declare makes a symbol for each declaration of m, so declarations may refer to each other in any order.
func (c *checker) declare(m *moduleInfo) {
	for _, d := range m.ast.decls {
		name, pos := d.name()
		if _, dup := m.symbols[name]; dup || builtinNames[name] {
			c.errorf(m, pos, "%s is already declared", name)
			continue
		}
		sym := &symbol{name: name, mod: m, pos: pos}
		switch {
		case d.class != nil:
			ci := &classInfo{name: name, mod: m, pos: pos, decl: d.class, native: d.class.native, isError: d.class.isError,
				fieldByName: map[string]*fieldInfo{}, methods: map[string]*funcInfo{}}
			ci.typ = &Type{kind: tClass, class: ci}
			sym.kind, sym.class = symClass, ci
		case d.iface != nil:
			ii := &ifaceInfo{name: name, mod: m, pos: pos, decl: d.iface,
				fields: map[string]*fieldInfo{}, methods: map[string]*funcInfo{}}
			ii.typ = &Type{kind: tIface, iface: ii}
			sym.kind, sym.iface = symIface, ii
		case d.alias != nil:
			sym.kind, sym.alias = symAlias, &aliasInfo{decl: d.alias}
		case d.fn != nil:
			fn := &funcInfo{name: name, mod: m, pos: pos, decl: d.fn}
			if d.native {
				if fn.native = c.native(m, name, pos); fn.native == nil {
					continue
				}
			}
			sym.kind, sym.fn = symFunc, fn
		case d.konst != nil:
			sym.kind, sym.konst = symConst, &constInfo{decl: d.konst, slot: len(m.consts), mod: m}
			m.consts = append(m.consts, sym.konst)
		}
		m.symbols[name] = sym
		d.sym = sym
		if d.exported {
			key := name
			if d.isDefault {
				key = "default"
			}
			m.exports[key] = sym
		}
	}
}

// native finds the host's code for a native declaration of a host module.
func (c *checker) native(m *moduleInfo, name string, pos int) NativeFunc {
	if m.host == nil {
		return nil
	}
	fn := m.host.Natives[name]
	if fn == nil {
		c.errorf(m, pos, "the host gives no code for the native %s", name)
	}
	return fn
}

// resolveType turns a written type into a Type, in the module it was written in.
func (c *checker) resolveType(m *moduleInfo, te *typeExpr) *Type {
	if c.rec != nil && c.rec.probe != nil && te.name == probeName {
		c.rec.probe.types(m)
		return typInvalid
	}
	var t *Type
	switch te.name {
	case "number":
		t = typNumber
	case "string":
		t = typString
	case "bool", "boolean":
		t = typBool
	case "void":
		c.errorf(m, te.pos, "void is only what a function returns")
		return typInvalid
	case "Map":
		if len(te.args) != 2 {
			c.errorf(m, te.pos, "Map takes two types: Map<key, value>")
			return typInvalid
		}
		k := c.resolveType(m, te.args[0])
		if k.kind != tNumber && k.kind != tString && k.kind != tInvalid {
			c.errorf(m, te.args[0].pos, "a map's keys are numbers or strings, not %s", k)
			k = typInvalid
		}
		t = mapOf(k, c.resolveType(m, te.args[1]))
	case "Array":
		if len(te.args) != 1 {
			c.errorf(m, te.pos, "Array takes one type: Array<element>")
			return typInvalid
		}
		t = arrayOf(c.resolveType(m, te.args[0]))
	case "Promise":
		t = c.resolvePromise(m, te)
	default:
		sym := m.symbols[te.name]
		switch {
		case sym == nil:
			c.errorf(m, te.pos, "unknown type %s", te.name)
			return typInvalid
		case sym.kind == symInvalid:
			return typInvalid
		case sym.kind == symClass:
			t = sym.class.typ
		case sym.kind == symIface:
			t = sym.iface.typ
		case sym.kind == symAlias:
			t = c.resolveAlias(sym.alias, sym.mod)
		default:
			c.errorf(m, te.pos, "%s is not a type", te.name)
			return typInvalid
		}
		if c.rec != nil {
			c.rec.add(m, te.pos, te.name, c.symbolRef(sym, false, aliasLoc(m, sym, te.name)))
		}
	}
	if len(te.args) > 0 && te.name != "Map" && te.name != "Array" && te.name != "Promise" {
		c.errorf(m, te.pos, "%s takes no type arguments", te.name)
	}
	for range te.dims {
		t = arrayOf(t)
	}
	return t
}

// resolvePromise resolves Promise<T> (a promise that can't fail) or Promise<T, E>, which fails with the error
// class E. T may be void; it can't be an error class, so what an async function returns says which it does.
func (c *checker) resolvePromise(m *moduleInfo, te *typeExpr) *Type {
	if len(te.args) != 1 && len(te.args) != 2 {
		c.errorf(m, te.pos, "Promise takes its value's type, and the error class it may fail with: Promise<value, error>")
		return typInvalid
	}
	v := typVoid
	if a := te.args[0]; a.name != "void" || a.dims > 0 || len(a.args) > 0 {
		v = c.resolveType(m, a)
	}
	if v.isErrorClass() {
		c.errorf(m, te.args[0].pos, "%s is an error class: a promise fails with it, as Promise<value, %s>", v, v)
		v = typInvalid
	}
	var e *Type
	if len(te.args) == 2 {
		e = c.resolveType(m, te.args[1])
		if e.kind != tInvalid && !e.isErrorClass() {
			c.errorf(m, te.args[1].pos, "a promise fails with an error class (error class %s { … }), not %s", e, e)
			e = typInvalid
		}
	}
	return promiseOf(v, e)
}

func (c *checker) resolveAlias(a *aliasInfo, m *moduleInfo) *Type {
	switch a.state {
	case 1:
		c.errorf(m, a.decl.pos, "the type %s refers to itself", a.decl.name)
		a.typ = typInvalid
		return a.typ
	case 2:
		return a.typ
	}
	a.state = 1
	t := c.resolveType(m, a.decl.typ)
	if a.state == 1 {
		a.typ, a.state = t, 2
	}
	return a.typ
}

// resolveSig sets fn's parameter and result types from its declaration.
func (c *checker) resolveSig(fn *funcInfo, d *funcDecl, m *moduleInfo) {
	seen := map[string]bool{}
	fn.params, fn.names = nil, nil
	for _, p := range d.params {
		if seen[p.name] {
			c.errorf(m, p.pos, "the parameter %s is declared twice", p.name)
		}
		seen[p.name] = true
		fn.params = append(fn.params, c.resolveType(m, p.typ))
		fn.names = append(fn.names, p.name)
	}
	fn.result = typVoid
	if d.result != nil {
		fn.result = c.resolveType(m, d.result)
	}
	fn.private, fn.optional, fn.async = d.private, d.optional, d.async
	if fn.async && fn.result.kind != tPromise && fn.result.kind != tInvalid {
		pos := d.pos
		if d.result != nil {
			pos = d.result.pos
		}
		c.errorf(m, pos, "an async function returns a Promise<value> or a Promise<value, error>, not %s", fn.result)
		fn.result = typInvalid
	}
	if m.host != nil {
		for i, p := range fn.params {
			if p.kind == tPromise {
				c.errorf(m, d.params[i].pos, "a native takes no promise: only scripts await them")
			}
		}
	}
}

func (c *checker) resolveIface(ii *ifaceInfo) {
	m := ii.mod
	switch ii.state {
	case 1:
		c.errorf(m, ii.pos, "the interface %s extends itself", ii.name)
		return
	case 2:
		return
	}
	ii.state = 1
	defer func() { ii.state = 2 }()
	add := func(name string, pos int, f *fieldInfo, fn *funcInfo) {
		if old, ok := ii.methods[name]; ok {
			if fn == nil || !old.sameSignature(fn) || old.optional != fn.optional {
				c.errorf(m, pos, "%s has two different members %s", ii.name, name)
			}
			return
		}
		if old, ok := ii.fields[name]; ok {
			if f == nil || !identical(old.typ, f.typ) || old.readonly != f.readonly {
				c.errorf(m, pos, "%s has two different members %s", ii.name, name)
			}
			return
		}
		if fn != nil {
			ii.methods[name] = fn
		} else {
			ii.fields[name] = f
		}
		ii.order = append(ii.order, name)
	}
	for _, e := range ii.decl.extends {
		t := c.resolveType(m, e)
		if t.kind == tInvalid {
			continue
		}
		if t.kind != tIface {
			c.errorf(m, e.pos, "an interface extends interfaces, and %s is not one", t)
			continue
		}
		c.resolveIface(t.iface)
		ii.bases = append(ii.bases, t.iface)
		for _, name := range t.iface.order {
			add(name, e.pos, t.iface.fields[name], t.iface.methods[name])
		}
	}
	own := map[string]bool{}
	for _, f := range ii.decl.fields {
		if own[f.name] {
			c.errorf(m, f.pos, "%s is declared twice", f.name)
		}
		own[f.name] = true
		add(f.name, f.pos, &fieldInfo{name: f.name, pos: f.pos, typ: c.resolveType(m, f.typ), readonly: f.readonly, decl: f, mod: m}, nil)
	}
	for _, d := range ii.decl.methods {
		if own[d.name] {
			c.errorf(m, d.pos, "%s is declared twice", d.name)
		}
		own[d.name] = true
		fn := &funcInfo{name: d.name, mod: m, pos: d.pos, decl: d}
		c.resolveSig(fn, d, m)
		add(d.name, d.pos, nil, fn)
	}
}

func (c *checker) resolveClass(ci *classInfo) {
	m, d := ci.mod, ci.decl
	switch ci.state {
	case 1:
		c.errorf(m, ci.pos, "the class %s extends itself", ci.name)
		return
	case 2:
		return
	}
	ci.state = 1
	defer func() { ci.state = 2 }()
	if d.extends != nil {
		t := c.resolveType(m, d.extends)
		switch {
		case t.kind == tInvalid:
		case t.kind != tClass:
			c.errorf(m, d.extends.pos, "a class extends a class, and %s is not one", t)
		case t.class.native:
			c.errorf(m, d.extends.pos, "%s is the host's, and can't be extended", t)
		case ci.native:
			c.errorf(m, d.extends.pos, "a native class extends nothing")
		case t.class.isError && !ci.isError:
			c.errorf(m, d.extends.pos, "%s is an error class, so only an error class extends it: error class %s", t, ci.name)
		case !t.class.isError && ci.isError:
			c.errorf(m, d.extends.pos, "an error class extends only error classes, and %s is not one", t)
		default:
			c.resolveClass(t.class)
			if t.class.state == 2 || t.class.ctor != nil {
				ci.super = t.class
			}
		}
	}
	if s := ci.super; s != nil {
		ci.fields = append(ci.fields, s.fields...)
		for k, v := range s.fieldByName {
			ci.fieldByName[k] = v
		}
		for k, v := range s.methods {
			ci.methods[k] = v
		}
	}
	for _, f := range d.fields {
		if _, dup := ci.fieldByName[f.name]; dup {
			c.errorf(m, f.pos, "%s already has a field %s", ci.name, f.name)
			continue
		}
		fi := &fieldInfo{name: f.name, pos: f.pos, slot: len(ci.fields), owner: ci, private: f.private,
			readonly: f.readonly || ci.native, decl: f, mod: m}
		switch {
		case f.typ != nil:
			fi.typ = c.resolveType(m, f.typ)
			if ci.native && fi.typ.kind == tPromise {
				c.errorf(m, f.pos, "a native field holds no promise: a native method returns one")
			}
		default:
			fi.typ = literalType(f.init)
			if fi.typ == nil {
				c.errorf(m, f.pos, "the field %s needs a type: only a number, string or bool value says it", f.name)
				fi.typ = typInvalid
			}
		}
		if ci.native {
			if fi.getter = c.native(m, ci.name+"."+f.name, f.pos); fi.getter == nil {
				continue
			}
		}
		ci.fields = append(ci.fields, fi)
		ci.fieldByName[f.name] = fi
	}
	own := map[string]bool{}
	for _, md := range d.methods {
		if own[md.name] {
			c.errorf(m, md.pos, "%s already has a method %s", ci.name, md.name)
			continue
		}
		own[md.name] = true
		if _, isField := ci.fieldByName[md.name]; isField {
			c.errorf(m, md.pos, "%s already has a field %s", ci.name, md.name)
			continue
		}
		fn := &funcInfo{name: md.name, mod: m, pos: md.pos, decl: md, owner: ci}
		c.resolveSig(fn, md, m)
		if base := ci.methods[md.name]; base != nil {
			switch {
			case base.private || fn.private:
				c.errorf(m, md.pos, "%s overrides %s.%s, and one of them is private", md.name, base.owner.name, md.name)
			case !fn.sameSignature(base):
				c.errorf(m, md.pos, "%s overrides %s.%s%s with another signature", md.name, base.owner.name, md.name, base.signature())
			case fn.async != base.async && fn.result.kind != tInvalid:
				c.errorf(m, md.pos, "%s overrides %s.%s, and only one of them is async", md.name, base.owner.name, md.name)
			}
		}
		if ci.native {
			if fn.native = c.native(m, ci.name+"."+md.name, md.pos); fn.native == nil {
				continue
			}
		}
		ci.methods[md.name] = fn
	}
	ctor := &funcInfo{name: "constructor", mod: m, pos: ci.pos, owner: ci, result: typVoid}
	switch {
	case d.ctor != nil && ci.native:
		c.errorf(m, d.ctor.pos, "a native class has no constructor: the host makes its objects")
	case d.ctor != nil:
		ctor.pos, ctor.decl = d.ctor.pos, d.ctor
		c.resolveSig(ctor, d.ctor, m)
	case ci.super != nil:
		ctor.params, ctor.names = ci.super.ctor.params, ci.super.ctor.names
	}
	ci.ctor = ctor
	for _, it := range d.implements {
		t := c.resolveType(m, it)
		if t.kind == tInvalid {
			continue
		}
		if t.kind != tIface {
			c.errorf(m, it.pos, "a class implements interfaces, and %s is not one", t)
			continue
		}
		c.resolveIface(t.iface)
		ci.implements = append(ci.implements, t.iface)
	}
}

// literalType is the type a field's value says without a written type: a number, string or bool literal.
func literalType(n node) *Type {
	if u, ok := n.(*unaryNode); ok && u.op == "-" {
		n = u.x
	}
	if lit, ok := n.(*literalNode); ok {
		switch lit.v.kind {
		case Number:
			return typNumber
		case String:
			return typString
		case Bool:
			return typBool
		}
	}
	return nil
}

// checkConst checks a module constant's value and type.
func (c *checker) checkConst(k *constInfo) {
	fc := c.newFuncCtx(k.mod, nil, nil)
	var want *Type
	if k.decl.typ != nil {
		want = c.resolveType(k.mod, k.decl.typ)
	}
	init, t := fc.expr(k.decl.init, want)
	if want != nil {
		if !assignable(t, want) {
			c.errorf(k.mod, k.decl.init.position(), "%s is a %s, not a %s", k.decl.name, t, want)
		}
		t = want
	}
	if !t.primitive() && t.kind != tInvalid {
		c.errorf(k.mod, k.decl.pos, "a module constant is a number, string or bool, not %s (it is shared by every object)", t)
	}
	k.typ, k.init = t, init
}

// compileFunc checks a function's or method's body.
func (c *checker) compileFunc(fn *funcInfo, d *funcDecl, class *classInfo) {
	fc := c.newFuncCtx(fn.mod, class, fn)
	fc.push()
	for i, p := range d.params {
		fc.declareParam(p.name, fn.params[i], p.pos)
	}
	body, done := fc.stmts(d.body.stmts)
	result := fn.result
	if fn.async && result.kind == tPromise {
		result = result.elem
	}
	if result.kind != tVoid && result.kind != tInvalid && !done {
		c.errorf(fn.mod, d.pos, "%s doesn't return a value on every path", fn.name)
	}
	fn.body, fn.locals = body, fc.slots
}

// compileClass checks a class's field values, methods and constructor, and the interfaces it implements.
func (c *checker) compileClass(ci *classInfo) {
	m, d := ci.mod, ci.decl
	for _, it := range ci.implements {
		if why := ci.satisfies(it); why != "" {
			c.errorf(m, ci.pos, "%s doesn't implement %s: %s", ci.name, it.name, why)
		}
	}
	if ci.native {
		return
	}
	for _, f := range ci.fields {
		if f.owner != ci || f.decl.init == nil {
			continue
		}
		fc := c.newFuncCtx(m, ci, nil)
		fc.inInit = true
		init, t := fc.expr(f.decl.init, f.typ)
		if !assignable(t, f.typ) {
			c.errorf(m, f.decl.init.position(), "%s is a %s, not a %s", f.name, t, f.typ)
		}
		f.init = init
	}
	for _, md := range d.methods {
		if fn := ci.methods[md.name]; fn != nil && fn.owner == ci {
			c.compileFunc(fn, md, ci)
		}
	}
	ctor := ci.ctor
	inits := &irFieldInits{class: ci}
	if ctor.decl == nil {
		// The implicit constructor passes its arguments to the base class's, then sets the fields' values.
		ctor.locals = append([]*Type(nil), ctor.params...)
		body := &irBlock{}
		if ci.super != nil {
			args := make([]irExpr, len(ci.super.ctor.params))
			for i, p := range ci.super.ctor.params {
				args[i] = &irLocal{irBase{p}, i}
			}
			body.stmts = append(body.stmts, &irSuperCall{ctor: ci.super.ctor, args: args, pos: ci.pos})
		}
		body.stmts = append(body.stmts, inits)
		ctor.body = body
		return
	}
	fc := c.newFuncCtx(m, ci, ctor)
	fc.ctor = true
	fc.push()
	for i, p := range ctor.decl.params {
		fc.declareParam(p.name, ctor.params[i], p.pos)
	}
	stmts := ctor.decl.body.stmts
	body := &irBlock{}
	if ci.super != nil {
		if len(stmts) == 0 || !isSuperCall(stmts[0]) {
			c.errorf(m, ctor.pos, "the constructor of %s must call super(…) first", ci.name)
		} else {
			body.stmts = append(body.stmts, fc.superCall(stmts[0].(*exprStmt).x.(*superCallNode)))
			stmts = stmts[1:]
		}
	}
	body.stmts = append(body.stmts, inits)
	rest, _ := fc.stmts(stmts)
	body.stmts = append(body.stmts, rest)
	ctor.body, ctor.locals = body, fc.slots
}

func isSuperCall(s stmt) bool {
	e, ok := s.(*exprStmt)
	if !ok {
		return false
	}
	_, ok = e.x.(*superCallNode)
	return ok
}
