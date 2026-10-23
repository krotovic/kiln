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
	globals []Value
	consts  []*constInfo
	imports []string
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

// Script is a compiled script: its entry module and every module it imports, checked together. A Script is
// immutable (module constants are numbers, strings and bools, computed once) and safe to use from many
// goroutines; each Instance belongs to one goroutine at a time.
type Script struct {
	entry   *moduleInfo
	modules []*moduleInfo // dependencies first, the entry last; host modules aside
	limits  ScriptLimits
}

// CompileScript loads the module entry from src with everything it imports, and checks it all against the
// host modules. It reports every problem it finds (up to 50) as diagnostics.
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
	return &Script{entry: c.modules[entry], modules: c.order, limits: c.limits}, nil
}

// Modules lists the IDs of the script's modules: the entry and everything it imports, host modules aside.
func (s *Script) Modules() []string {
	out := make([]string, len(s.modules))
	for i, m := range s.modules {
		out[i] = m.id
	}
	return out
}

// Class is a class a script exports.
type Class struct {
	info   *classInfo
	script *Script
}

// Interface is an interface a host module or script exports.
type Interface struct{ info *ifaceInfo }

// DefaultClass is the class the entry module exports as default.
func (s *Script) DefaultClass() (*Class, bool) { return s.Class("default") }

// Class finds a class the entry module exports.
func (s *Script) Class(name string) (*Class, bool) {
	sym := s.entry.exports[name]
	if sym == nil || sym.kind != symClass || sym.class.native {
		return nil, false
	}
	return &Class{info: sym.class, script: s}, true
}

func (c *Class) Name() string { return c.info.name }

// Satisfies says why the class doesn't satisfy i, or "" when it does.
func (c *Class) Satisfies(i *Interface) string { return c.info.satisfies(i.info) }

// ConstructorParams is how many arguments the class's constructor takes.
func (c *Class) ConstructorParams() int { return len(c.info.ctor.params) }

// Call calls a function the entry module exports, with data for the host's native functions.
func (s *Script) Call(name string, data any, args ...Value) (Value, error) {
	sym := s.entry.exports[name]
	if sym == nil || sym.kind != symFunc {
		return Value{}, fmt.Errorf("%s exports no function %s", s.entry.id, name)
	}
	if len(args) != len(sym.fn.params) {
		return Value{}, fmt.Errorf("%s takes %s", name, plural(len(sym.fn.params)))
	}
	v := &vm{limits: s.limits, data: data}
	return v.call(sym.fn, Value{}, args, sym.fn.mod, sym.fn.pos)
}

// Instance is an object of a script's class that the host keeps and calls into. It isn't safe for concurrent
// use.
type Instance struct {
	script *Script
	class  *classInfo
	obj    Value
	data   any
	// allocated counts the bytes calls allocated since the instance's memory was last measured
	allocated int
}

// New makes an object of c with its constructor, which must take no arguments. data is what the host's native
// functions get (Call.Data) whenever this instance calls them.
func (s *Script) New(c *Class, data any) (*Instance, error) {
	in := &Instance{script: s, class: c.info, data: data}
	if len(c.info.ctor.params) != 0 {
		return nil, fmt.Errorf("%s's constructor takes arguments", c.info.name)
	}
	_, err := in.run(func(v *vm) (Value, error) {
		obj, err := v.newObject(c.info, c.info.mod, c.info.pos)
		if err != nil {
			return Value{}, err
		}
		in.obj = obj
		_, err = v.call(c.info.ctor, obj, nil, c.info.mod, c.info.pos)
		return Value{}, err
	})
	if err != nil {
		return nil, err
	}
	return in, nil
}

// run runs fn as one call from the host, with fresh budgets, and checks the instance's memory afterwards.
func (in *Instance) run(fn func(v *vm) (Value, error)) (Value, error) {
	v := &vm{limits: in.script.limits, data: in.data}
	out, err := fn(v)
	if err != nil {
		return Value{}, err
	}
	in.allocated += v.allocated
	if in.allocated > in.script.limits.MaxHeap/2 {
		in.allocated = 0
		if heapSize(in.obj, in.script.limits.MaxHeap) > in.script.limits.MaxHeap {
			m := in.class.mod
			return Value{}, m.errAt(in.class.pos, "%s keeps more than %d bytes of memory", in.class.name, in.script.limits.MaxHeap)
		}
	}
	return out, nil
}

// HasMethod reports whether the instance's class has a public method name.
func (in *Instance) HasMethod(name string) bool {
	m := in.class.methods[name]
	return m != nil && !m.private
}

// Call calls the instance's public method name with args, which must have the method's parameter types.
func (in *Instance) Call(method string, args ...Value) (Value, error) {
	f := in.class.methods[method]
	if f == nil || f.private {
		return Value{}, fmt.Errorf("%s has no method %s", in.class.name, method)
	}
	if len(args) != len(f.params) {
		return Value{}, fmt.Errorf("%s.%s takes %s", in.class.name, method, plural(len(f.params)))
	}
	for i, a := range args {
		if !fits(a, f.params[i]) {
			return Value{}, fmt.Errorf("%s.%s wants a %s, not a %s", in.class.name, method, f.params[i], a.Kind())
		}
	}
	return in.run(func(v *vm) (Value, error) { return v.call(f, in.obj, args, f.mod, f.pos) })
}

// FieldKind is the kind of the instance's public field name, when it is a number, a string or a bool, so a host
// can set it from text.
func (in *Instance) FieldKind(name string) (Kind, bool) {
	f := in.class.fieldByName[name]
	if f == nil || f.private || !f.typ.primitive() {
		return 0, false
	}
	return f.typ.valueKind(), true
}

// Set sets the instance's public field name (a number, string or bool field, readonly ones included) to v.
func (in *Instance) Set(name string, v Value) error {
	kind, ok := in.FieldKind(name)
	if !ok {
		return fmt.Errorf("%s has no field %s of a number, string or bool", in.class.name, name)
	}
	if v.kind != kind {
		return fmt.Errorf("%s.%s is a %s, not a %s", in.class.name, name, kind, v.Kind())
	}
	o := in.obj.ref.(*object)
	o.fields[in.class.fieldByName[name].slot] = v
	return nil
}

// Get reads the instance's field name (tests and tools use it).
func (in *Instance) Get(name string) (Value, bool) {
	f := in.class.fieldByName[name]
	if f == nil || f.getter != nil {
		return Value{}, false
	}
	return in.obj.ref.(*object).fields[f.slot], true
}

// HostModule is a module the host offers scripts, imported by its name (such as "game"). Its source declares
// it in the language: interfaces, classes and functions written in it, and native classes and functions whose
// code is the host's. Natives maps their names to that code: "f" for a native function, "C.m" for a native class
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
type Call struct{ vm *vm }

// Data is what the host gave the Instance (Script.New) or Script.Call this call came from.
func (c *Call) Data() any { return c.vm.data }

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
