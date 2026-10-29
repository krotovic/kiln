package language

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"unicode"

	"adventurers-life-online/server/internal/engine/wasm"
)

// The code generator turns a checked script, every module of it, into one WebAssembly module (see abi.go for its
// interface, and runtime.wat for the runtime it takes the functions it uses from). Numbers are f64, bools i32,
// and strings, arrays, maps and objects i32 addresses of their blocks (0 for null).

//go:embed runtime.wat
var runtimeSource string

var (
	runtimeOnce  sync.Once
	runtimeFuncs map[string]*wasm.WatFunc
	runtimeErr   error
)

func runtimeLibrary() (map[string]*wasm.WatFunc, error) {
	runtimeOnce.Do(func() { runtimeFuncs, runtimeErr = wasm.ParseWat(runtimeSource) })
	return runtimeFuncs, runtimeErr
}

type posKey struct {
	m   *moduleInfo
	pos int
}

type gen struct {
	c       *checker
	limits  ScriptLimits
	entry   *moduleInfo
	m       *wasm.Module
	meta    *scriptMeta
	lib     map[string]*wasm.WatFunc
	imports map[string]uint32
	globals map[string]uint32

	rt      map[string]*wasm.Func // runtime functions, emitted or waiting
	rtQueue []string

	static  []byte // the static data, from staticBase
	strs    map[string]uint32
	symbols map[string]int64

	modIdx map[*moduleInfo]int
	posIdx map[posKey]int32

	classes  []*classInfo
	classIdx map[*classInfo]int

	funcs      map[*funcInfo]*wasm.Func // script code: functions, methods, constructors
	natives    map[*funcInfo]*wasm.Func // native functions and methods
	getters    map[*fieldInfo]*wasm.Func
	allocs     map[*classInfo]*wasm.Func
	constGlbs  map[*constInfo]uint32
	finals     map[*funcInfo]map[*classInfo]bool
	selectors  map[string]int
	selList    []selector
	nativeID   map[string]int
	fieldFuncs map[string]*wasm.Func
	trap       *wasm.Func

	promises bool // the script makes promises (gen_async.go)
	frames   map[*funcInfo]*frameInfo
	frameOf  map[*classInfo]*frameInfo
	resumes  map[*funcInfo]*wasm.Func
}

// selector is a name the dispatch table answers: a method, or a field's getter or setter, with the signature
// its entries have.
type selector struct {
	kind string // "m", "get" or "set"
	name string
	sig  wasm.FuncType
	typ  uint32
}

func wt(t *Type) wasm.ValType {
	if t.kind == tNumber {
		return wasm.F64
	}
	return wasm.I32
}

func sigOf(fn *funcInfo, this bool) wasm.FuncType {
	var s wasm.FuncType
	if this {
		s.Params = append(s.Params, wasm.I32)
	}
	for _, p := range fn.params {
		s.Params = append(s.Params, wt(p))
	}
	if fn.result != nil && fn.result.kind != tVoid {
		s.Results = []wasm.ValType{wt(fn.result)}
	}
	return s
}

// generate compiles the checked script whose entry is entry.
func (c *checker) generate(entry *moduleInfo) ([]byte, *scriptMeta, error) {
	lib, err := runtimeLibrary()
	if err != nil {
		return nil, nil, fmt.Errorf("the runtime: %w", err)
	}
	g := &gen{
		c: c, limits: c.limits, entry: entry, m: wasm.NewModule(), lib: lib,
		imports: map[string]uint32{}, globals: map[string]uint32{}, rt: map[string]*wasm.Func{},
		strs: map[string]uint32{}, symbols: map[string]int64{}, modIdx: map[*moduleInfo]int{}, posIdx: map[posKey]int32{},
		classIdx: map[*classInfo]int{}, funcs: map[*funcInfo]*wasm.Func{}, natives: map[*funcInfo]*wasm.Func{},
		getters: map[*fieldInfo]*wasm.Func{}, allocs: map[*classInfo]*wasm.Func{}, constGlbs: map[*constInfo]uint32{},
		finals: map[*funcInfo]map[*classInfo]bool{}, selectors: map[string]int{}, nativeID: map[string]int{},
		fieldFuncs: map[string]*wasm.Func{}, frames: map[*funcInfo]*frameInfo{}, frameOf: map[*classInfo]*frameInfo{},
		resumes: map[*funcInfo]*wasm.Func{},
		meta: &scriptMeta{ABI: abiVersion, Entry: entry.id, Limits: c.limits, Messages: errorMessages,
			Classes: []classMeta{}, Natives: []nativeMeta{}, Hosts: []hostMeta{}, Funcs: map[string]*funcMeta{},
			Exports: map[string]int{}},
	}
	return g.run()
}

func (g *gen) run() ([]byte, *scriptMeta, error) {
	i32, f64 := wasm.I32, wasm.F64
	imp := func(name string, params, results []wasm.ValType) {
		g.imports["imp_"+name] = g.m.ImportFunc("alo", name, wasm.FuncType{Params: params, Results: results})
	}
	imp(impFail, []wasm.ValType{i32, i32}, nil)
	imp(impNative, []wasm.ValType{i32, i32}, []wasm.ValType{i32})
	imp(impFmtNum, []wasm.ValType{f64}, []wasm.ValType{i32})
	imp(impParseNum, []wasm.ValType{i32, i32}, []wasm.ValType{i32})
	imp(impFmod, []wasm.ValType{f64, f64}, []wasm.ValType{f64})
	imp(impRelease, []wasm.ValType{i32}, nil)
	for _, name := range []string{"heap", "hp", "steps", "depth", "alloc", "since", "handles", "hfree", "hnext",
		"natives", "npos", "msp", "msphi", "qhead", "qtail"} {
		g.globals[name] = g.m.GlobalI32(true, 0)
	}
	g.setSymbols()

	for i, m := range g.c.order {
		g.modIdx[m] = i
		g.meta.Modules = append(g.meta.Modules, m.id)
	}
	g.pos(g.entry, 0) // position 0: somewhere in the entry
	g.collectClasses()
	g.declareFuncs()
	for _, m := range g.c.order {
		for _, k := range m.consts {
			if k.typ.kind == tNumber {
				g.constGlbs[k] = g.m.GlobalF64(true, 0)
			} else {
				g.constGlbs[k] = g.m.GlobalI32(true, 0)
			}
		}
	}
	if err := g.emitFuncs(); err != nil {
		return nil, nil, err
	}
	g.emitInit()
	g.emitExports()
	g.trap = g.m.Func("trap", wasm.FuncType{})
	g.trap.Unreachable()
	if err := g.drainRuntime(); err != nil {
		return nil, nil, err
	}
	g.emitTable()
	if err := g.drainRuntime(); err != nil {
		return nil, nil, err
	}

	heap := align(staticBase+len(g.static), 16)
	g.m.SetGlobalI32(g.globals["heap"], int32(heap))
	g.m.SetGlobalI32(g.globals["hp"], int32(heap))
	minPages := uint32(heap/pageSize + 1)
	maxPages := uint64(heap+pageSize)/pageSize + uint64(g.limits.MaxHeap)*7/2/pageSize + 2
	if maxPages > 65536 {
		maxPages = 65536
	}
	g.m.Memory(minPages, uint32(maxPages))
	g.m.Export("memory", wasm.ExportMemory, 0)
	if len(g.static) > 0 {
		g.m.Data(staticBase, g.static)
	}
	meta, err := json.Marshal(g.meta)
	if err != nil {
		return nil, nil, err
	}
	g.m.Custom(metaSection, meta)
	bin, err := g.m.Encode()
	return bin, g.meta, err
}

func align(n, a int) int { return (n + a - 1) / a * a }

// setSymbols gives the runtime its constants.
func (g *gen) setSymbols() {
	l := g.limits
	str := l.MaxString
	if str <= 0 {
		str = 1<<31 - 1
	}
	for k, v := range map[string]int64{
		"MAX_STEPS": int64(l.MaxSteps), "MAX_DEPTH": int64(l.MaxDepth), "MAX_LENGTH": int64(l.MaxLength),
		"MAX_STRING": int64(str), "MAX_HEAP": int64(l.MaxHeap), "HALF_HEAP": int64(l.MaxHeap / 2),
		"T_STRING": tagString, "T_ARR_VALS": tagArrVals, "T_ARR_REFS": tagArrRefs, "T_STORE": tagStore,
		"T_MAP": tagMap, "T_HANDLES": tagHandles, "T_NATIVES": tagNatives, "T_CLASS": tagClass,
		"UPPER_LOWER": upperLower,
		"E_STEPS":     errSteps, "E_DEPTH": errDepth, "E_MEMORY": errMemory, "E_LENGTH": errLength, "E_STRING": errString,
		"E_DIVZERO": errDivZero, "E_LARGE": errTooLarge, "E_INDEX": errIndex, "E_INDEX_SET": errIndexSet,
		"E_NULL": errNull, "E_NULL_WRITE": errNullWrite, "E_POP": errPopEmpty, "E_SLICE": errSliceWhole,
		"E_NOT_NUMBER": errNotNumber, "E_NOT_BOOL": errNotBool, "E_HOST_FIELD": errHostField,
	} {
		g.symbols[k] = v
	}
}

// Func, Global and Const resolve the runtime's names (wasm.Resolver).
func (g *gen) Func(name string) (uint32, bool) {
	if i, ok := g.imports[name]; ok {
		return i, true
	}
	return g.runtime(name), true
}

func (g *gen) Global(name string) (uint32, bool) {
	i, ok := g.globals[name]
	return i, ok
}

func (g *gen) Const(name string) (int64, bool) {
	if v, ok := g.symbols[name]; ok {
		return v, true
	}
	switch name {
	case "S_empty":
		return int64(g.str("")), true
	case "S_true":
		return int64(g.str("true")), true
	case "S_false":
		return int64(g.str("false")), true
	case "S_zero":
		return int64(g.str("0")), true
	case "S_negzero":
		return int64(g.str("-0")), true
	case "S_array":
		return int64(g.str("array")), true
	case "S_map":
		return int64(g.str("map")), true
	case "S_bool":
		return int64(g.str("bool")), true
	case "CLASSES":
		g.classTable()
		return g.symbols["CLASSES"], true
	case "CASES", "NCASES":
		g.caseTable()
		return g.symbols[name], true
	}
	return 0, false
}

// runtime gives the index of a runtime function, which is emitted once everything else is.
func (g *gen) runtime(name string) uint32 {
	if f, ok := g.rt[name]; ok {
		return f.Index
	}
	var t wasm.FuncType
	if name == "gc_consts" || name == "frame_resume" {
		t = wasm.FuncType{Params: []wasm.ValType{wasm.I32}}
	} else {
		w, ok := g.lib[name]
		if !ok {
			panic("language: no runtime function " + name)
		}
		t = w.Type()
	}
	g.rtQueue = append(g.rtQueue, name)
	f := g.m.Func(name, t)
	g.rt[name] = f
	return f.Index
}

func (g *gen) drainRuntime() error {
	for len(g.rtQueue) > 0 {
		name := g.rtQueue[0]
		g.rtQueue = g.rtQueue[1:]
		if name == "gc_consts" {
			g.emitGCConsts(g.rt[name])
			continue
		}
		if name == "frame_resume" {
			g.emitFrameResume(g.rt[name])
			continue
		}
		if err := g.lib[name].Emit(g.rt[name], g); err != nil {
			return err
		}
	}
	return nil
}

// static adds bytes to the static data, 8-aligned, and returns their address.
func (g *gen) addStatic(b []byte) uint32 {
	for len(g.static)%8 != 0 {
		g.static = append(g.static, 0)
	}
	addr := uint32(staticBase + len(g.static))
	g.static = append(g.static, b...)
	return addr
}

// str is the address of a static string block.
func (g *gen) str(s string) uint32 {
	if a, ok := g.strs[s]; ok {
		return a
	}
	b := make([]byte, headerSize+len(s))
	binary.LittleEndian.PutUint32(b[0:], tagString)
	binary.LittleEndian.PutUint32(b[4:], uint32(align(headerSize+len(s), 8)))
	binary.LittleEndian.PutUint32(b[12:], uint32(len(s)))
	copy(b[headerSize:], s)
	a := g.addStatic(b)
	g.strs[s] = a
	return a
}

// pos is the number of a position errors can name.
func (g *gen) pos(m *moduleInfo, pos int) int32 {
	k := posKey{m, pos}
	if i, ok := g.posIdx[k]; ok {
		return i
	}
	line, col := m.lineCol(pos)
	i := int32(len(g.posIdx))
	g.posIdx[k] = i
	g.meta.Positions = append(g.meta.Positions, int32(g.modIdx[m]), int32(line), int32(col))
	return i
}

// collectClasses numbers every class: the scripts' in module order, then the host modules' native ones.
func (g *gen) collectClasses() {
	add := func(ci *classInfo) {
		g.classIdx[ci] = len(g.classes)
		g.classes = append(g.classes, ci)
	}
	for _, m := range g.c.order {
		for _, d := range m.ast.decls {
			if d.sym != nil && d.class != nil {
				add(d.sym.class)
			}
		}
	}
	var hosts []string
	for id, m := range g.c.modules {
		if m.host != nil {
			hosts = append(hosts, id)
		}
	}
	sort.Strings(hosts)
	for _, id := range hosts {
		m := g.c.modules[id]
		sum := sha256.Sum256([]byte(m.host.Source))
		g.meta.Hosts = append(g.meta.Hosts, hostMeta{Name: id, Hash: hex.EncodeToString(sum[:8])})
		for _, d := range m.ast.decls {
			if d.sym != nil && d.class != nil {
				add(d.sym.class)
			}
		}
	}
	g.collectAsync(add)
	for _, ci := range g.classes {
		g.meta.Classes = append(g.meta.Classes, g.classMeta(ci))
	}
	for name, sym := range g.entry.exports {
		if sym.kind == symClass && !sym.class.native {
			g.meta.Exports[name] = g.classIdx[sym.class]
		}
	}
}

func (g *gen) typeMeta(t *Type) typeMeta {
	tm := typeMeta{Name: t.String()}
	switch t.kind {
	case tNumber:
		tm.K = "n"
	case tString:
		tm.K = "s"
	case tBool:
		tm.K = "b"
	case tVoid:
		tm.K = "v"
	case tArray:
		e := g.typeMeta(t.elem)
		tm.K, tm.E = "a", &e
	case tMap:
		k, e := g.typeMeta(t.key), g.typeMeta(t.elem)
		tm.K, tm.Key, tm.E = "m", &k, &e
	case tClass:
		tm.K, tm.C = "c", g.classIdx[t.class]
	case tPromise:
		e := g.typeMeta(t.elem)
		tm.K, tm.E = "p", &e
		if t.key != nil {
			x := g.typeMeta(t.key)
			tm.Err = &x
		}
	default:
		tm.K = "i"
	}
	return tm
}

func (g *gen) typeMetas(ts []*Type) []typeMeta {
	out := make([]typeMeta, len(ts))
	for i, t := range ts {
		out[i] = g.typeMeta(t)
	}
	return out
}

func classSize(ci *classInfo) int {
	if ci.native {
		return headerSize + 8
	}
	return headerSize + 8*len(ci.fields)
}

func (g *gen) classMeta(ci *classInfo) classMeta {
	cm := classMeta{Name: ci.name, Module: ci.mod.id, Native: ci.native, Size: classSize(ci), Fields: []fieldMeta{},
		Methods: map[string]*methodMeta{}, Ctor: []typeMeta{}}
	if ci.hidden {
		return cm
	}
	if ci.mod.host == nil {
		cm.Pos = g.pos(ci.mod, ci.pos)
	}
	for _, f := range ci.fields {
		cm.Fields = append(cm.Fields, fieldMeta{Name: f.name, Type: g.typeMeta(f.typ), Canon: canon(f.typ),
			Offset: headerSize + 8*f.slot, Private: f.private, Readonly: f.readonly})
	}
	for name, fn := range ci.methods {
		mm := &methodMeta{Params: g.typeMetas(fn.params), Result: g.typeMeta(fn.result), Private: fn.private,
			Sig: fn.signature(), Canon: canonSig(fn)}
		if !ci.native && !fn.private {
			mm.Export = fmt.Sprintf("m%d.%s", g.classIdx[ci], name)
		}
		cm.Methods[name] = mm
	}
	if ci.ctor != nil {
		cm.Ctor = g.typeMetas(ci.ctor.params)
	}
	return cm
}

// classTable writes, for the collector, how many slots each class's objects have and which hold references.
func (g *gen) classTable() {
	if _, ok := g.symbols["CLASSES"]; ok {
		return
	}
	flags := make([]uint32, len(g.classes))
	for i, ci := range g.classes {
		var b []byte
		if !ci.native {
			b = make([]byte, len(ci.fields))
			for j, f := range ci.fields {
				if f.typ.isRef() || f.typ.kind == tString {
					b[j] = 1
				}
			}
		}
		if len(b) == 0 {
			b = []byte{0}
		}
		flags[i] = g.addStatic(b)
	}
	table := make([]byte, 8*len(g.classes)+8)
	for i, ci := range g.classes {
		n := len(ci.fields)
		if ci.native {
			n = 1
		}
		binary.LittleEndian.PutUint32(table[8*i:], uint32(n))
		binary.LittleEndian.PutUint32(table[8*i+4:], flags[i])
	}
	g.symbols["CLASSES"] = int64(g.addStatic(table))
}

// caseTable writes Go's unicode.CaseRanges: lo, hi, and the upper and lower case deltas.
func (g *gen) caseTable() {
	if _, ok := g.symbols["CASES"]; ok {
		return
	}
	b := make([]byte, 16*len(unicode.CaseRanges))
	for i, cr := range unicode.CaseRanges {
		binary.LittleEndian.PutUint32(b[16*i:], cr.Lo)
		binary.LittleEndian.PutUint32(b[16*i+4:], cr.Hi)
		binary.LittleEndian.PutUint32(b[16*i+8:], uint32(int32(cr.Delta[unicode.UpperCase])))
		binary.LittleEndian.PutUint32(b[16*i+12:], uint32(int32(cr.Delta[unicode.LowerCase])))
	}
	g.symbols["CASES"] = int64(g.addStatic(b))
	g.symbols["NCASES"] = int64(len(unicode.CaseRanges))
}

// declareFuncs makes a function for every function, method and constructor of the scripts, so code may call any
// of them before it is emitted.
func (g *gen) declareFuncs() {
	for _, m := range g.c.order {
		for _, d := range m.ast.decls {
			switch {
			case d.sym == nil:
			case d.fn != nil && !d.native:
				fn := d.sym.fn
				g.funcs[fn] = g.m.Func(m.id+"."+fn.name, sigOf(fn, false))
			case d.class != nil && !d.sym.class.native:
				ci := d.sym.class
				for _, md := range d.class.methods {
					if fn := ci.methods[md.name]; fn != nil && fn.owner == ci {
						g.funcs[fn] = g.m.Func(ci.name+"."+fn.name, sigOf(fn, true))
					}
				}
				g.funcs[ci.ctor] = g.m.Func(ci.name+".constructor", sigOf(ci.ctor, true))
			}
		}
	}
	for fn, info := range g.frames {
		g.resumes[fn] = g.m.Func(info.class.name+" resume", wasm.FuncType{Params: []wasm.ValType{wasm.I32}})
	}
}

func (g *gen) emitFuncs() error {
	for _, m := range g.c.order {
		for _, d := range m.ast.decls {
			switch {
			case d.sym == nil:
			case d.fn != nil && !d.native && d.sym.fn.async:
				g.emitAsync(d.sym.fn, nil)
			case d.fn != nil && !d.native:
				fn := d.sym.fn
				g.emitBody(g.funcs[fn], fn, nil, fn.body)
			case d.class != nil && !d.sym.class.native:
				ci := d.sym.class
				for _, md := range d.class.methods {
					if fn := ci.methods[md.name]; fn != nil && fn.owner == ci && fn.async {
						g.emitAsync(fn, ci)
					} else if fn != nil && fn.owner == ci {
						g.emitBody(g.funcs[fn], fn, ci, fn.body)
					}
				}
				g.emitBody(g.funcs[ci.ctor], ci.ctor, ci, ci.ctor.body)
			}
		}
	}
	return nil
}

// funcFor is the function that runs fn: its code, or the native that calls the host.
func (g *gen) funcFor(fn *funcInfo) *wasm.Func {
	if fn.native != nil {
		return g.nativeFunc(fn)
	}
	return g.funcs[fn]
}

// final reports whether a call of fn on an object whose class is static (or derives from it) always runs fn:
// no class overrides it.
func (g *gen) final(static *classInfo, fn *funcInfo) bool {
	byClass := g.finals[fn]
	if byClass == nil {
		byClass = map[*classInfo]bool{}
		g.finals[fn] = byClass
	}
	if v, ok := byClass[static]; ok {
		return v
	}
	v := true
	for _, k := range g.classes {
		if k.isA(static) && k.methods[fn.name] != fn {
			v = false
			break
		}
	}
	byClass[static] = v
	return v
}

// nativeFunc is the function that calls the host for a native function or method.
func (g *gen) nativeFunc(fn *funcInfo) *wasm.Func {
	if f, ok := g.natives[fn]; ok {
		return f
	}
	this := fn.owner != nil
	name := fn.name
	if this {
		name = fn.owner.name + "." + fn.name
	}
	f := g.m.Func("native "+name, sigOf(fn, this))
	g.natives[fn] = f
	id := g.nativeMeta(fn.mod.id, name, this, fn.params, fn.result)
	g.emitNative(f, id, true)
	return f
}

// getterFunc is the function that reads a native class's field through the host.
func (g *gen) getterFunc(fi *fieldInfo) *wasm.Func {
	if f, ok := g.getters[fi]; ok {
		return f
	}
	name := fi.owner.name + "." + fi.name
	f := g.m.Func("native "+name, wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wt(fi.typ)}})
	g.getters[fi] = f
	id := g.nativeMeta(fi.mod.id, name, true, nil, fi.typ)
	g.emitNative(f, id, false)
	return f
}

func (g *gen) nativeMeta(host, name string, this bool, params []*Type, result *Type) int {
	key := host + ":" + name
	if id, ok := g.nativeID[key]; ok {
		return id
	}
	id := len(g.meta.Natives)
	g.nativeID[key] = id
	g.meta.Natives = append(g.meta.Natives, nativeMeta{Host: host, Name: name, This: this, Params: g.typeMetas(params),
		Result: g.typeMeta(result)})
	return id
}

// emitNative writes a function that passes its parameters to the host's native id through the scratch slots,
// and returns what the host left in slot 0. A call counts against the budgets as a script's does (counted is
// false for a field). Its errors name where the script called it: $npos, set at the call.
func (g *gen) emitNative(f *wasm.Func, id int, counted bool) {
	sig := f.Sig()
	npos := g.globals["npos"]
	if counted {
		g.prologue(f, -1)
	}
	for i, p := range sig.Params {
		f.I32Const(0).LocalGet(uint32(i))
		if p == wasm.F64 {
			f.F64Store(uint32(scratchBase + 8*i))
		} else {
			f.I32Store(uint32(scratchBase + 8*i))
		}
	}
	f.I32Const(int32(id)).GlobalGet(npos).Call(g.imports["imp_"+impNative])
	f.If(wasm.Void)
	f.Unreachable()
	f.End()
	if counted {
		g.leave(f)
	}
	if len(sig.Results) == 1 {
		f.I32Const(0)
		if sig.Results[0] == wasm.F64 {
			f.F64Load(scratchBase)
		} else {
			f.I32Load(scratchBase)
		}
	}
}

// prologue counts a call: one step and one level of nesting. pos -1 takes the position from $npos.
func (g *gen) prologue(f *wasm.Func, pos int32) {
	steps, depth := g.globals["steps"], g.globals["depth"]
	posArg := func() {
		if pos < 0 {
			f.GlobalGet(g.globals["npos"])
		} else {
			f.I32Const(pos)
		}
	}
	f.GlobalGet(steps).I32Const(1).Op(wasm.OpI32Sub).GlobalSet(steps)
	f.GlobalGet(steps).I32Const(0).Op(wasm.OpI32LtS)
	f.If(wasm.Void)
	posArg()
	f.Call(g.runtime("fail_steps"))
	f.End()
	f.GlobalGet(depth).I32Const(1).Op(wasm.OpI32Add).GlobalSet(depth)
	f.GlobalGet(depth).I32Const(int32(g.limits.MaxDepth)).Op(wasm.OpI32GtS)
	f.If(wasm.Void)
	posArg()
	f.Call(g.runtime("fail_depth"))
	f.End()
}

// leave ends a call's level of nesting.
func (g *gen) leave(f *wasm.Func) {
	depth := g.globals["depth"]
	f.GlobalGet(depth).I32Const(1).Op(wasm.OpI32Sub).GlobalSet(depth)
}

// allocFunc makes an object of a script class with its fields at their zero values.
func (g *gen) allocFunc(ci *classInfo) *wasm.Func {
	if f, ok := g.allocs[ci]; ok {
		return f
	}
	f := g.m.Func("new "+ci.name, wasm.FuncType{Results: []wasm.ValType{wasm.I32}})
	g.allocs[ci] = f
	o := f.Local(wasm.I32)
	f.I32Const(int32(classSize(ci))).I32Const(int32(tagClass + g.classIdx[ci])).I32Const(g.pos(ci.mod, ci.pos))
	f.Call(g.runtime("alloc")).LocalSet(o)
	for _, fi := range ci.fields {
		if fi.typ.kind == tString {
			f.LocalGet(o).I32Const(int32(g.str(""))).I32Store(uint32(headerSize + 8*fi.slot))
		}
	}
	f.LocalGet(o)
	return f
}

// selector numbers a name the dispatch table answers.
func (g *gen) selector(kind, name string, sig wasm.FuncType) int {
	key := kind + " " + name + " " + fmt.Sprint(sig.Params, sig.Results)
	if i, ok := g.selectors[key]; ok {
		return i
	}
	i := len(g.selList)
	g.selectors[key] = i
	g.selList = append(g.selList, selector{kind: kind, name: name, sig: sig, typ: g.m.Type(sig)})
	return i
}

// emitTable fills the dispatch table: for each selector, a row with every class's entry.
func (g *gen) emitTable() {
	if len(g.selList) == 0 {
		return
	}
	n := len(g.classes)
	elems := make([]uint32, len(g.selList)*n)
	for si, s := range g.selList {
		for ci, k := range g.classes {
			f := g.entryFor(s, k)
			if f == nil {
				f = g.trap
			}
			elems[si*n+ci] = f.Index
		}
	}
	g.m.Table(elems)
}

func sameSig(a, b wasm.FuncType) bool {
	return fmt.Sprint(a.Params, a.Results) == fmt.Sprint(b.Params, b.Results)
}

// entryFor is the function class k runs for selector s, or nil.
func (g *gen) entryFor(s selector, k *classInfo) *wasm.Func {
	switch s.kind {
	case "m":
		fn := k.methods[s.name]
		if fn == nil || fn.private || !sameSig(sigOf(fn, true), s.sig) {
			return nil
		}
		return g.funcFor(fn)
	case "resume":
		if info := g.frameOf[k]; info != nil {
			return g.resumes[info.fn]
		}
		return nil
	case "get", "set":
		fi := k.fieldByName[s.name]
		if fi == nil {
			return nil
		}
		want := wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wt(fi.typ)}}
		if s.kind == "set" {
			want = wasm.FuncType{Params: []wasm.ValType{wasm.I32, wt(fi.typ)}}
		}
		if !sameSig(want, s.sig) {
			return nil
		}
		return g.fieldFunc(s.kind, k, fi)
	}
	return nil
}

// fieldFunc reads (get) or sets (set) field fi of an object of class k, for interfaces.
func (g *gen) fieldFunc(kind string, k *classInfo, fi *fieldInfo) *wasm.Func {
	key := fmt.Sprintf("%s %d %s", kind, g.classIdx[k], fi.name)
	if f, ok := g.fieldFuncs[key]; ok {
		return f
	}
	if kind == "get" && fi.getter != nil {
		f := g.getterFunc(fi)
		g.fieldFuncs[key] = f
		return f
	}
	var f *wasm.Func
	off := uint32(headerSize + 8*fi.slot)
	if kind == "get" {
		f = g.m.Func(key, wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wt(fi.typ)}})
		f.LocalGet(0)
		load(f, fi.typ, off)
	} else {
		f = g.m.Func(key, wasm.FuncType{Params: []wasm.ValType{wasm.I32, wt(fi.typ)}})
		if fi.getter != nil {
			f.I32Const(int32(g.str(fi.name))).GlobalGet(g.globals["npos"]).Call(g.runtime("fail_hostfield"))
		} else {
			f.LocalGet(0).LocalGet(1)
			store(f, fi.typ, off)
		}
	}
	g.fieldFuncs[key] = f
	return f
}

func load(f *wasm.Func, t *Type, off uint32) {
	if t.kind == tNumber {
		f.F64Load(off)
	} else {
		f.I32Load(off)
	}
}

func store(f *wasm.Func, t *Type, off uint32) {
	if t.kind == tNumber {
		f.F64Store(off)
	} else {
		f.I32Store(off)
	}
}

// emitInit writes __init: the runtime's tables, then every module's constants, in order.
func (g *gen) emitInit() {
	f := g.m.Func("__init", wasm.FuncType{})
	f.Call(g.runtime("rt_init"))
	for _, m := range g.c.order {
		fg := g.newFgen(f, nil, nil, m)
		for _, k := range m.consts {
			fg.expr(k.init)
			f.GlobalSet(g.constGlbs[k])
		}
	}
	g.m.Export("__init", wasm.ExportFunc, f.Index)
}

// emitGCConsts writes $gc_consts: with 0 it marks the module constants that are strings, with 1 it points them
// where the collector moves their strings.
func (g *gen) emitGCConsts(f *wasm.Func) {
	var refs []uint32
	for _, m := range g.c.order {
		for _, k := range m.consts {
			if k.typ.kind == tString {
				refs = append(refs, g.constGlbs[k])
			}
		}
	}
	f.LocalGet(0)
	f.If(wasm.Void)
	for _, gl := range refs {
		f.GlobalGet(gl).Call(g.runtime("gc_fwd")).GlobalSet(gl)
	}
	f.Else()
	for _, gl := range refs {
		f.GlobalGet(gl).Call(g.runtime("gc_push"))
	}
	f.End()
}

func (g *gen) emitExports() {
	for _, name := range []string{"__begin", "__end", "__gc", "__str_new", "__arr_new", "__map_new", "__map_put",
		"__handle_new", "__handle_get", "__handle_drop", "__native_new", "__native_get"} {
		g.m.Export(name, wasm.ExportFunc, g.runtime(name))
	}
	if g.promises {
		for _, name := range []string{"__promise_new", "__promise_resolve_f", "__promise_resolve_i", "__promise_reject"} {
			g.m.Export(name, wasm.ExportFunc, g.runtime(name))
		}
	}
	for i, ci := range g.classes {
		if ci.native || ci.hidden {
			continue
		}
		g.m.Export(fmt.Sprintf("n%d", i), wasm.ExportFunc, g.allocFunc(ci).Index)
		g.m.Export(fmt.Sprintf("c%d", i), wasm.ExportFunc, g.funcs[ci.ctor].Index)
		names := make([]string, 0, len(ci.methods))
		for name := range ci.methods {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if fn := ci.methods[name]; !fn.private {
				g.m.Export(fmt.Sprintf("m%d.%s", i, name), wasm.ExportFunc, g.funcFor(fn).Index)
			}
		}
	}
	names := make([]string, 0, len(g.entry.exports))
	for name := range g.entry.exports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sym := g.entry.exports[name]
		if sym.kind != symFunc || sym.fn.native != nil || g.funcs[sym.fn] == nil {
			continue
		}
		export := "f." + name
		g.m.Export(export, wasm.ExportFunc, g.funcs[sym.fn].Index)
		g.meta.Funcs[name] = &funcMeta{Params: g.typeMetas(sym.fn.params), Result: g.typeMeta(sym.fn.result), Export: export}
	}
}
