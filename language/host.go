package language

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"adventurers-life-online/server/internal/engine/wasm"
)

// The Go host of compiled scripts: one wazero runtime per process that compiles modules to machine code (on
// amd64 and arm64) for the scripts that run, one that interprets them for the checks CompileScript makes, each
// with the "alo" module of every script's imports, and one module instance per Instance.

type wazeroHost struct {
	once sync.Once
	rt   wazero.Runtime
	err  error
}

var (
	machineHost wazeroHost // runs scripts
	checkHost   wazeroHost // runs a script's constants once, when it compiles: no machine code to make
)

type instanceKey struct{}

func runtimeHost() (wazero.Runtime, error) {
	return machineHost.get(wazero.NewRuntimeConfig())
}

func checkRuntime() (wazero.Runtime, error) {
	return checkHost.get(wazero.NewRuntimeConfigInterpreter())
}

func (w *wazeroHost) get(cfg wazero.RuntimeConfig) (wazero.Runtime, error) {
	w.once.Do(func() {
		ctx := context.Background()
		rt := wazero.NewRuntimeWithConfig(ctx, cfg)
		b := rt.NewHostModuleBuilder("alo")
		i32, f64 := api.ValueTypeI32, api.ValueTypeF64
		add := func(name string, fn func(in *Instance, stack []uint64), params, results []api.ValueType) {
			b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) {
				fn(ctx.Value(instanceKey{}).(*Instance), stack)
			}), params, results).Export(name)
		}
		add(impFail, func(in *Instance, stack []uint64) {
			in.err = in.failure(int(int32(stack[0])), int32(stack[1]))
		}, []api.ValueType{i32, i32}, nil)
		add(impNative, func(in *Instance, stack []uint64) {
			if err := in.native(int(int32(stack[0])), int32(stack[1])); err != nil {
				in.err = err
				stack[0] = 1
				return
			}
			stack[0] = 0
		}, []api.ValueType{i32, i32}, []api.ValueType{i32})
		add(impFmtNum, func(in *Instance, stack []uint64) {
			p, err := in.newString(numberText(api.DecodeF64(stack[0])))
			if err != nil {
				panic(err) // the script ran out of memory: unwind its call
			}
			stack[0] = uint64(p)
		}, []api.ValueType{f64}, []api.ValueType{i32})
		add(impParseNum, func(in *Instance, stack []uint64) {
			f, err := strconv.ParseFloat(in.readBytes(uint32(stack[0]), uint32(stack[1])), 64)
			if err != nil || !finite(f) {
				stack[0] = 0
				return
			}
			in.mem.WriteFloat64Le(scratchBase, f)
			stack[0] = 1
		}, []api.ValueType{i32, i32}, []api.ValueType{i32})
		add(impFmod, func(in *Instance, stack []uint64) {
			stack[0] = api.EncodeF64(math.Mod(api.DecodeF64(stack[0]), api.DecodeF64(stack[1])))
		}, []api.ValueType{f64, f64}, []api.ValueType{f64})
		add(impRelease, func(in *Instance, stack []uint64) {
			in.release(int32(stack[0]))
		}, []api.ValueType{i32}, nil)
		if _, err := b.Instantiate(ctx); err != nil {
			w.err = err
			return
		}
		w.rt = rt
	})
	return w.rt, w.err
}

// Script is a compiled script, whose module the host instantiates for each Instance. It is immutable and safe to
// share between goroutines. Its module's machine code is made when it is first instantiated (LoadScript makes it
// at once), and freed once the script is closed and its last instance is.
type Script struct {
	module []byte
	meta   *scriptMeta

	mu        sync.Mutex
	compiled  wazero.CompiledModule
	instances int
	closed    bool

	natives     []NativeFunc
	nativeNames []string           // the member's own name, for errors
	hostClasses map[int]*classInfo // native classes by index
	classIndex  map[*classInfo]int // and back
	hostIfaces  map[string]*HostModule
}

// Module is the script compiled to WebAssembly, with its metadata: what the asset service stores and the
// browser and LoadScript run.
func (s *Script) Module() []byte { return s.module }

// Modules lists the IDs of the script's modules: the entry and everything it imports, host modules aside.
func (s *Script) Modules() []string { return append([]string(nil), s.meta.Modules...) }

// hostHash is what a module records of a host module's definition file.
func hostHash(h *HostModule) string {
	sum := sha256.Sum256([]byte(h.Source))
	return hex.EncodeToString(sum[:8])
}

// LoadScript loads a module CompileScript made (Script.Module), against the host modules it was compiled with, and
// compiles it to machine code. It refuses one made for another version of the runtime, or against another
// definition of a host module. Close frees it.
func LoadScript(module []byte, opts ScriptOptions) (*Script, error) {
	s, err := readScript(module, opts)
	if err != nil {
		return nil, err
	}
	if err := s.compile(); err != nil {
		return nil, err
	}
	return s, nil
}

// readScript reads a module's metadata and binds its natives, without compiling it.
func readScript(module []byte, opts ScriptOptions) (*Script, error) {
	raw, ok := wasm.CustomSection(module, metaSection)
	if !ok {
		return nil, errors.New("not a compiled script: it has no " + metaSection + " section")
	}
	meta := &scriptMeta{}
	if err := json.Unmarshal(raw, meta); err != nil {
		return nil, fmt.Errorf("a compiled script with broken metadata: %w", err)
	}
	if meta.ABI != abiVersion {
		return nil, fmt.Errorf("the script was compiled for runtime version %d, not %d: compile it again", meta.ABI, abiVersion)
	}
	hosts := map[string]*HostModule{}
	for _, h := range opts.Hosts {
		hosts[h.Name] = h
	}
	s := &Script{module: module, meta: meta, hostClasses: map[int]*classInfo{}, classIndex: map[*classInfo]int{}, hostIfaces: hosts}
	for _, hm := range meta.Hosts {
		h := hosts[hm.Name]
		if h == nil {
			return nil, fmt.Errorf("the script imports the host module %q, which this host doesn't offer", hm.Name)
		}
		if hostHash(h) != hm.Hash {
			return nil, fmt.Errorf("the script was compiled against another version of the host module %q: compile it again", hm.Name)
		}
		if err := h.Check(); err != nil {
			return nil, err
		}
	}
	for i, cm := range meta.Classes {
		if !cm.Native {
			continue
		}
		h := hosts[cm.Module]
		if h == nil {
			return nil, fmt.Errorf("no host module %q for the native class %s", cm.Module, cm.Name)
		}
		m, _ := h.module()
		sym := m.symbols[cm.Name]
		if sym == nil || sym.kind != symClass {
			return nil, fmt.Errorf("the host module %q has no class %s", cm.Module, cm.Name)
		}
		s.hostClasses[i] = sym.class
		s.classIndex[sym.class] = i
	}
	for _, nm := range meta.Natives {
		h := hosts[nm.Host]
		var fn NativeFunc
		if h != nil {
			fn = h.Natives[nm.Name]
		}
		if fn == nil {
			return nil, fmt.Errorf("the host gives no code for the native %s of %q", nm.Name, nm.Host)
		}
		s.natives = append(s.natives, fn)
		member := nm.Name
		if i := strings.LastIndexByte(member, '.'); i >= 0 {
			member = member[i+1:]
		}
		s.nativeNames = append(s.nativeNames, member)
	}
	return s, nil
}

// compile makes the module's machine code, once.
func (s *Script) compile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compileLocked()
}

func (s *Script) compileLocked() error {
	if s.closed {
		return errors.New("the script is closed")
	}
	if s.compiled != nil {
		return nil
	}
	rt, err := runtimeHost()
	if err != nil {
		return err
	}
	s.compiled, err = rt.CompileModule(context.Background(), s.module)
	return err
}

// Close frees the script's machine code once its instances are closed too; it can't make more afterwards.
func (s *Script) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.release()
}

// release frees the machine code of a closed script without instances; the caller holds mu.
func (s *Script) release() {
	if s.closed && s.instances == 0 && s.compiled != nil {
		_ = s.compiled.Close(context.Background())
		s.compiled = nil
	}
}

// checkConsts runs the module's constants once in the interpreting runtime, as CompileScript checks them.
func (s *Script) checkConsts() error {
	rt, err := checkRuntime()
	if err != nil {
		return err
	}
	ctx := context.Background()
	compiled, err := rt.CompileModule(ctx, s.module)
	if err != nil {
		return err
	}
	defer compiled.Close(ctx)
	in, err := s.newInstance(rt, compiled, nil)
	if err != nil {
		return err
	}
	defer in.mod.Close(in.ctx)
	return in.initConsts()
}

// Class is a class a script exports.
type Class struct {
	script *Script
	idx    int
}

// Interface is an interface a host module or script exports.
type Interface struct{ info *ifaceInfo }

// DefaultClass is the class the entry module exports as default.
func (s *Script) DefaultClass() (*Class, bool) { return s.Class("default") }

// Class finds a class the entry module exports.
func (s *Script) Class(name string) (*Class, bool) {
	i, ok := s.meta.Exports[name]
	if !ok {
		return nil, false
	}
	return &Class{script: s, idx: i}, true
}

func (c *Class) meta() *classMeta { return &c.script.meta.Classes[c.idx] }

func (c *Class) Name() string { return c.meta().Name }

// Satisfies says why the class doesn't satisfy i, or "" when it does, by the rules the checker has (satisfiesBy).
func (c *Class) Satisfies(i *Interface) string {
	cm := c.meta()
	return satisfiesBy(i.info, func(name string) (member, bool) {
		if m := cm.Methods[name]; m != nil {
			return member{method: true, private: m.Private, text: m.Sig, canon: m.Canon}, true
		}
		for _, f := range cm.Fields {
			if f.Name == name {
				return member{private: f.Private, readonly: f.Readonly, text: f.Type.Name, canon: f.Canon}, true
			}
		}
		return member{}, false
	})
}

// ConstructorParams is how many arguments the class's constructor takes.
func (c *Class) ConstructorParams() int { return len(c.meta().Ctor) }

// Call calls a function the entry module exports, with data for the host's native functions, on an instance of
// the script's module of its own.
func (s *Script) Call(name string, data any, args ...Value) (Value, error) {
	fm := s.meta.Funcs[name]
	if fm == nil {
		return Value{}, fmt.Errorf("%s exports no function %s", s.meta.Entry, name)
	}
	if len(args) != len(fm.Params) {
		return Value{}, fmt.Errorf("%s takes %s", name, plural(len(fm.Params)))
	}
	in, err := s.instantiate(data)
	if err != nil {
		return Value{}, err
	}
	defer in.Close()
	if err := in.initConsts(); err != nil {
		return Value{}, err
	}
	return in.invoke(fm.Export, -1, args, fm.Params, fm.Result)
}

// Instance is an object of a script's class that the host keeps and calls into, with a module instance of its
// own. It isn't safe for concurrent use. Objects of the script it hands the host (results, arguments of natives)
// stay valid until the host's next call into it, unless the host keeps them (Keep).
type Instance struct {
	script  *Script
	mod     api.Module
	mem     api.Memory
	ctx     context.Context
	data    any
	class   int  // the class of its object, -1 for Script.Call's
	counted bool // among its script's instances
	self    int32

	fns    map[string]api.Function
	stack  []uint64
	hstack []uint64
	err    error

	temp []int32 // handles of objects handed out during the last call
	kept []int32
	live map[int32]bool // every handle the host holds: self, temp and kept

	natObjs map[int32]*object
	natIdx  map[*object]int32
	natFree []int32
	natNext int32

	// pending are the promises natives made that the host hasn't settled yet
	pending int
}

// scriptRef is a script's object the host holds, by its handle in its instance.
type scriptRef struct {
	in     *Instance
	handle int32
	class  int
}

func (s *Script) instantiate(data any) (*Instance, error) {
	rt, err := runtimeHost()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.compileLocked(); err != nil {
		return nil, err
	}
	in, err := s.newInstance(rt, s.compiled, data)
	if err != nil {
		return nil, err
	}
	s.instances++
	in.counted = true
	return in, nil
}

func (s *Script) newInstance(rt wazero.Runtime, compiled wazero.CompiledModule, data any) (*Instance, error) {
	in := &Instance{script: s, data: data, class: -1, fns: map[string]api.Function{}, stack: make([]uint64, 16),
		hstack: make([]uint64, 4), natObjs: map[int32]*object{}, natIdx: map[*object]int32{}, natNext: 1, live: map[int32]bool{}}
	in.ctx = context.WithValue(context.Background(), instanceKey{}, in)
	mod, err := rt.InstantiateModule(in.ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, err
	}
	in.mod, in.mem = mod, mod.Memory()
	return in, nil
}

// New makes an object of c with its constructor, which must take no arguments. data is what the host's native
// functions get (Call.Data) whenever this instance calls them.
func (s *Script) New(c *Class, data any) (*Instance, error) {
	if c.ConstructorParams() != 0 {
		return nil, fmt.Errorf("%s's constructor takes arguments", c.Name())
	}
	in, err := s.instantiate(data)
	if err != nil {
		return nil, err
	}
	in.class = c.idx
	if err := in.initConsts(); err != nil {
		in.Close()
		return nil, err
	}
	in.begin()
	obj, err := in.helper(fmt.Sprintf("n%d", c.idx))
	if err == nil {
		in.self, err = in.helper("__handle_new", uint64(uint32(obj)))
		in.live[in.self] = true
	}
	if err == nil {
		in.stack[0] = uint64(uint32(obj))
		err = in.callExport(fmt.Sprintf("c%d", c.idx), 1)
	}
	if err == nil {
		err = in.end()
	}
	if err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

// initConsts computes the module's constants.
func (in *Instance) initConsts() error {
	in.begin()
	if err := in.callExport("__init", 0); err != nil {
		return err
	}
	return in.end()
}

// Close frees the instance's module. The instance can't be used afterwards.
func (in *Instance) Close() {
	if in.mod == nil {
		return
	}
	in.mod.Close(in.ctx)
	in.mod = nil
	if in.counted {
		s := in.script
		s.mu.Lock()
		s.instances--
		s.release()
		s.mu.Unlock()
	}
}

func (in *Instance) fn(name string) (api.Function, error) {
	if f, ok := in.fns[name]; ok {
		return f, nil
	}
	if in.mod == nil {
		return nil, errors.New("the script's instance is closed")
	}
	f := in.mod.ExportedFunction(name)
	if f == nil {
		return nil, fmt.Errorf("the compiled script has no %s", name)
	}
	in.fns[name] = f
	return f, nil
}

// callExport calls an export with its n parameters in in.stack, leaving its result in in.stack[0].
func (in *Instance) callExport(name string, n int) error {
	f, err := in.fn(name)
	if err != nil {
		return err
	}
	if err := f.CallWithStack(in.ctx, in.stack); err != nil {
		return in.trapped(err)
	}
	return nil
}

// helper calls one of the runtime's exports from the host, while a call may be under way.
func (in *Instance) helper(name string, args ...uint64) (int32, error) {
	f, err := in.fn(name)
	if err != nil {
		return 0, err
	}
	stack := in.hstack
	if len(args) > len(stack) {
		stack = make([]uint64, len(args))
	}
	copy(stack, args)
	// an error is this call's own: one an earlier call left isn't
	saved := in.err
	in.err = nil
	if err := f.CallWithStack(in.ctx, stack); err != nil {
		return 0, in.trapped(err)
	}
	if in.err == nil {
		in.err = saved
	}
	return int32(stack[0]), nil
}

// trapped is the error a call stopped with: the script's own, which the runtime reported before it trapped, or
// the trap.
func (in *Instance) trapped(err error) error {
	if in.err != nil {
		e := in.err
		return e
	}
	var se *ScriptError
	if errors.As(err, &se) {
		return se
	}
	return in.script.meta.errorAt(0, "the script stopped: %v", err)
}

func (in *Instance) begin() {
	in.err = nil
	in.stack[0] = 0
	if f, err := in.fn("__begin"); err == nil {
		_ = f.CallWithStack(in.ctx, in.stack)
	}
}

// end ends a call: it collects when it is time, and fails when the instance keeps more than MaxHeap.
func (in *Instance) end() error {
	live, err := in.helper("__end")
	if err != nil {
		return err
	}
	if limit := in.script.meta.Limits.MaxHeap; live >= 0 && int(live) > limit && in.class >= 0 {
		cm := in.script.meta.Classes[in.class]
		return in.script.meta.errorAt(cm.Pos, "%s keeps more than %d bytes of memory", cm.Name, limit)
	}
	return nil
}

// dropTemps forgets the handles of the objects the last call handed out, unless they were kept.
func (in *Instance) dropTemps() {
	for _, h := range in.temp {
		delete(in.live, h)
		_, _ = in.helper("__handle_drop", uint64(h))
	}
	in.temp = in.temp[:0]
}

// invoke runs an export: a method of the object with handle this (-1: a function), with arguments of the
// given types and a result of type result.
func (in *Instance) invoke(export string, this int32, args []Value, params []typeMeta, result typeMeta) (Value, error) {
	in.begin()
	n := 0
	raw := make([]uint64, 0, len(args)+1)
	for i, a := range args {
		r, err := in.toWasm(a, params[i])
		if err != nil {
			return Value{}, err
		}
		raw = append(raw, r)
	}
	if this >= 0 {
		p, err := in.helper("__handle_get", uint64(this))
		if err != nil {
			return Value{}, err
		}
		in.stack[0] = uint64(uint32(p))
		n = 1
	}
	if n+len(raw) > len(in.stack) {
		in.stack = make([]uint64, n+len(raw))
	}
	copy(in.stack[n:], raw)
	in.dropTemps()
	if err := in.callExport(export, n+len(raw)); err != nil {
		return Value{}, err
	}
	var out Value
	if result.K != "v" {
		v, err := in.fromWasm(in.stack[0], result)
		if err != nil {
			return Value{}, err
		}
		out = v
	}
	if err := in.end(); err != nil {
		return Value{}, err
	}
	return out, nil
}

// HasMethod reports whether the instance's class has a public method name.
func (in *Instance) HasMethod(name string) bool {
	m := in.script.meta.Classes[in.class].Methods[name]
	return m != nil && !m.Private
}

// Call calls the instance's public method name with args, which must have the method's parameter types.
func (in *Instance) Call(method string, args ...Value) (Value, error) {
	return in.callOn(in.self, in.class, method, args)
}

// HasMethodOn reports whether obj, an object of this instance's script, has a public method name.
func (in *Instance) HasMethodOn(obj Value, name string) bool {
	ref, ok := in.own(obj)
	if !ok {
		return false
	}
	m := in.script.meta.Classes[ref.class].Methods[name]
	return m != nil && !m.Private
}

// CallOn calls the public method of obj, an object of this instance's script that it handed out, under the same
// budgets as one call from the host.
func (in *Instance) CallOn(obj Value, method string, args ...Value) (Value, error) {
	ref, ok := in.own(obj)
	if !ok {
		return Value{}, fmt.Errorf("%s isn't an object of this script", obj)
	}
	return in.callOn(ref.handle, ref.class, method, args)
}

// Self is the instance's object.
func (in *Instance) Self() Value {
	return Value{kind: Object, ref: &scriptRef{in: in, handle: in.self, class: in.class}}
}

// Keep tells the instance which more objects of the script the host holds on to: they stay valid past the next
// call, and their memory counts as the instance's own. It replaces what Keep was given before.
func (in *Instance) Keep(objs []Value) {
	var kept []int32
	var refs []*scriptRef
	for _, o := range objs {
		ref, ok := in.own(o)
		if !ok || ref.handle == in.self {
			continue
		}
		p, err := in.helper("__handle_get", uint64(ref.handle))
		if err != nil {
			continue
		}
		if h, err := in.helper("__handle_new", uint64(uint32(p))); err == nil {
			kept = append(kept, h)
			refs = append(refs, ref)
		}
	}
	for _, h := range in.kept {
		delete(in.live, h)
		_, _ = in.helper("__handle_drop", uint64(h))
	}
	for _, h := range kept {
		in.live[h] = true
	}
	in.kept = kept
	for i, ref := range refs {
		ref.handle = kept[i]
	}
}

// own finds the handle of obj, an object of this instance the host still holds.
func (in *Instance) own(obj Value) (*scriptRef, bool) {
	ref, ok := obj.ref.(*scriptRef)
	if !ok || ref.in != in || !in.live[ref.handle] {
		return nil, false
	}
	return ref, true
}

func (in *Instance) callOn(this int32, class int, method string, args []Value) (Value, error) {
	cm := &in.script.meta.Classes[class]
	m := cm.Methods[method]
	if m == nil || m.Private || m.Export == "" {
		return Value{}, fmt.Errorf("%s has no method %s", cm.Name, method)
	}
	if len(args) != len(m.Params) {
		return Value{}, fmt.Errorf("%s.%s takes %s", cm.Name, method, plural(len(m.Params)))
	}
	for i, a := range args {
		if !fitsMeta(a, m.Params[i]) {
			return Value{}, fmt.Errorf("%s.%s wants a %s, not a %s", cm.Name, method, m.Params[i].Name, a.Kind())
		}
	}
	return in.invoke(m.Export, this, args, m.Params, m.Result)
}

// FieldKind is the kind of the instance's public field name, when it is a number, a string or a bool, so a host
// can set it from text.
func (in *Instance) FieldKind(name string) (Kind, bool) {
	f := in.field(name)
	if f == nil || f.Private {
		return 0, false
	}
	switch f.Type.K {
	case "n":
		return Number, true
	case "s":
		return String, true
	case "b":
		return Bool, true
	}
	return 0, false
}

func (in *Instance) field(name string) *fieldMeta {
	cm := &in.script.meta.Classes[in.class]
	for i := range cm.Fields {
		if cm.Fields[i].Name == name {
			return &cm.Fields[i]
		}
	}
	return nil
}

// Set sets the instance's public field name (a number, string or bool field, readonly ones included) to v.
func (in *Instance) Set(name string, v Value) error {
	kind, ok := in.FieldKind(name)
	cm := in.script.meta.Classes[in.class]
	if !ok {
		return fmt.Errorf("%s has no field %s of a number, string or bool", cm.Name, name)
	}
	if v.kind != kind {
		return fmt.Errorf("%s.%s is a %s, not a %s", cm.Name, name, kind, v.Kind())
	}
	f := in.field(name)
	in.begin()
	raw, err := in.toWasm(v, f.Type)
	if err != nil {
		return err
	}
	p, err := in.helper("__handle_get", uint64(in.self))
	if err != nil {
		return err
	}
	addr := uint32(p) + uint32(f.Offset)
	if kind == Number {
		in.mem.WriteUint64Le(addr, raw)
	} else {
		in.mem.WriteUint32Le(addr, uint32(raw))
	}
	return in.end()
}

// Get reads the instance's field name (tests and tools use it).
func (in *Instance) Get(name string) (Value, bool) {
	f := in.field(name)
	if f == nil {
		return Value{}, false
	}
	p, err := in.helper("__handle_get", uint64(in.self))
	if err != nil {
		return Value{}, false
	}
	addr := uint32(p) + uint32(f.Offset)
	var raw uint64
	if f.Type.K == "n" {
		raw, _ = in.mem.ReadUint64Le(addr)
	} else {
		r, _ := in.mem.ReadUint32Le(addr)
		raw = uint64(r)
	}
	v, err := in.fromWasm(raw, f.Type)
	return v, err == nil
}

// ---- values across the boundary ----

func (in *Instance) u32(addr uint32) uint32 {
	v, _ := in.mem.ReadUint32Le(addr)
	return v
}

func (in *Instance) readBytes(addr, n uint32) string {
	b, ok := in.mem.Read(addr, n)
	if !ok {
		return ""
	}
	return string(b)
}

func (in *Instance) readString(p uint32) string {
	return in.readBytes(p+headerSize, in.u32(p+12))
}

// newString puts s into the script's memory.
func (in *Instance) newString(s string) (uint32, error) {
	p, err := in.helper("__str_new", uint64(len(s)))
	if err != nil {
		return 0, err
	}
	in.mem.WriteString(uint32(p)+headerSize, s)
	return uint32(p), nil
}

func fitsMeta(v Value, t typeMeta) bool {
	switch t.K {
	case "p":
		_, ok := v.ref.(*promiseRef)
		return ok
	case "n":
		return v.kind == Number
	case "s":
		return v.kind == String
	case "b":
		return v.kind == Bool
	case "a":
		return v.kind == Array || v.kind == Null
	case "m":
		return v.kind == Map || v.kind == Null
	case "c", "i":
		return v.kind == Object || v.kind == Null
	case "v":
		return true
	}
	return false
}

func isRefMeta(t *typeMeta) bool { return t.K != "n" && t.K != "b" }

// toWasm turns a host value of type t into the script's representation, putting strings, arrays and maps into
// its memory.
func (in *Instance) toWasm(v Value, t typeMeta) (uint64, error) {
	if !fitsMeta(v, t) {
		return 0, fmt.Errorf("a %s is no %s", v.Kind(), t.Name)
	}
	switch t.K {
	case "n":
		return api.EncodeF64(v.num), nil
	case "b":
		if v.b {
			return 1, nil
		}
		return 0, nil
	case "s":
		p, err := in.newString(v.str)
		return uint64(p), err
	}
	if v.kind == Null {
		return 0, nil
	}
	switch t.K {
	case "a":
		items := v.ref.(*arrayVal).items
		tag := tagArrVals
		if isRefMeta(t.E) {
			tag = tagArrRefs
		}
		raw := make([]uint64, len(items))
		for i, item := range items {
			r, err := in.toWasm(item, *t.E)
			if err != nil {
				return 0, err
			}
			raw[i] = r
		}
		ap, err := in.helper("__arr_new", uint64(tag), uint64(len(items)))
		if err != nil {
			return 0, err
		}
		p := uint32(ap)
		st := in.u32(p + 16)
		for i, r := range raw {
			in.writeSlot(st+headerSize+uint32(8*i), r, *t.E)
		}
		return uint64(p), nil
	case "m":
		mv := v.ref.(*mapVal)
		flags := 0
		if t.Key.K == "s" {
			flags |= mapStrKeys
		}
		if isRefMeta(t.E) {
			flags |= mapRefVals
		}
		p, err := in.helper("__map_new", uint64(flags))
		if err != nil {
			return 0, err
		}
		for i := range mv.keys {
			k, err := in.toWasm(mv.keys[i], *t.Key)
			if err != nil {
				return 0, err
			}
			val, err := in.toWasm(mv.vals[i], *t.E)
			if err != nil {
				return 0, err
			}
			in.writeSlot(scratchBase, k, *t.Key)
			in.writeSlot(scratchBase+8, val, *t.E)
			if _, err := in.helper("__map_put", uint64(uint32(p))); err != nil {
				return 0, err
			}
		}
		return uint64(uint32(p)), nil
	}
	switch r := v.ref.(type) {
	case *promiseRef:
		if r.res.in != in || r.res.handle == 0 {
			return 0, errors.New("a promise of another instance, or one settled already")
		}
		p, err := in.helper("__handle_get", uint64(r.res.handle))
		return uint64(uint32(p)), err
	case *scriptRef:
		if r.in != in {
			return 0, errors.New("an object of another instance of the script")
		}
		if !in.live[r.handle] {
			return 0, errors.New("an object of the script from an earlier call, which the host didn't keep")
		}
		p, err := in.helper("__handle_get", uint64(r.handle))
		return uint64(uint32(p)), err
	case *object:
		cls, ok := in.script.classIndex[r.class]
		if !ok {
			return 0, fmt.Errorf("the script doesn't know the host's class %s", r.class.name)
		}
		if nh, ok := in.natIdx[r]; ok {
			p, err := in.helper("__native_get", uint64(nh))
			return uint64(uint32(p)), err
		}
		nh := in.natNext
		if n := len(in.natFree); n > 0 {
			nh, in.natFree = in.natFree[n-1], in.natFree[:n-1]
		} else {
			in.natNext++
		}
		in.natIdx[r], in.natObjs[nh] = nh, r
		p, err := in.helper("__native_new", uint64(cls), uint64(nh))
		return uint64(uint32(p)), err
	}
	return 0, fmt.Errorf("a %s is no %s", v.Kind(), t.Name)
}

func (in *Instance) writeSlot(addr uint32, raw uint64, t typeMeta) {
	if t.K == "n" {
		in.mem.WriteUint64Le(addr, raw)
	} else {
		in.mem.WriteUint32Le(addr, uint32(raw))
	}
}

func (in *Instance) readSlot(addr uint32, t typeMeta) uint64 {
	if t.K == "n" {
		v, _ := in.mem.ReadUint64Le(addr)
		return v
	}
	return uint64(in.u32(addr))
}

// fromWasm turns the script's value raw of type t into a host value: strings, arrays and maps copied out, and
// objects by handle (valid until the next call, unless kept).
func (in *Instance) fromWasm(raw uint64, t typeMeta) (Value, error) {
	switch t.K {
	case "n":
		return Num(api.DecodeF64(raw)), nil
	case "b":
		return Boolean(uint32(raw) != 0), nil
	case "s":
		return Str(in.readString(uint32(raw))), nil
	case "v", "p":
		// the host doesn't hold the script's promises: an async method's promise is ignored
		return Value{}, nil
	}
	p := uint32(raw)
	if p == 0 {
		return null, nil
	}
	switch t.K {
	case "a":
		n, st := in.u32(p+12), in.u32(p+16)
		items := make([]Value, n)
		for i := range items {
			v, err := in.fromWasm(in.readSlot(st+headerSize+uint32(8*i), *t.E), *t.E)
			if err != nil {
				return Value{}, err
			}
			items[i] = v
		}
		return Value{kind: Array, ref: &arrayVal{items: items}}, nil
	case "m":
		n, ks, vs := in.u32(p+12), in.u32(p+16), in.u32(p+20)
		mv := &mapVal{index: map[mapKey]int{}}
		for i := uint32(0); i < n; i++ {
			k, err := in.fromWasm(in.readSlot(ks+headerSize+8*i, *t.Key), *t.Key)
			if err != nil {
				return Value{}, err
			}
			v, err := in.fromWasm(in.readSlot(vs+headerSize+8*i, *t.E), *t.E)
			if err != nil {
				return Value{}, err
			}
			mv.index[keyOf(k)] = len(mv.keys)
			mv.keys = append(mv.keys, k)
			mv.vals = append(mv.vals, v)
		}
		return Value{kind: Map, ref: mv}, nil
	}
	cls := int(in.u32(p)&0x7fffffff) - tagClass
	if cls < 0 || cls >= len(in.script.meta.Classes) {
		return Value{}, errors.New("the script gave something that isn't an object")
	}
	if in.script.meta.Classes[cls].Native {
		obj := in.natObjs[int32(in.u32(p+16))]
		if obj == nil {
			return Value{}, errors.New("a host object the instance no longer knows")
		}
		return Value{kind: Object, ref: obj}, nil
	}
	h, err := in.helper("__handle_new", uint64(p))
	if err != nil {
		return Value{}, err
	}
	in.temp = append(in.temp, h)
	in.live[h] = true
	return Value{kind: Object, ref: &scriptRef{in: in, handle: h, class: cls}}, nil
}

// release forgets the native object the collector dropped.
func (in *Instance) release(nh int32) {
	if obj := in.natObjs[nh]; obj != nil {
		delete(in.natIdx, obj)
		delete(in.natObjs, nh)
		in.natFree = append(in.natFree, nh)
	}
}

// ---- the imports ----

// failure is the error the runtime reported with code, its arguments in the scratch slots.
func (in *Instance) failure(code int, pos int32) error {
	meta := in.script.meta
	msg := "the script failed"
	if code >= 0 && code < len(meta.Messages) {
		msg = formatMessage(meta.Messages[code], func(i int, kind byte) string {
			addr := uint32(scratchBase + 8*i)
			switch kind {
			case 'i':
				return strconv.Itoa(int(int32(in.u32(addr))))
			case 'n':
				v, _ := in.mem.ReadFloat64Le(addr)
				return numberText(v)
			case 's':
				return in.readString(in.u32(addr))
			case 'q':
				return quoteText(in.readString(in.u32(addr)))
			}
			return ""
		})
	}
	return meta.errorAt(pos, "%s", msg)
}

// native runs native id with what the script put in the scratch slots, and puts its result in slot 0.
func (in *Instance) native(id int, pos int32) error {
	s := in.script
	if id < 0 || id >= len(s.natives) {
		return s.meta.errorAt(pos, "no native %d", id)
	}
	nm := &s.meta.Natives[id]
	slot := 0
	raw := make([]uint64, 0, len(nm.Params)+1)
	if nm.This {
		raw = append(raw, uint64(in.u32(scratchBase)))
		slot = 1
	}
	for i, p := range nm.Params {
		raw = append(raw, in.readSlot(uint32(scratchBase+8*(slot+i)), p))
	}
	var this Value
	if nm.This {
		obj := in.natObjs[int32(in.u32(uint32(raw[0])+16))]
		if obj == nil {
			return s.meta.errorAt(pos, "%s: a host object the instance no longer knows", s.nativeNames[id])
		}
		this = Value{kind: Object, ref: obj}
		raw = raw[1:]
	}
	args := make([]Value, len(nm.Params))
	for i, p := range nm.Params {
		v, err := in.fromWasm(raw[i], p)
		if err != nil {
			return s.meta.errorAt(pos, "%s: %v", s.nativeNames[id], err)
		}
		args[i] = v
	}
	out, err := s.natives[id](&Call{data: in.data, in: in, result: nm.Result}, this, args)
	if err != nil {
		var se *ScriptError
		if errors.As(err, &se) {
			return se
		}
		return s.meta.errorAt(pos, "%s: %v", s.nativeNames[id], err)
	}
	if nm.Result.K == "v" {
		return nil
	}
	if !fitsMeta(out, nm.Result) {
		return s.meta.errorAt(pos, "%s gave a %s, not a %s", s.nativeNames[id], out.Kind(), nm.Result.Name)
	}
	r, err := in.toWasm(out, nm.Result)
	if err != nil {
		return s.meta.errorAt(pos, "%s: %v", s.nativeNames[id], err)
	}
	in.writeSlot(scratchBase, r, nm.Result)
	return nil
}

// ---- promises natives make ----

// MaxPendingPromises bounds the promises an instance's natives made that the host hasn't settled yet.
const MaxPendingPromises = 64

// promiseRef is a promise a native made, as the script gets it.
type promiseRef struct{ res *Resolver }

// Resolver settles a promise a native made (Call.NewPromise). Resolve and Reject run, under fresh budgets, what
// waited for it, so they are calls into the instance: they must come from the goroutine that uses it, between its
// other calls. A promise settles once; settling it again, or after the instance closed, does nothing.
type Resolver struct {
	in     *Instance
	handle int32 // 0 once it settled
	value  typeMeta
	err    *typeMeta
}

// NewPromise makes a pending promise for the native's result, which must be a Promise: the native returns the
// promise and keeps the resolver to settle it later. It fails when the instance has MaxPendingPromises already.
func (c *Call) NewPromise() (Value, *Resolver, error) {
	in := c.in
	if in == nil || c.result.K != "p" {
		return Value{}, nil, errors.New("only a native that returns a Promise makes one")
	}
	if in.pending >= MaxPendingPromises {
		return Value{}, nil, fmt.Errorf("more than %d promises are waiting for the host", MaxPendingPromises)
	}
	kind := promiseKindValue
	switch {
	case c.result.E.K == "s":
		kind = promiseKindString
	case isRefMeta(c.result.E) && c.result.E.K != "v":
		kind = promiseKindRef
	}
	p, err := in.helper("__promise_new", uint64(kind))
	if err != nil {
		return Value{}, nil, err
	}
	h, err := in.helper("__handle_new", uint64(uint32(p)))
	if err != nil {
		return Value{}, nil, err
	}
	in.pending++
	r := &Resolver{in: in, handle: h, value: *c.result.E, err: c.result.Err}
	return Value{kind: Object, ref: &promiseRef{res: r}}, r, nil
}

// Pending reports whether the promise hasn't settled.
func (r *Resolver) Pending() bool { return r.handle != 0 && r.in.mod != nil }

// Resolve fulfils the promise with v, a value of its type (anything for a Promise<void>), and runs what waited
// for it. The error is the script's, when what ran failed.
func (r *Resolver) Resolve(v Value) error {
	if !r.Pending() {
		return nil
	}
	if r.value.K == "v" {
		return r.settle("__promise_resolve_i", func() (uint64, error) { return 0, nil })
	}
	if !fitsMeta(v, r.value) {
		return fmt.Errorf("a %s is no %s", v.Kind(), r.value.Name)
	}
	export := "__promise_resolve_i"
	if r.value.K == "n" {
		export = "__promise_resolve_f"
	}
	return r.settle(export, func() (uint64, error) { return r.in.toWasm(v, r.value) })
}

// Reject fails the promise with e, an object of its error class, and runs what waited for it.
func (r *Resolver) Reject(e Value) error {
	if !r.Pending() {
		return nil
	}
	if r.err == nil {
		return errors.New("the promise can't fail")
	}
	obj, ok := e.ref.(*object)
	if !ok || e.kind != Object {
		return fmt.Errorf("a %s is no %s", e.Kind(), r.err.Name)
	}
	if cls, ok := r.in.script.classIndex[obj.class]; !ok || cls != r.err.C {
		return fmt.Errorf("a %s is no %s", obj.class.name, r.err.Name)
	}
	return r.settle("__promise_reject", func() (uint64, error) { return r.in.toWasm(e, *r.err) })
}

// settle calls the runtime's export with the promise and the value value gives, as one call from the host. A
// value that can't go into the script gives the promise up: it never settles, and no longer counts as pending.
func (r *Resolver) settle(export string, value func() (uint64, error)) error {
	in := r.in
	in.begin()
	raw, verr := value()
	p, err := in.helper("__handle_get", uint64(r.handle))
	_, _ = in.helper("__handle_drop", uint64(r.handle))
	r.handle = 0
	in.pending--
	if verr != nil {
		return verr
	}
	if err != nil {
		return err
	}
	in.stack[0], in.stack[1] = uint64(uint32(p)), raw
	in.dropTemps()
	if err := in.callExport(export, 2); err != nil {
		return err
	}
	return in.end()
}
