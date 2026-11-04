package language

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ScriptLimits bound what scripts may be and do, so content can't stall or exhaust a server.
type ScriptLimits struct {
	MaxSource  int // bytes of one module's source
	MaxModules int // modules in one script, the entry and everything it imports (host modules aside)
	MaxNesting int // nesting of blocks, expressions and types in the source
	MaxSteps   int // statements, loop turns and calls in one call from the host
	MaxDepth   int // calls nested in one another
	MaxLength  int // elements of an array or entries of a map
	MaxString  int // bytes of a string
	// MaxHeap bounds the bytes one call from the host may allocate, and the bytes an Instance may keep
	// reachable between calls
	MaxHeap int
}

// DefaultScriptLimits suit content scripts.
var DefaultScriptLimits = ScriptLimits{
	MaxSource:  64 << 10,
	MaxModules: 64,
	MaxNesting: 64,
	MaxSteps:   100_000,
	MaxDepth:   64,
	MaxLength:  10_000,
	MaxString:  4096,
	MaxHeap:    4 << 20,
}

// Source gives the scripts a script imports by their IDs. An ID it doesn't know is an error.
type Source interface {
	Load(id string) (string, error)
}

// MapSource is a Source backed by a map of IDs to sources.
type MapSource map[string]string

func (m MapSource) Load(id string) (string, error) {
	src, ok := m[id]
	if !ok {
		return "", fmt.Errorf("no script %q", id)
	}
	return src, nil
}

// ScriptOptions are what CompileScript checks a script against.
type ScriptOptions struct {
	Hosts  []*HostModule
	Limits ScriptLimits
}

// Diagnostic is a problem in a script's source, at a 1-based line and column (in bytes) of a module.
type Diagnostic struct {
	Module string `json:"module"`
	Line   int    `json:"line"`
	Col    int    `json:"col"`
	Msg    string `json:"message"`
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", d.Module, d.Line, d.Col, d.Msg)
}

// DiagnosticsError joins diagnostics into one error.
func DiagnosticsError(diags []Diagnostic) error {
	errs := make([]error, len(diags))
	for i, d := range diags {
		errs[i] = d
	}
	return errors.Join(errs...)
}

// maxDiagnostics is how many problems a compile reports before it stops.
const maxDiagnostics = 50

// moduleInfo is one module of a script, or a host module.
type moduleInfo struct {
	id      string
	src     string
	lines   []int // byte offsets where lines start
	ast     *moduleAST
	host    *HostModule
	symbols map[string]*symbol
	exports map[string]*symbol
	consts  []*constInfo
	imports []string
	// aliases are where names imported under another name (with as, or a default import) were imported
	aliases map[string]Location
}

func newModule(id, src string) *moduleInfo {
	m := &moduleInfo{id: id, src: src, lines: []int{0}, symbols: map[string]*symbol{}, exports: map[string]*symbol{}}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			m.lines = append(m.lines, i+1)
		}
	}
	return m
}

// lineCol turns a byte offset into a 1-based line and column.
func (m *moduleInfo) lineCol(pos int) (int, int) {
	line := sort.Search(len(m.lines), func(i int) bool { return m.lines[i] > pos }) - 1
	if line < 0 {
		line = 0
	}
	return line + 1, pos - m.lines[line] + 1
}

// CompileScript loads the module entry from src with everything it imports, checks it all against the host
// modules, and compiles it to WebAssembly (Script.Module). It reports every problem it finds (up to 50) as
// diagnostics. The module's constants are computed when it compiles, so one that fails is a diagnostic too. Its
// machine code is made only when it is first instantiated, so a host that only wants the module makes none.
func CompileScript(entry string, src Source, opts ScriptOptions) (*Script, []Diagnostic) {
	c := newChecker(opts)
	c.loadAll(entry, src)
	if len(c.diags) > 0 {
		return nil, c.diags
	}
	for _, m := range c.order {
		c.checkModule(m)
	}
	if len(c.diags) > 0 {
		return nil, c.diags
	}
	fail := func(err error) (*Script, []Diagnostic) {
		var se *ScriptError
		if errors.As(err, &se) {
			return nil, []Diagnostic{{Module: se.Module, Line: se.Line, Col: se.Col, Msg: se.Msg}}
		}
		return nil, []Diagnostic{{Module: entry, Line: 1, Col: 1, Msg: err.Error()}}
	}
	bin, _, err := c.generate(c.modules[entry])
	if err != nil {
		return fail(fmt.Errorf("the compiler failed: %w", err))
	}
	s, err := readScript(bin, ScriptOptions{Hosts: opts.Hosts, Limits: c.limits})
	if err != nil {
		return fail(err)
	}
	if err := s.checkConsts(); err != nil {
		return fail(err)
	}
	return s, nil
}

// HostModule is a module the host offers scripts, imported by its name (such as "game"). Its Source is a definition
// file in the language: interfaces, types, and native classes and functions whose code is the host's (no bodies,
// no constants), which tools show and navigate like any module. Natives maps their names to that code: "f" for a native function, "C.m" for a native class
// C's method m, and "C.f" for its readonly field f. A host module imports nothing.
type HostModule struct {
	Name    string
	Source  string
	Natives map[string]NativeFunc

	once  sync.Once
	mod   *moduleInfo
	diags []Diagnostic
}

// NativeFunc is a native function, method or field: this is the object (for methods and fields), and args
// already have the declared types. A field gets no args. It returns a value of the declared type (nothing for
// void); an error fails the script's call at the place it called from.
type NativeFunc func(c *Call, this Value, args []Value) (Value, error)

// Call is what a native function knows about the call it serves.
type Call struct {
	data   any
	in     *Instance
	result typeMeta
}

// Data is what the host gave the Instance (Script.New) or Script.Call this call came from.
func (c *Call) Data() any { return c.data }

// module checks the host module once.
func (h *HostModule) module() (*moduleInfo, []Diagnostic) {
	h.once.Do(func() {
		c := newChecker(ScriptOptions{Limits: DefaultScriptLimits})
		m := newModule(h.Name, h.Source)
		m.host = h
		ast, err := parseModule(h.Source, DefaultScriptLimits, true)
		if err != nil {
			c.compileError(m, err)
			h.diags = c.diags
			return
		}
		m.ast = ast
		if len(ast.imports) > 0 {
			c.errorf(m, ast.imports[0].pos, "a host module imports nothing")
		}
		c.modules[h.Name] = m
		c.checkModule(m)
		h.mod, h.diags = m, c.diags
	})
	return h.mod, h.diags
}

// Check reports the problems in the host module's own source and natives.
func (h *HostModule) Check() error {
	_, diags := h.module()
	if len(diags) > 0 {
		return DiagnosticsError(diags)
	}
	return nil
}

// Interface finds an interface the host module exports.
func (h *HostModule) Interface(name string) (*Interface, error) {
	m, diags := h.module()
	if len(diags) > 0 {
		return nil, DiagnosticsError(diags)
	}
	sym := m.exports[name]
	if sym == nil || sym.kind != symIface {
		return nil, fmt.Errorf("%s exports no interface %s", h.Name, name)
	}
	return &Interface{info: sym.iface}, nil
}

// Object makes an object of the host module's native class with the host's payload, for passing to scripts.
func (h *HostModule) Object(class string, payload any) (Value, error) {
	m, diags := h.module()
	if len(diags) > 0 {
		return Value{}, DiagnosticsError(diags)
	}
	sym := m.symbols[class]
	if sym == nil || sym.kind != symClass || !sym.class.native {
		return Value{}, fmt.Errorf("%s has no native class %s", h.Name, class)
	}
	return Value{kind: Object, ref: &object{class: sym.class, payload: payload}}, nil
}

// InterfaceMeta describes an interface for hosts without this package's checker (Kiln's Rust runtime), so they
// can tell whether a compiled script's class satisfies it by the same rules (Class.Satisfies): its members in
// declaration order, those of the interfaces it extends included.
type InterfaceMeta struct {
	Name    string            `json:"name"`
	Members []InterfaceMember `json:"members"`
}

// InterfaceMember is a method or field of an interface. Canon is its type as Satisfies compares it (a method's
// signature, classes and interfaces named by the module that declares them), Text as messages write it.
type InterfaceMember struct {
	Name     string `json:"name"`
	Method   bool   `json:"method,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Readonly bool   `json:"readonly,omitempty"`
	Canon    string `json:"canon"`
	Text     string `json:"text"`
}

// Meta describes the interface for hosts without the checker.
func (i *Interface) Meta() InterfaceMeta {
	info := i.info
	out := InterfaceMeta{Name: info.name, Members: []InterfaceMember{}}
	for _, name := range info.order {
		if m, ok := info.methods[name]; ok {
			out.Members = append(out.Members, InterfaceMember{Name: name, Method: true, Optional: m.optional,
				Canon: canonSig(m), Text: m.signature()})
			continue
		}
		f := info.fields[name]
		out.Members = append(out.Members, InterfaceMember{Name: name, Readonly: f.readonly, Canon: canon(f.typ),
			Text: f.typ.String()})
	}
	return out
}

// Interfaces describes every interface the host module exports, by name, for hosts without the checker
// (Interface.Meta).
func (h *HostModule) Interfaces() (map[string]InterfaceMeta, error) {
	m, diags := h.module()
	if len(diags) > 0 {
		return nil, DiagnosticsError(diags)
	}
	out := map[string]InterfaceMeta{}
	for name, sym := range m.exports {
		if sym.kind == symIface {
			out[name] = (&Interface{info: sym.iface}).Meta()
		}
	}
	return out, nil
}
