package language

import (
	"adventurers-life-online/server/internal/engine/wasm"
)

// Async functions compile to state machines. Calling one makes its frame, an object of a hidden class that keeps
// its variables while it waits, and its promise, and runs its resume function; that one runs the body until it
// returns, which settles the promise, or until an await finds its promise pending: it then saves the variables
// in the frame, notes which await it waits at, puts the frame on the promise's waiters, and returns. Settling a
// promise queues its waiters, and the runtime resumes them ($promise_drain): the resume function restores the
// variables and goes through the body again to that await, skipping every statement before it, entering only the
// branch and the loop that hold it. awaits are whole statements, so nothing waits in the middle of an
// expression, and every await is numbered: the statements that hold awaits know the range of their numbers.

// A frame's slots: the await it waits at (0 before the first), its promise, the next frame waiting on the same
// promise (or in the run queue), the promise it awaits, this, then its variables and the saved temps of the
// for … of loops that await.
const (
	frameState = iota
	framePromise
	frameNext
	frameAwaiting
	frameThis
	frameVars
)

// A promise's slots: its state (0 pending, 1 fulfilled, 2 rejected), its value, its error and the first frame
// waiting on it.
const (
	promiseState = iota
	promiseValue
	promiseError
	promiseWaiters
	promiseSlots
)

// Kinds of promises the runtime makes ($promise_new): of a number, bool or nothing; of a string; of another
// reference.
const (
	promiseKindValue = iota
	promiseKindString
	promiseKindRef
)

// typRefSlot types the hidden classes' slots that hold references, for the collector.
var typRefSlot = arrayOf(typNumber)

func slotOff(i int) uint32 { return uint32(headerSize + 8*i) }

// frameInfo is an async function's frame class, and where its body's awaits are.
type frameInfo struct {
	fn    *funcInfo
	class *classInfo
	// forOfs are the for … of loops that await: the slot of the first of their four saved temps
	forOfs map[*irForOf]int
	// ids number the awaits from 1, in order; ranges are the first and last numbers a statement holds
	ids    map[*irAwait]int
	ranges map[irStmt][2]int
}

// asyncGen is what an async function's resume function is written with.
type asyncGen struct {
	info  *frameInfo
	frame uint32 // the local of the frame: the resume function's parameter
	rs    uint32 // the await it resumes at, 0 once it runs again
	temps map[*irForOf][4]uint32
}

// promiseKind is the kind of promise of value type t.
func promiseKind(t *Type) int32 {
	switch {
	case t.kind == tString:
		return promiseKindString
	case t.isRef():
		return promiseKindRef
	}
	return promiseKindValue
}

// hiddenClass makes a class the compiler uses itself, with a slot of each type.
func hiddenClass(name string, m *moduleInfo, slots []*Type) *classInfo {
	ci := &classInfo{name: name, mod: m, hidden: true, fieldByName: map[string]*fieldInfo{}, methods: map[string]*funcInfo{}}
	for i, t := range slots {
		ci.fields = append(ci.fields, &fieldInfo{name: name, typ: t, slot: i, owner: ci, mod: m})
	}
	ci.typ = &Type{kind: tClass, class: ci}
	return ci
}

// usesPromises reports whether the script makes promises: it has async functions, or calls natives that may
// return a promise.
func (g *gen) usesPromises() bool {
	for _, m := range g.c.order {
		for _, d := range m.ast.decls {
			switch {
			case d.sym == nil:
			case d.fn != nil && d.sym.fn.async:
				return true
			case d.class != nil:
				for _, fn := range d.sym.class.methods {
					if fn.async {
						return true
					}
				}
			}
		}
	}
	for _, m := range g.c.modules {
		if m.host == nil {
			continue
		}
		for _, sym := range m.symbols {
			switch {
			case sym.kind == symFunc && sym.fn.result.kind == tPromise:
				return true
			case sym.kind == symClass:
				for _, fn := range sym.class.methods {
					if fn.result.kind == tPromise {
						return true
					}
				}
			}
		}
	}
	return false
}

// collectAsync makes the hidden classes of promises and of the async functions' frames.
func (g *gen) collectAsync(add func(*classInfo)) {
	if !g.usesPromises() {
		return
	}
	g.promises = true
	n := hiddenClass("Promise", g.entry, []*Type{typBool, typNumber, typRefSlot, typRefSlot})
	r := hiddenClass("Promise", g.entry, []*Type{typBool, typRefSlot, typRefSlot, typRefSlot})
	add(n)
	add(r)
	g.symbols["T_PROMISE_N"] = int64(tagClass + g.classIdx[n])
	g.symbols["T_PROMISE_R"] = int64(tagClass + g.classIdx[r])
	for _, m := range g.c.order {
		for _, d := range m.ast.decls {
			switch {
			case d.sym == nil:
			case d.fn != nil && !d.native && d.sym.fn.async:
				add(g.frameClass(d.sym.fn))
			case d.class != nil && !d.sym.class.native:
				ci := d.sym.class
				for _, md := range d.class.methods {
					if fn := ci.methods[md.name]; fn != nil && fn.owner == ci && fn.async {
						add(g.frameClass(fn))
					}
				}
			}
		}
	}
}

// frameClass makes the frame class of an async function, numbering its awaits.
func (g *gen) frameClass(fn *funcInfo) *classInfo {
	info := &frameInfo{fn: fn, forOfs: map[*irForOf]int{}, ids: map[*irAwait]int{}, ranges: map[irStmt][2]int{}}
	slots := []*Type{typBool, typRefSlot, typRefSlot, typRefSlot, typRefSlot}
	slots = append(slots, fn.locals...)
	var walk func(s irStmt) (int, int)
	walk = func(s irStmt) (int, int) {
		lo, hi := 0, 0
		span := func(l, h int) {
			if l == 0 {
				return
			}
			if lo == 0 {
				lo = l
			}
			hi = h
		}
		switch s := s.(type) {
		case *irBlock:
			for _, x := range s.stmts {
				span(walk(x))
			}
		case *irIf:
			span(walk(s.then))
			if s.els != nil {
				span(walk(s.els))
			}
		case *irLoop:
			span(walk(s.body))
		case *irForOf:
			span(walk(s.body))
			if lo != 0 {
				info.forOfs[s] = len(slots)
				slots = append(slots, typRefSlot, typBool, typRefSlot, typBool)
			}
		case *irAwait:
			id := len(info.ids) + 1
			info.ids[s] = id
			span(id, id)
		}
		if lo != 0 {
			info.ranges[s] = [2]int{lo, hi}
		}
		return lo, hi
	}
	walk(fn.body)
	name := "async " + fn.name
	if fn.owner != nil {
		name = "async " + fn.owner.name + "." + fn.name
	}
	info.class = hiddenClass(name, fn.mod, slots)
	g.frames[fn] = info
	g.frameOf[info.class] = info
	return info.class
}

// emitAsync writes an async function: the function the script calls, which makes the frame and the promise and
// runs the body, and the resume function, which runs it from where the frame waits.
func (g *gen) emitAsync(fn *funcInfo, class *classInfo) {
	info := g.frames[fn]
	f, pos := g.funcs[fn], g.pos(fn.mod, fn.pos)
	frame, p := f.Local(wasm.I32), f.Local(wasm.I32)
	g.prologue(f, pos)
	f.I32Const(int32(classSize(info.class))).I32Const(int32(tagClass + g.classIdx[info.class])).I32Const(pos)
	f.Call(g.runtime("alloc")).LocalSet(frame)
	f.I32Const(promiseKind(fn.result.elem)).I32Const(pos).Call(g.runtime("promise_new")).LocalSet(p)
	f.LocalGet(frame).LocalGet(p).I32Store(slotOff(framePromise))
	first := uint32(0)
	if class != nil {
		f.LocalGet(frame).LocalGet(0).I32Store(slotOff(frameThis))
		first = 1
	}
	for i, t := range fn.params {
		f.LocalGet(frame).LocalGet(first + uint32(i))
		store(f, t, slotOff(frameVars+i))
	}
	f.LocalGet(frame).Call(g.resumes[fn].Index)
	g.leave(f)
	f.LocalGet(p)

	r := g.resumes[fn]
	fg := g.newFgen(r, fn, class, fn.mod)
	fg.result = fn.result
	ag := &asyncGen{info: info, frame: 0, temps: map[*irForOf][4]uint32{}}
	fg.async = ag
	if class != nil {
		fg.self = r.Local(wasm.I32)
	}
	for _, t := range fn.locals {
		fg.slots = append(fg.slots, r.Local(wt(t)))
	}
	for s := range info.forOfs {
		ag.temps[s] = [4]uint32{r.Local(wasm.I32), r.Local(wasm.I32), r.Local(wasm.I32), r.Local(wasm.I32)}
	}
	ag.rs = r.Local(wasm.I32)
	g.prologue(r, pos)
	if class != nil {
		r.LocalGet(ag.frame).I32Load(slotOff(frameThis)).LocalSet(fg.self)
	}
	fg.saved(false)
	r.LocalGet(ag.frame).I32Load(slotOff(frameState)).LocalSet(ag.rs)
	fg.astmt(fn.body)
	fg.settle(nil, false)
	g.leave(r)
}

// saved stores the variables and the awaiting loops' temps in the frame (save), or restores them.
func (fg *fgen) saved(save bool) {
	f, ag := fg.f, fg.async
	move := func(local uint32, t *Type, slot int) {
		if save {
			f.LocalGet(ag.frame).LocalGet(local)
			store(f, t, slotOff(slot))
		} else {
			f.LocalGet(ag.frame)
			load(f, t, slotOff(slot))
			f.LocalSet(local)
		}
	}
	for i, t := range fg.fn.locals {
		move(fg.slots[i], t, frameVars+i)
	}
	for s, first := range ag.info.forOfs {
		for k, l := range ag.temps[s] {
			move(l, typBool, first+k)
		}
	}
}

// settle settles the function's promise: with the value value pushes (nil: the zero value), or with reject,
// with the error it pushes. It doesn't return.
func (fg *fgen) settle(value irExpr, reject bool) {
	f := fg.f
	t := fg.fn.result.elem
	f.LocalGet(fg.async.frame).I32Load(slotOff(framePromise))
	switch {
	case reject:
		fg.expr(value)
		fg.call("promise_reject")
		return
	case value != nil:
		fg.expr(value)
	case t.kind == tVoid:
		f.I32Const(0)
	default:
		fg.zero(t)
	}
	if t.kind == tNumber {
		fg.call("promise_fulfil_f")
	} else {
		fg.call("promise_fulfil_i")
	}
}

// whenRunning writes code that runs only when the function isn't resuming: it skips what comes before the await
// it resumes at.
func (fg *fgen) whenRunning(code func()) {
	f := fg.f
	f.LocalGet(fg.async.rs).Op(wasm.OpI32Eqz)
	f.If(wasm.Void)
	code()
	f.End()
}

// inRange pushes whether the await the function resumes at is one of lo..hi.
func (fg *fgen) inRange(r [2]int) {
	f, rs := fg.f, fg.async.rs
	f.LocalGet(rs).I32Const(int32(r[0])).Op(wasm.OpI32GeS)
	f.LocalGet(rs).I32Const(int32(r[1])).Op(wasm.OpI32LeS)
	f.Op(wasm.OpI32And)
}

// astmt writes a statement of an async function's body: one without awaits only runs when not resuming; one
// with awaits runs when not resuming or when it holds the await it resumes at, entering what holds it.
func (fg *fgen) astmt(s irStmt) {
	f, ag := fg.f, fg.async
	r, has := ag.info.ranges[s]
	if !has {
		fg.whenRunning(func() { fg.stmt(s) })
		return
	}
	if b, ok := s.(*irBlock); ok {
		for _, x := range b.stmts {
			fg.astmt(x)
		}
		return
	}
	f.LocalGet(ag.rs).Op(wasm.OpI32Eqz)
	fg.inRange(r)
	f.Op(wasm.OpI32Or)
	f.If(wasm.Void)
	switch s := s.(type) {
	case *irIf:
		f.LocalGet(ag.rs).Op(wasm.OpI32Eqz)
		f.If(wasm.Result(wasm.I32))
		fg.step(s.pos)
		fg.expr(s.cond)
		f.Else()
		if tr, ok := ag.info.ranges[s.then]; ok {
			fg.inRange(tr)
		} else {
			f.I32Const(0)
		}
		f.End()
		f.If(wasm.Void)
		fg.astmt(s.then)
		if s.els != nil {
			f.Else()
			fg.astmt(s.els)
		}
		f.End()
	case *irLoop:
		if s.init != nil {
			fg.whenRunning(func() { fg.stmt(s.init) })
		}
		exit := f.Block(wasm.Void)
		top := f.Loop(wasm.Void)
		fg.whenRunning(func() {
			fg.step(s.pos)
			if s.cond != nil {
				fg.expr(s.cond)
				f.Op(wasm.OpI32Eqz).BrIf(exit)
			}
		})
		next := f.Block(wasm.Void)
		fg.loops = append(fg.loops, loopLabels{exit: exit, next: next})
		fg.astmt(s.body)
		fg.loops = fg.loops[:len(fg.loops)-1]
		f.End()
		if s.step != nil {
			fg.stmt(s.step)
		}
		f.Br(top)
		f.End()
		f.End()
	case *irForOf:
		t := ag.temps[s]
		a, n, st, i := t[0], t[1], t[2], t[3]
		fg.whenRunning(func() {
			fg.step(s.pos)
			fg.expr(s.iter)
			f.LocalTee(a)
			f.If(wasm.Void)
			f.LocalGet(a).I32Load(12).LocalSet(n)
			f.LocalGet(a).I32Load(16).LocalSet(st)
			f.Else()
			f.I32Const(0).LocalSet(n)
			f.End()
			f.I32Const(0).LocalSet(i)
		})
		exit := f.Block(wasm.Void)
		top := f.Loop(wasm.Void)
		fg.whenRunning(func() {
			f.LocalGet(i).LocalGet(n).Op(wasm.OpI32GeU).BrIf(exit)
			fg.step(s.pos)
			f.LocalGet(st).LocalGet(i).I32Const(3).Op(wasm.OpI32Shl).Op(wasm.OpI32Add)
			load(f, s.elem, headerSize)
			f.LocalSet(fg.slots[s.slot])
		})
		next := f.Block(wasm.Void)
		fg.loops = append(fg.loops, loopLabels{exit: exit, next: next})
		fg.astmt(s.body)
		fg.loops = fg.loops[:len(fg.loops)-1]
		f.End()
		f.LocalGet(i).I32Const(1).Op(wasm.OpI32Add).LocalSet(i)
		f.Br(top)
		f.End()
		f.End()
	case *irAwait:
		fg.await(s)
	default:
		f.Unreachable()
	}
	f.End()
}

// await writes an await: when running, it evaluates the promise and, when it is pending, waits for it; then
// (at once, or once resumed there) it takes what the promise settled with.
func (fg *fgen) await(s *irAwait) {
	f, g, ag := fg.f, fg.g, fg.async
	id := int32(ag.info.ids[s])
	p := fg.tmp(wasm.I32)
	fg.whenRunning(func() {
		fg.step(s.pos)
		fg.expr(s.promise)
		f.LocalSet(p)
		fg.nullCheck(p, "await", s.pos)
		f.LocalGet(ag.frame).LocalGet(p).I32Store(slotOff(frameAwaiting))
		f.LocalGet(p).I32Load(slotOff(promiseState)).Op(wasm.OpI32Eqz)
		f.If(wasm.Void)
		f.LocalGet(ag.frame).I32Const(id).I32Store(slotOff(frameState))
		fg.saved(true)
		f.LocalGet(ag.frame).LocalGet(p).I32Load(slotOff(promiseWaiters)).I32Store(slotOff(frameNext))
		f.LocalGet(p).LocalGet(ag.frame).I32Store(slotOff(promiseWaiters))
		g.leave(f)
		f.Return()
		f.End()
	})
	f.I32Const(0).LocalSet(ag.rs)
	f.LocalGet(ag.frame).I32Load(slotOff(frameAwaiting)).LocalSet(p)
	f.LocalGet(ag.frame).I32Const(0).I32Store(slotOff(frameAwaiting))
	saved := fg.awaited
	fg.awaited = p
	defer func() { fg.awaited = saved }()
	if s.ret {
		if s.e != nil {
			f.LocalGet(p).I32Load(slotOff(promiseState)).I32Const(2).Op(wasm.OpI32Eq)
			f.If(wasm.Void)
			fg.settle(&irAwaited{irBase{s.e}, true}, true)
			g.leave(f)
			f.Return()
			f.End()
		}
		var v irExpr
		if s.t.kind != tVoid {
			v = &irAwaited{irBase{s.t}, false}
		}
		fg.settle(v, false)
		g.leave(f)
		f.Return()
	} else {
		if s.val != nil {
			fg.assign(&irAssign{target: s.val, op: '=', value: &irAwaited{irBase{s.t}, false}, pos: s.pos})
		}
		if s.err != nil {
			fg.assign(&irAssign{target: s.err, op: '=', value: &irAwaited{irBase{s.e}, true}, pos: s.pos})
		}
	}
	fg.release(wasm.I32, p)
}

// emitFrameResume writes $frame_resume, which the runtime resumes a frame with: its class's resume function.
func (g *gen) emitFrameResume(f *wasm.Func) {
	sig := wasm.FuncType{Params: []wasm.ValType{wasm.I32}}
	sel := g.selector("resume", "", sig)
	f.LocalGet(0)
	f.LocalGet(0).I32Load(0).I32Const(int32(tagClass - sel*len(g.classes))).Op(wasm.OpI32Sub)
	f.CallIndirect(g.selList[sel].typ)
}
