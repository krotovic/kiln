package language

import (
	"fmt"
	"sort"
	"strings"
)

// Tools (a language server, an editor) ask the checker what it found: Analyze checks a script the way
// CompileScript does, but keeps going past errors and records every name it resolved, with its type, doc
// comment and definition. See docs/language.md ("Tooling").

// Location is a range of a module's source, in byte offsets.
type Location struct {
	Module     string `json:"module"`
	Start, End int
}

// RefKind says what a name is.
type RefKind string

const (
	RefLocal     RefKind = "local"
	RefParameter RefKind = "parameter"
	RefField     RefKind = "field"
	RefMethod    RefKind = "method"
	RefFunction  RefKind = "function"
	RefClass     RefKind = "class"
	RefInterface RefKind = "interface"
	RefType      RefKind = "type"
	RefConstant  RefKind = "constant"
	RefModule    RefKind = "module"
	RefBuiltin   RefKind = "builtin" // the standard library and the members of arrays, maps and strings
	RefKeyword   RefKind = "keyword" // completions only
)

// Param is one parameter of a signature.
type Param struct {
	Name string
	Type string
}

// Signature is what a call takes: a function's, a method's, or a class's constructor.
type Signature struct {
	Label  string // name(a: number, b: string): string
	Params []Param
	Result string
	Doc    string
}

// Ref is one occurrence of a name in a module's source.
type Ref struct {
	Location
	Name   string
	Kind   RefKind
	Detail string // its type or signature, as hover shows it
	Doc    string
	// Def is where it is declared: a script module, or a host module's definition file; nil for the standard
	// library and the members of arrays, maps and strings
	Def *Location
	// Decl marks the occurrence that declares it
	Decl bool
	// Signature is what calling it takes, for functions, methods and classes
	Signature *Signature
}

// Symbol is a declaration of a module's outline.
type Symbol struct {
	Name     string
	Kind     RefKind
	Detail   string
	Range    Location // the whole declaration
	NameAt   Location // its name
	Children []Symbol
}

// Analysis is what Analyze found in a script's modules.
type Analysis struct {
	Diagnostics []Diagnostic
	// Modules lists the modules it read (not host modules), dependencies first
	Modules []string
	refs    map[string][]Ref
	symbols map[string][]Symbol
	classes []*classInfo // the scripts' classes and interfaces, for RenameConflict
	ifaces  []*ifaceInfo
}

// Refs are a module's name occurrences, by position.
func (a *Analysis) Refs(module string) []Ref { return a.refs[module] }

// Symbols is a module's outline.
func (a *Analysis) Symbols(module string) []Symbol { return a.symbols[module] }

// At finds the name at offset in module (the cursor touching it, at its end too).
func (a *Analysis) At(module string, offset int) (Ref, bool) {
	refs := a.refs[module]
	i := sort.Search(len(refs), func(i int) bool { return refs[i].End >= offset })
	for ; i < len(refs) && refs[i].Start <= offset; i++ {
		if offset <= refs[i].End {
			return refs[i], true
		}
	}
	return Ref{}, false
}

// References lists every occurrence of what is declared at def, in every module analysed, by module and
// position.
func (a *Analysis) References(def Location) []Ref {
	var out []Ref
	mods := make([]string, 0, len(a.refs))
	for m := range a.refs {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		for _, r := range a.refs[m] {
			if r.Def != nil && *r.Def == def {
				out = append(out, r)
			}
		}
	}
	return out
}

// Analyze loads entry and what it imports from src and checks it all, recording names, types and definitions.
// Unlike CompileScript it goes on past errors: a module that doesn't parse is left out (what imports it sees its
// names without types), and the others are checked as far as they go.
func Analyze(entry string, src Source, opts ScriptOptions) *Analysis {
	return analyze(entry, src, opts, nil)
}

func analyze(entry string, src Source, opts ScriptOptions, probe *probe) *Analysis {
	c := newChecker(opts)
	c.rec = &recorder{refs: map[string][]Ref{}, probe: probe}
	if h := c.hosts[entry]; h != nil {
		// A host module's definition file, read on its own: checked afresh so its names are recorded too
		m := newModule(h.Name, h.Source)
		m.host = h
		c.rec.host = m
		if ast, err := parseModule(h.Source, c.limits, true); err != nil {
			c.compileError(m, err)
		} else {
			m.ast = ast
			c.modules[h.Name] = m
			c.order = append(c.order, m)
		}
	} else {
		c.loadAll(entry, src)
	}
	for _, m := range c.order {
		c.checkModuleSafely(m)
	}
	a := &Analysis{Diagnostics: c.diags, refs: map[string][]Ref{}, symbols: map[string][]Symbol{}}
	for _, m := range c.order {
		a.Modules = append(a.Modules, m.id)
		a.refs[m.id] = c.rec.sorted(m.id)
		a.symbols[m.id] = symbolsOf(m)
		for _, d := range m.ast.decls {
			switch {
			case d.sym == nil:
			case d.class != nil:
				a.classes = append(a.classes, d.sym.class)
			case d.iface != nil:
				a.ifaces = append(a.ifaces, d.sym.iface)
			}
		}
	}
	return a
}

// RenameConflict says why the field or method declared at def can't be renamed on its own, or "" when it can:
// it overrides a base class's member or is overridden in a subclass, it implements a member of an interface its
// class names in implements (the host's Behaviour too, whose methods the host calls by name), or, for an
// interface's member, a class implementing the interface has it. Renaming it would rename one of them and break
// the others. Classes that satisfy an interface without naming it aren't seen.
func (a *Analysis) RenameConflict(def Location) string {
	for _, ci := range a.classes {
		name := ownMemberAt(ci, def)
		if name == "" {
			continue
		}
		if s := ci.super; s != nil && (s.fieldByName[name] != nil || s.methods[name] != nil) {
			return fmt.Sprintf("it overrides %s.%s", memberOwner(s, name), name)
		}
		for _, k := range a.classes {
			if k != ci && k.isA(ci) && ownMember(k, name) {
				return fmt.Sprintf("%s overrides it", k.name)
			}
		}
		for k := ci; k != nil; k = k.super {
			for _, it := range k.implements {
				if it.fields[name] != nil || it.methods[name] != nil {
					return fmt.Sprintf("it implements %s.%s", it.name, name)
				}
			}
		}
	}
	for _, ii := range a.ifaces {
		name := ifaceMemberAt(ii, def)
		if name == "" {
			continue
		}
		for _, k := range a.classes {
			if implementsIface(k, ii) && (k.fieldByName[name] != nil || k.methods[name] != nil) {
				return fmt.Sprintf("%s implements it", k.name)
			}
		}
		for _, jj := range a.ifaces {
			if jj != ii && jj.extends(ii) {
				return fmt.Sprintf("%s extends %s", jj.name, ii.name)
			}
		}
	}
	return ""
}

func ownMember(ci *classInfo, name string) bool {
	if f := ci.fieldByName[name]; f != nil && f.owner == ci {
		return true
	}
	f := ci.methods[name]
	return f != nil && f.owner == ci
}

func memberOwner(ci *classInfo, name string) string {
	if f := ci.fieldByName[name]; f != nil {
		return f.owner.name
	}
	return ci.methods[name].owner.name
}

// ownMemberAt is the name of ci's own field or method declared at def, or "".
func ownMemberAt(ci *classInfo, def Location) string {
	at := func(pos int, name string) bool {
		l := loc(ci.mod, pos, name)
		return l != nil && *l == def
	}
	for _, f := range ci.fields {
		if f.owner == ci && at(f.pos, f.name) {
			return f.name
		}
	}
	for _, f := range ci.methods {
		if f.owner == ci && at(f.pos, f.name) {
			return f.name
		}
	}
	return ""
}

func ifaceMemberAt(ii *ifaceInfo, def Location) string {
	if ii.decl == nil {
		return ""
	}
	at := func(pos int, name string) bool {
		l := loc(ii.mod, pos, name)
		return l != nil && *l == def
	}
	for _, f := range ii.decl.fields {
		if at(f.pos, f.name) {
			return f.name
		}
	}
	for _, f := range ii.decl.methods {
		if at(f.pos, f.name) {
			return f.name
		}
	}
	return ""
}

// implementsIface reports whether a class (or a base of it) names ii, or an interface extending it, in implements.
func implementsIface(ci *classInfo, ii *ifaceInfo) bool {
	for k := ci; k != nil; k = k.super {
		for _, it := range k.implements {
			if it.extends(ii) {
				return true
			}
		}
	}
	return false
}

// checkModuleSafely checks a module for a tool, turning a crash on broken code into a diagnostic rather than
// taking the tool down.
func (c *checker) checkModuleSafely(m *moduleInfo) {
	defer func() {
		if r := recover(); r != nil {
			c.errorf(m, 0, "the checker failed here: %v", r)
		}
	}()
	c.checkModule(m)
}

// recorder collects what the checker resolves, for Analyze.
type recorder struct {
	refs  map[string][]Ref
	probe *probe
	// host is the host module being analysed itself, whose names are recorded (others' aren't: no script is in them)
	host *moduleInfo
}

func (r *recorder) add(m *moduleInfo, pos int, name string, ref Ref) {
	if m == nil || (m.host != nil && m != r.host) || pos < 0 {
		return
	}
	ref.Location = Location{Module: m.id, Start: pos, End: pos + len(name)}
	ref.Name = name
	r.refs[m.id] = append(r.refs[m.id], ref)
}

// sorted is a module's refs by position, each position once (a type may be resolved more than once).
func (r *recorder) sorted(module string) []Ref {
	refs := r.refs[module]
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Start < refs[j].Start })
	out := refs[:0]
	for i, ref := range refs {
		if i > 0 && ref.Start == refs[i-1].Start {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// loc is where a name is declared: in a script module, or in a host module's definition file (its Source).
func loc(m *moduleInfo, pos int, name string) *Location {
	if m == nil {
		return nil
	}
	return &Location{Module: m.id, Start: pos, End: pos + len(name)}
}

func signatureOf(name string, f *funcInfo, doc string) *Signature {
	s := &Signature{Label: name + f.signature(), Result: f.result.String(), Doc: doc}
	for i, p := range f.params {
		s.Params = append(s.Params, Param{Name: f.names[i], Type: p.String()})
	}
	return s
}

func funcDoc(f *funcInfo) string {
	if f.decl != nil {
		return f.decl.doc
	}
	return ""
}

func fieldDoc(f *fieldInfo) string {
	if f.decl != nil {
		return f.decl.doc
	}
	return ""
}

// The refs of each kind of name.

func classRef(ci *classInfo, decl bool) Ref {
	detail := "class " + ci.name
	if ci.super != nil {
		detail += " extends " + ci.super.name
	}
	ref := Ref{Kind: RefClass, Detail: detail, Def: loc(ci.mod, ci.pos, ci.name), Decl: decl}
	if ci.decl != nil {
		ref.Doc = ci.decl.doc
	}
	if ci.ctor != nil && !ci.native {
		doc := funcDoc(ci.ctor)
		ref.Signature = signatureOf("new "+ci.name, &funcInfo{params: ci.ctor.params, names: ci.ctor.names, result: ci.typ}, doc)
	}
	return ref
}

func ifaceRef(ii *ifaceInfo, decl bool) Ref {
	ref := Ref{Kind: RefInterface, Detail: "interface " + ii.name, Def: loc(ii.mod, ii.pos, ii.name), Decl: decl}
	if ii.decl != nil {
		ref.Doc = ii.decl.doc
	}
	return ref
}

func fieldRef(owner string, f *fieldInfo, decl bool) Ref {
	kind := "field"
	if f.readonly {
		kind = "readonly field"
	}
	return Ref{Kind: RefField, Detail: fmt.Sprintf("(%s) %s.%s: %s", kind, owner, f.name, f.typ), Doc: fieldDoc(f),
		Def: loc(f.mod, f.pos, f.name), Decl: decl}
}

func methodRef(owner string, f *funcInfo, decl bool) Ref {
	doc := funcDoc(f)
	return Ref{Kind: RefMethod, Detail: fmt.Sprintf("(method) %s.%s%s", owner, f.name, f.signature()), Doc: doc,
		Def: loc(f.mod, f.pos, f.name), Decl: decl, Signature: signatureOf(f.name, f, doc)}
}

func funcRef(f *funcInfo, decl bool) Ref {
	doc := funcDoc(f)
	return Ref{Kind: RefFunction, Detail: "function " + f.name + f.signature(), Doc: doc, Def: loc(f.mod, f.pos, f.name),
		Decl: decl, Signature: signatureOf(f.name, f, doc)}
}

// symbolRef is the ref of a module symbol used (or declared) under name, which differs from its own for an
// import with as: the alias then counts as declared where it is imported (aliasAt).
func (c *checker) symbolRef(sym *symbol, decl bool, aliasAt *Location) Ref {
	var ref Ref
	switch sym.kind {
	case symClass:
		ref = classRef(sym.class, decl)
	case symIface:
		ref = ifaceRef(sym.iface, decl)
	case symAlias:
		t := "?"
		if sym.alias.typ != nil {
			t = sym.alias.typ.String()
		}
		ref = Ref{Kind: RefType, Detail: "type " + sym.name + " = " + t, Doc: sym.alias.decl.doc, Def: loc(sym.mod, sym.pos, sym.name), Decl: decl}
	case symFunc:
		if sym.fn.result == nil {
			return Ref{Kind: RefFunction, Detail: "function " + sym.name, Def: loc(sym.mod, sym.pos, sym.name), Decl: decl}
		}
		ref = funcRef(sym.fn, decl)
	case symConst:
		t := "?"
		if sym.konst.typ != nil {
			t = sym.konst.typ.String()
		}
		ref = Ref{Kind: RefConstant, Detail: "const " + sym.name + ": " + t, Doc: sym.konst.decl.doc, Def: loc(sym.mod, sym.pos, sym.name), Decl: decl}
	default:
		ref = Ref{Kind: RefType, Detail: sym.name}
	}
	if aliasAt != nil {
		ref.Def = aliasAt
	}
	return ref
}

// aliasLoc is where a module imported a symbol under another name, or nil when name is its own.
func aliasLoc(m *moduleInfo, sym *symbol, name string) *Location {
	if sym.name == name {
		return nil
	}
	if at, ok := m.aliases[name]; ok {
		return &at
	}
	return nil
}

func localRef(l *local, name string, m *moduleInfo, decl bool) Ref {
	kind, detail := RefLocal, "let "+name+": "+l.typ.String()
	switch {
	case l.param:
		kind, detail = RefParameter, "(parameter) "+name+": "+l.typ.String()
	case l.constant:
		detail = "const " + name + ": " + l.typ.String()
	}
	return Ref{Kind: kind, Detail: detail, Def: loc(m, l.pos, name), Decl: decl}
}

func stdRef(name string, sig *stdSig) Ref {
	params := make([]Param, len(sig.params))
	parts := make([]string, len(sig.params))
	for i, p := range sig.params {
		t := "number | string | bool"
		if p != nil {
			t = p.String()
		}
		pname := string(rune('a' + i))
		params[i] = Param{Name: pname, Type: t}
		parts[i] = pname + ": " + t
		if sig.variadic && i == len(sig.params)-1 {
			parts[i] += ", …"
		}
	}
	label := name + "(" + strings.Join(parts, ", ") + "): " + sig.result.String()
	return Ref{Kind: RefBuiltin, Detail: "function " + label, Signature: &Signature{Label: label, Params: params, Result: sig.result.String()}}
}

func builtinMethodRef(recv *Type, name string, bm *builtinMethod) Ref {
	params := bm.params(recv)
	parts := make([]string, len(params))
	ps := make([]Param, len(params))
	for i, p := range params {
		pname := string(rune('a' + i))
		ps[i] = Param{Name: pname, Type: p.String()}
		parts[i] = pname
		if i >= bm.minArgs {
			parts[i] += "?"
		}
		parts[i] += ": " + p.String()
		if bm.maxArgs < 0 && i == len(params)-1 {
			parts[i] += ", …"
		}
	}
	label := name + "(" + strings.Join(parts, ", ") + "): " + bm.result(recv).String()
	return Ref{Kind: RefBuiltin, Detail: "(method) " + recv.String() + "." + label,
		Signature: &Signature{Label: label, Params: ps, Result: bm.result(recv).String()}}
}

// symbolsOf is a module's outline, from its declarations.
func symbolsOf(m *moduleInfo) []Symbol {
	var out []Symbol
	at := func(pos, end int) Location { return Location{Module: m.id, Start: pos, End: end} }
	for _, d := range m.ast.decls {
		name, pos := d.name()
		s := Symbol{Name: name, Range: at(d.start, d.end), NameAt: at(pos, pos+len(name))}
		switch {
		case d.class != nil:
			s.Kind, s.Detail = RefClass, "class"
			if d.class.ctor != nil {
				s.Children = append(s.Children, Symbol{Name: "constructor", Kind: RefMethod, Range: at(d.class.ctor.pos, d.class.ctor.end),
					NameAt: at(d.class.ctor.pos, d.class.ctor.pos+len("constructor"))})
			}
			for _, f := range d.class.fields {
				s.Children = append(s.Children, Symbol{Name: f.name, Kind: RefField, Range: at(f.pos, max(f.end, f.pos+len(f.name))), NameAt: at(f.pos, f.pos+len(f.name))})
			}
			for _, f := range d.class.methods {
				s.Children = append(s.Children, Symbol{Name: f.name, Kind: RefMethod, Range: at(f.pos, max(f.end, f.pos+len(f.name))), NameAt: at(f.pos, f.pos+len(f.name))})
			}
		case d.iface != nil:
			s.Kind, s.Detail = RefInterface, "interface"
			for _, f := range d.iface.fields {
				s.Children = append(s.Children, Symbol{Name: f.name, Kind: RefField, Range: at(f.pos, max(f.end, f.pos+len(f.name))), NameAt: at(f.pos, f.pos+len(f.name))})
			}
			for _, f := range d.iface.methods {
				s.Children = append(s.Children, Symbol{Name: f.name, Kind: RefMethod, Range: at(f.pos, max(f.end, f.pos+len(f.name))), NameAt: at(f.pos, f.pos+len(f.name))})
			}
		case d.alias != nil:
			s.Kind, s.Detail = RefType, "type"
		case d.fn != nil:
			s.Kind, s.Detail = RefFunction, "function"
		case d.konst != nil:
			s.Kind, s.Detail = RefConstant, "const"
		}
		sort.SliceStable(s.Children, func(i, j int) bool { return s.Children[i].Range.Start < s.Children[j].Range.Start })
		out = append(out, s)
	}
	return out
}
