package language

import (
	"regexp"
	"sort"
	"strings"
)

// probeName is the name Complete and SignatureHelp put where the cursor is: the checker, meeting it, records
// what could go there instead of reporting it unknown.
const probeName = "__alo_probe__"

// Completion is one suggestion for the cursor's place.
type Completion struct {
	Label  string
	Kind   RefKind
	Detail string
	Doc    string
}

// Lister is a Source that can list its modules, for completing the names of modules to import.
type Lister interface {
	List() []string
}

func (m MapSource) List() []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// overlay is a Source with one module's text replaced.
type overlay struct {
	base Source
	id   string
	text string
}

func (o overlay) Load(id string) (string, error) {
	if id == o.id {
		return o.text, nil
	}
	return o.base.Load(id)
}

// probe collects the completions the checker finds at the probe.
type probe struct {
	items []Completion
	seen  map[string]bool
	hit   bool
}

func (p *probe) add(c Completion) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	if c.Label == probeName || p.seen[c.Label] {
		return
	}
	p.seen[c.Label] = true
	p.items = append(p.items, c)
}

func (p *probe) ref(label string, r Ref) {
	p.add(Completion{Label: label, Kind: r.Kind, Detail: r.Detail, Doc: r.Doc})
}

var (
	valueKeywords     = []string{"null", "true", "false", "new"}
	statementKeywords = []string{"let", "const", "if", "else", "while", "for", "return", "break", "continue"}
	topKeywords       = []string{"import", "export", "class", "interface", "type", "function", "const"}
)

// names lists what a name can be where the probe stands: the variables in scope (innermost first), the module's
// constants, functions and classes, the standard library, and keywords.
func (p *probe) names(fc *funcCtx) {
	p.hit = true
	for i := len(fc.scopes) - 1; i >= 0; i-- {
		names := make([]string, 0, len(fc.scopes[i]))
		for n := range fc.scopes[i] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p.ref(n, localRef(fc.scopes[i][n], n, fc.m, false))
		}
	}
	if fc.class != nil {
		p.add(Completion{Label: "this", Kind: RefKeyword, Detail: "this: " + fc.class.name})
	}
	for _, n := range sortedKeys(fc.m.symbols) {
		sym := fc.m.symbols[n]
		switch sym.kind {
		case symConst, symFunc, symClass:
			p.ref(n, fc.c.symbolRef(sym, false, nil))
		}
	}
	for _, n := range sortedKeys(stdSigs) {
		p.ref(n, stdRef(n, stdSigs[n]))
	}
	for _, k := range append(append([]string{}, valueKeywords...), statementKeywords...) {
		p.add(Completion{Label: k, Kind: RefKeyword})
	}
}

// members lists what follows a dot on a value of type t: the fields and methods code here may use.
func (p *probe) members(fc *funcCtx, t *Type) {
	p.hit = true
	switch t.kind {
	case tClass:
		ci := t.class
		for _, n := range sortedKeys(ci.fieldByName) {
			if f := ci.fieldByName[n]; fc.canSee(f.owner, f.private) {
				p.ref(n, fieldRef(f.owner.name, f, false))
			}
		}
		for _, n := range sortedKeys(ci.methods) {
			if f := ci.methods[n]; fc.canSee(f.owner, f.private) {
				p.ref(n, methodRef(f.owner.name, f, false))
			}
		}
	case tIface:
		ii := t.iface
		for _, n := range sortedKeys(ii.fields) {
			p.ref(n, fieldRef(ii.name, ii.fields[n], false))
		}
		for _, n := range sortedKeys(ii.methods) {
			if f := ii.methods[n]; !f.optional {
				p.ref(n, methodRef(ii.name, f, false))
			}
		}
	case tArray, tMap, tString:
		prop := builtinProps[t.kind]
		p.add(Completion{Label: prop.name, Kind: RefBuiltin, Detail: "(property) " + t.String() + "." + prop.name + ": number"})
		methods := builtinMethods[t.kind]
		for _, n := range sortedKeys(methods) {
			p.ref(n, builtinMethodRef(t, n, methods[n]))
		}
	}
}

// types lists the types a type can be: the built-in ones and the module's classes, interfaces and aliases.
func (p *probe) types(m *moduleInfo) {
	p.hit = true
	for _, t := range []string{"number", "string", "bool", "void", "Map", "Array"} {
		p.add(Completion{Label: t, Kind: RefType})
	}
	for _, n := range sortedKeys(m.symbols) {
		sym := m.symbols[n]
		switch sym.kind {
		case symClass, symIface, symAlias:
			p.ref(n, (&checker{}).symbolRef(sym, false, nil))
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	// importBraces matches an import's { } the cursor stands in; importAfter finds its module after the cursor
	importBraces = regexp.MustCompile(`import\s+(?:[A-Za-z_]\w*\s*,\s*)?\{[^{}]*$`)
	importAfter  = regexp.MustCompile(`^[^{}]*\}\s*from\s*["']([^"'\n]*)["']`)
	// importQuotes matches a module name being typed in quotes after from
	importQuotes = regexp.MustCompile(`from\s*["'][^"'\n]*$`)
)

// Complete lists what fits at offset in module (read from src): names, members after a dot, types, an import's
// names, or module names in an import's quotes.
func Complete(module string, src Source, opts ScriptOptions, offset int) []Completion {
	text, err := src.Load(module)
	if err != nil || offset < 0 || offset > len(text) {
		return nil
	}
	before := text[:offset]
	if importQuotes.MatchString(before) {
		var out []Completion
		if l, ok := src.(Lister); ok {
			for _, id := range l.List() {
				out = append(out, Completion{Label: id, Kind: RefModule})
			}
		}
		for _, h := range opts.Hosts {
			out = append(out, Completion{Label: h.Name, Kind: RefModule, Detail: "the host's module"})
		}
		return out
	}
	if importBraces.MatchString(before) {
		if m := importAfter.FindStringSubmatch(text[offset:]); m != nil {
			return exportsOf(m[1], src, opts)
		}
		return nil
	}
	start, end := offset, offset
	for start > 0 && isIdentByte(text[start-1]) {
		start--
	}
	for end < len(text) && isIdentByte(text[end]) {
		end++
	}
	// The probe in place of the word at the cursor; then, for code that follows on the same line (ctx.| log(…)),
	// with the statement ended right after it
	for _, after := range []string{"", ";"} {
		p := &probe{}
		analyze(module, overlay{base: src, id: module, text: text[:start] + probeName + after + text[end:]}, opts, p)
		if p.hit {
			return p.items
		}
	}
	// The source around the cursor doesn't parse yet: offer the keywords that start declarations and statements
	var out []Completion
	seen := map[string]bool{}
	for _, k := range append(append([]string{}, topKeywords...), statementKeywords...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, Completion{Label: k, Kind: RefKeyword})
		}
	}
	return out
}

func isIdentByte(c byte) bool { return isIdentStart(c) || isDigit(c) }

// exportsOf lists what module id exports, for the names of an import.
func exportsOf(id string, src Source, opts ScriptOptions) []Completion {
	c := newChecker(opts)
	c.rec = &recorder{refs: map[string][]Ref{}}
	c.loadAll(id, src)
	for _, m := range c.order {
		c.checkModuleSafely(m)
	}
	m := c.modules[id]
	if m == nil {
		return nil
	}
	var out []Completion
	for _, n := range sortedKeys(m.exports) {
		if n == "default" {
			continue
		}
		r := c.symbolRef(m.exports[n], false, nil)
		out = append(out, Completion{Label: n, Kind: r.Kind, Detail: r.Detail, Doc: r.Doc})
	}
	return out
}

// SignatureHelp is the signature of the call the cursor is in, with the parameter it is at.
type SignatureHelp struct {
	Signature Signature
	Active    int
}

// SignatureAt finds the call around offset in module (read from src): the function, method or constructor
// before the innermost unclosed "(" and how many arguments come before the cursor. An unfinished argument list
// is completed with the probe (and a ")") so the rest of the module still checks.
func SignatureAt(module string, src Source, opts ScriptOptions, offset int) (SignatureHelp, bool) {
	text, err := src.Load(module)
	if err != nil || offset < 0 || offset > len(text) {
		return SignatureHelp{}, false
	}
	var chosen string
	for _, candidate := range []string{
		text,
		text[:offset] + probeName + text[offset:],
		text[:offset] + probeName + ")" + text[offset:],
	} {
		if _, err := parseModule(candidate, DefaultScriptLimits, false); err == nil {
			chosen = candidate
			break
		}
	}
	if chosen == "" {
		return SignatureHelp{}, false
	}
	toks, _, err := lexScript(chosen)
	if err != nil {
		return SignatureHelp{}, false
	}
	// The innermost "(" still open at the cursor, and the commas at its level after it
	depth, commas, open := 0, 0, -1
	last := sort.Search(len(toks), func(i int) bool { return toks[i].pos >= offset }) - 1
	for i := last; i >= 0 && open < 0; i-- {
		switch t := toks[i]; {
		case t.kind != tokOp:
		case t.text == ")" || t.text == "]" || t.text == "}":
			depth++
		case t.text == "(" || t.text == "[" || t.text == "{":
			if depth == 0 {
				if t.text != "(" {
					return SignatureHelp{}, false
				}
				open = i
			} else {
				depth--
			}
		case t.text == "," && depth == 0:
			commas++
		case t.text == ";" && depth == 0:
			return SignatureHelp{}, false
		}
	}
	if open < 1 || toks[open-1].kind != tokIdent {
		return SignatureHelp{}, false
	}
	a := analyze(module, overlay{base: src, id: module, text: chosen}, opts, nil)
	ref, ok := a.At(module, toks[open-1].pos)
	if !ok || ref.Signature == nil {
		return SignatureHelp{}, false
	}
	return SignatureHelp{Signature: *ref.Signature, Active: min(commas, max(len(ref.Signature.Params)-1, 0))}, true
}

// ValidName reports whether name can name something in a script: a name that isn't a keyword or the standard
// library's.
func ValidName(name string) bool {
	if name == "" || !isIdentStart(name[0]) || keywords[name] || builtinNames[name] || strings.HasPrefix(name, "__alo") {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isIdentByte(name[i]) {
			return false
		}
	}
	return true
}
