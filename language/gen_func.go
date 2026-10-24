package language

import (
	"math"

	"adventurers-life-online/server/internal/engine/wasm"
)

// fgen writes the code of one function, method, constructor, or of the module constants.
type fgen struct {
	g      *gen
	f      *wasm.Func
	fn     *funcInfo // nil for the constants
	class  *classInfo
	m      *moduleInfo
	slots  []uint32 // the locals of the variables' slots
	loops  []loopLabels
	temps  map[wasm.ValType][]uint32
	result *Type
}

type loopLabels struct{ exit, next *wasm.Label }

func (g *gen) newFgen(f *wasm.Func, fn *funcInfo, class *classInfo, m *moduleInfo) *fgen {
	return &fgen{g: g, f: f, fn: fn, class: class, m: m, temps: map[wasm.ValType][]uint32{}}
}

// emitBody writes a function's code: the call's count, its statements, and a zero value to return when its end
// is reached without a return (only a function returning nothing gets there).
func (g *gen) emitBody(f *wasm.Func, fn *funcInfo, class *classInfo, body *irBlock) {
	fg := g.newFgen(f, fn, class, fn.mod)
	fg.result = fn.result
	first := uint32(0)
	if class != nil {
		first = 1
	}
	for i, t := range fn.locals {
		if i < len(fn.params) {
			fg.slots = append(fg.slots, first+uint32(i))
		} else {
			fg.slots = append(fg.slots, f.Local(wt(t)))
		}
	}
	g.prologue(f, g.pos(fn.mod, fn.pos))
	fg.block(body)
	g.leave(f)
	if fn.result != nil && fn.result.kind != tVoid {
		fg.zero(fn.result)
	}
}

func (fg *fgen) pos(pos int) int32 { return fg.g.pos(fg.m, pos) }

func (fg *fgen) call(name string) { fg.f.Call(fg.g.runtime(name)) }

func (fg *fgen) str(s string) { fg.f.I32Const(int32(fg.g.str(s))) }

// tmp borrows a local of type t until release gives it back.
func (fg *fgen) tmp(t wasm.ValType) uint32 {
	if free := fg.temps[t]; len(free) > 0 {
		l := free[len(free)-1]
		fg.temps[t] = free[:len(free)-1]
		return l
	}
	return fg.f.Local(t)
}

func (fg *fgen) release(t wasm.ValType, l uint32) { fg.temps[t] = append(fg.temps[t], l) }

func (fg *fgen) zero(t *Type) {
	switch t.kind {
	case tNumber:
		fg.f.F64Const(0)
	case tString:
		fg.str("")
	default:
		fg.f.I32Const(0)
	}
}

// step counts a statement or a loop's turn against the budget.
func (fg *fgen) step(pos int) {
	f, steps := fg.f, fg.g.globals["steps"]
	f.GlobalGet(steps).I32Const(1).Op(wasm.OpI32Sub).GlobalSet(steps)
	f.GlobalGet(steps).I32Const(0).Op(wasm.OpI32LtS)
	f.If(wasm.Void)
	f.I32Const(fg.pos(pos))
	fg.call("fail_steps")
	f.End()
}

// setPos tells the natives and the dispatch where the call that may reach them is.
func (fg *fgen) setPos(pos int) {
	fg.f.I32Const(fg.pos(pos)).GlobalSet(fg.g.globals["npos"])
}

// ---- statements ----

func (fg *fgen) block(b *irBlock) {
	for _, s := range b.stmts {
		fg.stmt(s)
	}
}

func (fg *fgen) stmt(s irStmt) {
	f := fg.f
	switch s := s.(type) {
	case *irBlock:
		fg.block(s)
	case *irLet:
		fg.step(s.pos)
		if s.init != nil {
			fg.expr(s.init)
		} else {
			fg.zero(s.t)
		}
		f.LocalSet(fg.slots[s.slot])
	case *irAssign:
		fg.step(s.pos)
		fg.assign(s)
	case *irExprStmt:
		fg.step(s.pos)
		fg.expr(s.x)
		if t := s.x.typeOf(); t.kind != tVoid {
			f.Drop()
		}
	case *irIf:
		fg.step(s.pos)
		fg.expr(s.cond)
		f.If(wasm.Void)
		fg.stmt(s.then)
		if s.els != nil {
			f.Else()
			fg.stmt(s.els)
		}
		f.End()
	case *irLoop:
		if s.init != nil {
			fg.stmt(s.init)
		}
		exit := f.Block(wasm.Void)
		top := f.Loop(wasm.Void)
		fg.step(s.pos)
		if s.cond != nil {
			fg.expr(s.cond)
			f.Op(wasm.OpI32Eqz).BrIf(exit)
		}
		next := f.Block(wasm.Void)
		fg.loops = append(fg.loops, loopLabels{exit: exit, next: next})
		fg.stmt(s.body)
		fg.loops = fg.loops[:len(fg.loops)-1]
		f.End()
		if s.step != nil {
			fg.stmt(s.step)
		}
		f.Br(top)
		f.End()
		f.End()
	case *irForOf:
		fg.step(s.pos)
		a, n, st, i := fg.tmp(wasm.I32), fg.tmp(wasm.I32), fg.tmp(wasm.I32), fg.tmp(wasm.I32)
		fg.expr(s.iter)
		f.LocalTee(a)
		f.If(wasm.Void)
		f.LocalGet(a).I32Load(12).LocalSet(n)
		f.LocalGet(a).I32Load(16).LocalSet(st)
		f.I32Const(0).LocalSet(i)
		exit := f.Block(wasm.Void)
		top := f.Loop(wasm.Void)
		f.LocalGet(i).LocalGet(n).Op(wasm.OpI32GeU).BrIf(exit)
		fg.step(s.pos)
		f.LocalGet(st).LocalGet(i).I32Const(3).Op(wasm.OpI32Shl).Op(wasm.OpI32Add)
		load(f, s.elem, headerSize)
		f.LocalSet(fg.slots[s.slot])
		next := f.Block(wasm.Void)
		fg.loops = append(fg.loops, loopLabels{exit: exit, next: next})
		fg.stmt(s.body)
		fg.loops = fg.loops[:len(fg.loops)-1]
		f.End()
		f.LocalGet(i).I32Const(1).Op(wasm.OpI32Add).LocalSet(i)
		f.Br(top)
		f.End()
		f.End()
		f.End()
		fg.release(wasm.I32, a)
		fg.release(wasm.I32, n)
		fg.release(wasm.I32, st)
		fg.release(wasm.I32, i)
	case *irBreak:
		f.Br(fg.loops[len(fg.loops)-1].exit)
	case *irContinue:
		f.Br(fg.loops[len(fg.loops)-1].next)
	case *irReturn:
		fg.step(s.pos)
		if s.value != nil {
			fg.expr(s.value)
		}
		fg.g.leave(f)
		f.Return()
	case *irSuperCall:
		f.LocalGet(0)
		for _, a := range s.args {
			fg.expr(a)
		}
		f.Call(fg.g.funcs[s.ctor].Index)
	case *irFieldInits:
		for _, fi := range s.class.fields {
			if fi.owner != s.class || fi.init == nil {
				continue
			}
			f.LocalGet(0)
			fg.expr(fi.init)
			store(f, fi.typ, uint32(headerSize+8*fi.slot))
		}
	case *irNop:
	}
}

// nullCheck fails with "<what> of null" when the reference in l is null.
func (fg *fgen) nullCheck(l uint32, what string, pos int) {
	f := fg.f
	f.LocalGet(l).Op(wasm.OpI32Eqz)
	f.If(wasm.Void)
	fg.str(what)
	f.I32Const(fg.pos(pos))
	fg.call("fail_null")
	f.End()
}

func (fg *fgen) assign(s *irAssign) {
	f, t := fg.f, s.target
	switch t.kind {
	case targetLocal:
		l := fg.slots[t.slot]
		fg.update(s, t.t, func() { f.LocalGet(l) })
		f.LocalSet(l)
	case targetField:
		o := fg.tmp(wasm.I32)
		fg.expr(t.x)
		f.LocalSet(o)
		fg.nullCheck(o, t.name, t.pos)
		if t.f.getter != nil {
			fg.str(t.name)
			f.I32Const(fg.pos(t.pos))
			fg.call("fail_hostfield")
			fg.release(wasm.I32, o)
			return
		}
		off := uint32(headerSize + 8*t.f.slot)
		f.LocalGet(o)
		fg.update(s, t.t, func() { f.LocalGet(o); load(f, t.t, off) })
		store(f, t.t, off)
		fg.release(wasm.I32, o)
	case targetIfaceField:
		o := fg.tmp(wasm.I32)
		fg.expr(t.x)
		f.LocalSet(o)
		fg.nullCheck(o, t.name, t.pos)
		f.LocalGet(o)
		fg.update(s, t.t, func() {
			f.LocalGet(o)
			fg.dispatch(o, "get", t.name, wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wt(t.t)}}, t.pos)
		})
		fg.dispatch(o, "set", t.name, wasm.FuncType{Params: []wasm.ValType{wasm.I32, wt(t.t)}}, t.pos)
		fg.release(wasm.I32, o)
	case targetIndex:
		a, idx, addr := fg.tmp(wasm.I32), fg.tmp(wasm.F64), fg.tmp(wasm.I32)
		v := fg.tmp(wt(t.t))
		fg.expr(t.x)
		f.LocalSet(a)
		fg.expr(t.i)
		f.LocalSet(idx)
		f.LocalGet(a).LocalGet(idx).I32Const(fg.pos(t.pos))
		fg.call("arr_elem")
		f.LocalSet(addr)
		fg.update(s, t.t, func() { f.LocalGet(addr); load(f, t.t, 0) })
		f.LocalSet(v)
		// the value may have grown or shrunk the array: set the element of the array as it is now
		f.LocalGet(a).LocalGet(idx).Raw(0xab) // i32.trunc_f64_u
		f.I32Const(fg.pos(t.pos))
		fg.call("arr_elem_set")
		f.LocalGet(v)
		store(f, t.t, 0)
		fg.release(wasm.I32, a)
		fg.release(wasm.F64, idx)
		fg.release(wasm.I32, addr)
		fg.release(wt(t.t), v)
	}
}

// update pushes the target's new value: the value assigned, or the operator applied to the current value (cur
// pushes it) and the value.
func (fg *fgen) update(s *irAssign, t *Type, cur func()) {
	f := fg.f
	switch s.op {
	case '=':
		fg.expr(s.value)
	case '+', '-', '*', '/', '%':
		cur()
		fg.expr(s.value)
		if t.kind == tString {
			fg.toString(s.value.typeOf(), s.pos)
			f.I32Const(fg.pos(s.pos))
			fg.call("str_concat")
			return
		}
		fg.arith(s.op, s.pos)
	default: // ++ and --
		cur()
		f.F64Const(1)
		if s.op == 'i' {
			fg.arith('+', s.pos)
		} else {
			fg.arith('-', s.pos)
		}
	}
}

// ---- expressions ----

// expr pushes an expression's value (nothing for a call that returns nothing).
func (fg *fgen) expr(e irExpr) {
	f := fg.f
	switch e := e.(type) {
	case *irNum:
		f.F64Const(e.v)
	case *irStr:
		fg.str(e.s)
	case *irBool:
		if e.v {
			f.I32Const(1)
		} else {
			f.I32Const(0)
		}
	case *irNull:
		f.I32Const(0)
	case *irLocal:
		f.LocalGet(fg.slots[e.slot])
	case *irThis:
		f.LocalGet(0)
	case *irConst:
		f.GlobalGet(fg.g.constGlbs[e.k])
	case *irNot:
		fg.expr(e.x)
		f.Op(wasm.OpI32Eqz)
	case *irNeg:
		fg.expr(e.x)
		f.Op(wasm.OpF64Neg)
	case *irArith:
		fg.expr(e.l)
		fg.expr(e.r)
		fg.arith(e.op, e.pos)
	case *irConcat:
		fg.expr(e.l)
		fg.toString(e.l.typeOf(), e.pos)
		fg.expr(e.r)
		fg.toString(e.r.typeOf(), e.pos)
		f.I32Const(fg.pos(e.pos))
		fg.call("str_concat")
	case *irCompare:
		fg.expr(e.l)
		fg.expr(e.r)
		if e.l.typeOf().kind == tString {
			fg.call("str_cmp")
			f.I32Const(0)
			f.Op(map[string]byte{"<": wasm.OpI32LtS, "<=": wasm.OpI32LeS, ">": wasm.OpI32GtS, ">=": wasm.OpI32GeS}[e.op])
		} else {
			f.Op(map[string]byte{"<": wasm.OpF64Lt, "<=": wasm.OpF64Le, ">": wasm.OpF64Gt, ">=": wasm.OpF64Ge}[e.op])
		}
	case *irEqual:
		fg.expr(e.l)
		fg.expr(e.r)
		kind := e.l.typeOf().kind
		if kind == tNull {
			kind = e.r.typeOf().kind
		}
		switch kind {
		case tNumber:
			f.Op(wasm.OpF64Eq)
		case tString:
			fg.call("str_eq")
		default:
			f.Op(wasm.OpI32Eq)
		}
		if e.neq {
			f.Op(wasm.OpI32Eqz)
		}
	case *irLogic:
		fg.expr(e.l)
		f.If(wasm.Result(wasm.I32))
		if e.and {
			fg.expr(e.r)
			f.Else()
			f.I32Const(0)
		} else {
			f.I32Const(1)
			f.Else()
			fg.expr(e.r)
		}
		f.End()
	case *irCond:
		fg.expr(e.c)
		f.If(wasm.Result(wt(e.t)))
		fg.expr(e.a)
		f.Else()
		fg.expr(e.b)
		f.End()
	case *irCall:
		for _, a := range e.args {
			fg.expr(a)
		}
		if e.fn.native != nil {
			fg.setPos(e.pos)
		}
		f.Call(fg.g.funcFor(e.fn).Index)
	case *irStd:
		fg.std(e)
	case *irLength:
		fg.expr(e.x)
		switch e.x.typeOf().kind {
		case tString:
			fg.call("str_runes")
		case tMap:
			fg.call("map_len")
		default:
			fg.call("arr_len")
		}
		f.Op(wasm.OpF64ConvertI32U)
	case *irField:
		o := fg.tmp(wasm.I32)
		fg.expr(e.x)
		f.LocalSet(o)
		fg.nullCheck(o, e.f.name, e.pos)
		f.LocalGet(o)
		if e.f.getter != nil {
			fg.setPos(e.pos)
			f.Call(fg.g.getterFunc(e.f).Index)
		} else {
			load(f, e.f.typ, uint32(headerSize+8*e.f.slot))
		}
		fg.release(wasm.I32, o)
	case *irIfaceField:
		o := fg.tmp(wasm.I32)
		fg.expr(e.x)
		f.LocalSet(o)
		fg.nullCheck(o, e.name, e.pos)
		f.LocalGet(o)
		fg.dispatch(o, "get", e.name, wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wt(e.t)}}, e.pos)
		fg.release(wasm.I32, o)
	case *irIndex:
		fg.expr(e.x)
		fg.expr(e.i)
		f.I32Const(fg.pos(e.pos))
		fg.call("arr_elem")
		load(f, e.t, 0)
	case *irMethod:
		fg.method(e)
	case *irSuper:
		f.LocalGet(0)
		for _, a := range e.args {
			fg.expr(a)
		}
		f.Call(fg.g.funcFor(e.fn).Index)
	case *irNew:
		var temps []uint32
		for _, a := range e.args {
			l := fg.tmp(wt(a.typeOf()))
			fg.expr(a)
			f.LocalSet(l)
			temps = append(temps, l)
		}
		o := fg.tmp(wasm.I32)
		f.Call(fg.g.allocFunc(e.class).Index).LocalTee(o)
		for _, l := range temps {
			f.LocalGet(l)
		}
		f.Call(fg.g.funcs[e.class.ctor].Index)
		f.LocalGet(o)
		for i, l := range temps {
			fg.release(wt(e.args[i].typeOf()), l)
		}
		fg.release(wasm.I32, o)
	case *irNewMap:
		flags := int32(0)
		if e.t.key.kind == tString {
			flags |= mapStrKeys
		}
		if e.t.elem.isRef() || e.t.elem.kind == tString {
			flags |= mapRefVals
		}
		f.I32Const(flags).I32Const(fg.pos(e.pos))
		fg.call("map_new")
	case *irArray:
		a := fg.tmp(wasm.I32)
		f.I32Const(arrayTag(e.t)).I32Const(int32(len(e.items))).I32Const(fg.pos(e.pos))
		fg.call("arr_new")
		f.LocalSet(a)
		for i, item := range e.items {
			f.LocalGet(a).I32Load(16)
			fg.expr(item)
			store(f, e.t.elem, uint32(headerSize+8*i))
		}
		f.LocalGet(a)
		fg.release(wasm.I32, a)
	case *irBuiltin:
		fg.builtin(e)
	default:
		f.Unreachable()
	}
}

func arrayTag(t *Type) int32 {
	if t.elem.isRef() || t.elem.kind == tString {
		return tagArrRefs
	}
	return tagArrVals
}

// arith applies a numeric operator to the two numbers on the stack, failing on a result that isn't finite.
func (fg *fgen) arith(op byte, pos int) {
	f := fg.f
	switch op {
	case '+':
		f.Op(wasm.OpF64Add)
	case '-':
		f.Op(wasm.OpF64Sub)
	case '*':
		f.Op(wasm.OpF64Mul)
	case '/':
		r := fg.tmp(wasm.F64)
		f.LocalTee(r).F64Const(0).Op(wasm.OpF64Eq)
		f.If(wasm.Void)
		f.I32Const(fg.pos(pos))
		fg.call("fail_divzero")
		f.End()
		f.LocalGet(r).Op(wasm.OpF64Div)
		fg.release(wasm.F64, r)
	case '%':
		f.I32Const(fg.pos(pos))
		fg.call("mod")
		return
	}
	v := fg.tmp(wasm.F64)
	f.LocalTee(v).Op(wasm.OpF64Abs).F64Const(math.Inf(1)).Op(wasm.OpF64Lt).Op(wasm.OpI32Eqz)
	f.If(wasm.Void)
	fg.str(string(op))
	f.I32Const(fg.pos(pos))
	fg.call("fail_large")
	f.End()
	f.LocalGet(v)
	fg.release(wasm.F64, v)
}

// toString turns the value of type t on the stack into its text form.
func (fg *fgen) toString(t *Type, pos int) {
	f := fg.f
	switch t.kind {
	case tNumber:
		f.I32Const(fg.pos(pos))
		fg.call("num_str")
	case tBool:
		b := fg.tmp(wasm.I32)
		f.LocalSet(b)
		fg.str("true")
		fg.str("false")
		f.LocalGet(b).Select()
		fg.release(wasm.I32, b)
	}
}

// dispatch calls what the object in o's class answers for a selector, with the arguments on the stack.
func (fg *fgen) dispatch(o uint32, kind, name string, sig wasm.FuncType, pos int) {
	f, g := fg.f, fg.g
	sel := g.selector(kind, name, sig)
	fg.setPos(pos)
	f.LocalGet(o).I32Load(0).I32Const(int32(tagClass - sel*len(g.classes))).Op(wasm.OpI32Sub)
	f.CallIndirect(g.selList[sel].typ)
}

func (fg *fgen) method(e *irMethod) {
	f, g := fg.f, fg.g
	o := fg.tmp(wasm.I32)
	fg.expr(e.recv)
	f.LocalSet(o)
	fg.nullCheck(o, e.name+"()", e.pos)
	f.LocalGet(o)
	for _, a := range e.args {
		fg.expr(a)
	}
	switch {
	case e.fn.native != nil:
		fg.setPos(e.pos)
		f.Call(g.nativeFunc(e.fn).Index)
	case !e.iface && (e.fn.private || g.final(e.recv.typeOf().class, e.fn)):
		f.Call(g.funcFor(e.fn).Index)
	default:
		fg.dispatch(o, "m", e.name, sigOf(e.fn, true), e.pos)
	}
	fg.release(wasm.I32, o)
}
