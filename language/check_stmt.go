package language

// stmts checks statements in the current scope. It returns their code, and whether they always return.
func (fc *funcCtx) stmts(list []stmt) (*irBlock, bool) {
	block := &irBlock{stmts: make([]irStmt, 0, len(list))}
	returns := false
	for _, s := range list {
		code, r := fc.stmt(s)
		block.stmts = append(block.stmts, code)
		returns = returns || r
	}
	return block, returns
}

func (fc *funcCtx) block(b *blockStmt) (*irBlock, bool) {
	fc.push()
	defer fc.pop()
	return fc.stmts(b.stmts)
}

func (fc *funcCtx) cond(n node) irExpr {
	e, t := fc.expr(n, nil)
	if t.kind != tBool && t.kind != tInvalid {
		fc.errorf(n.position(), "a condition is a bool, not a %s", t)
	}
	return e
}

// stmt checks a statement. Every statement but blocks, loops, break and continue counts one step against the
// budget where it starts; loops count one for every turn.
func (fc *funcCtx) stmt(s stmt) (irStmt, bool) {
	switch s := s.(type) {
	case *blockStmt:
		return fc.block(s)
	case *letStmt:
		return fc.let(s), false
	case *assignStmt:
		return fc.assign(s), false
	case *exprStmt:
		if a, ok := s.x.(*awaitNode); ok {
			return fc.await(a, false), false
		}
		switch s.x.(type) {
		case *callNode, *methodCallNode, *superMethodNode, *newNode:
		case *superCallNode:
			fc.errorf(s.pos, "super(…) is only the first statement of a constructor")
		default:
			fc.errorf(s.pos, "this does nothing: assign its value, or call something")
		}
		x, _ := fc.expr(s.x, nil)
		return &irExprStmt{x: x, pos: s.pos}, false
	case *ifStmt:
		cond := fc.cond(s.cond)
		then, thenReturns := fc.block(s.then)
		var els irStmt
		elsReturns := false
		if s.els != nil {
			els, elsReturns = fc.stmt(s.els)
		}
		return &irIf{cond: cond, then: then, els: els, pos: s.pos}, thenReturns && elsReturns
	case *whileStmt:
		cond := fc.cond(s.cond)
		fc.loops++
		body, _ := fc.block(s.body)
		fc.loops--
		return &irLoop{cond: cond, body: body, pos: s.pos}, false
	case *forStmt:
		fc.push()
		defer fc.pop()
		loop := &irLoop{pos: s.pos}
		fc.header = true
		if s.init != nil {
			loop.init, _ = fc.stmt(s.init)
		}
		if s.cond != nil {
			loop.cond = fc.cond(s.cond)
		}
		if s.step != nil {
			loop.step, _ = fc.stmt(s.step)
		}
		fc.header = false
		fc.loops++
		loop.body, _ = fc.block(s.body)
		fc.loops--
		return loop, false
	case *forOfStmt:
		iter, it := fc.expr(s.iter, nil)
		elem := typInvalid
		switch it.kind {
		case tArray:
			elem = it.elem
		case tInvalid:
		case tMap:
			fc.errorf(s.iter.position(), "go through a map's keys() or values()")
		default:
			fc.errorf(s.iter.position(), "for … of goes through arrays, not a %s", it)
		}
		fc.push()
		defer fc.pop()
		slot := fc.declare(s.name, elem, s.constant, s.pos).slot
		fc.loops++
		body, _ := fc.block(s.body)
		fc.loops--
		return &irForOf{slot: slot, elem: elem, iter: iter, body: body, pos: s.pos}, false
	case *breakStmt, *continueStmt:
		if fc.loops == 0 {
			fc.errorf(s.stmtPos(), "break and continue are only in loops")
		}
		if _, ok := s.(*continueStmt); ok {
			return &irContinue{}, false
		}
		return &irBreak{}, false
	case *returnStmt:
		return fc.ret(s), true
	}
	fc.errorf(s.stmtPos(), "unexpected statement")
	return &irNop{}, false
}

func (fc *funcCtx) let(s *letStmt) irStmt {
	if s.names != nil {
		return fc.destructure(s)
	}
	var want *Type
	if s.typ != nil {
		want = fc.c.resolveType(fc.m, s.typ)
	}
	if a, ok := s.init.(*awaitNode); ok {
		promise, pt := fc.awaited(a)
		t := typInvalid
		switch {
		case pt.kind != tPromise:
		case pt.key != nil:
			fc.errorf(a.pos, "%s may fail with %s: take both, const [%s, err] = await …", pt, pt.key, s.name)
		case pt.elem.kind == tVoid:
			fc.errorf(a.pos, "that gives nothing to put in %s", s.name)
		default:
			t = pt.elem
		}
		if want != nil {
			if !assignable(t, want) {
				fc.errorf(a.pos, "%s is a %s, not a %s", s.name, t, want)
			}
			t = want
		}
		slot := fc.declare(s.name, t, s.constant, s.pos).slot
		return fc.awaitInto(promise, pt, &irTarget{kind: targetLocal, t: t, slot: slot, pos: s.pos}, nil, a.pos)
	}
	var init irExpr
	t := want
	if s.init != nil {
		var it *Type
		init, it = fc.expr(s.init, want)
		switch {
		case want != nil:
			if !assignable(it, want) {
				fc.errorf(s.init.position(), "%s is a %s, not a %s", s.name, it, want)
			}
		case it.kind == tNull:
			fc.errorf(s.pos, "give %s a type: null alone doesn't say one", s.name)
			it = typInvalid
		case it.kind == tVoid:
			fc.errorf(s.init.position(), "that gives nothing to put in %s", s.name)
			it = typInvalid
		}
		if t == nil {
			t = it
		}
	}
	slot := fc.declare(s.name, t, s.constant, s.pos).slot
	return &irLet{slot: slot, t: t, init: init, pos: s.pos}
}

// destructure checks const [v, err] = await p, which takes both what a promise that may fail gives.
func (fc *funcCtx) destructure(s *letStmt) irStmt {
	a, ok := s.init.(*awaitNode)
	if !ok {
		fc.errorf(s.pos, "only what an await gives is taken apart: const [value, err] = await …")
		fc.expr(s.init, nil)
		return &irNop{}
	}
	promise, pt := fc.awaited(a)
	if len(s.names) != 2 {
		fc.errorf(s.pos, "an await gives two things: const [value, err] = await …")
	}
	vt, et := typInvalid, typInvalid
	if pt.kind == tPromise {
		if pt.key == nil {
			fc.errorf(a.pos, "%s can't fail: take its value alone, const %s = await …", pt, s.names[0])
		} else {
			vt, et = pt.elem, pt.key
		}
	}
	var targets [2]*irTarget
	for i, name := range s.names {
		if i > 1 || name == "_" {
			continue
		}
		t := et
		if i == 0 {
			t = vt
			if t.kind == tVoid {
				fc.errorf(s.namePos[i], "that promise gives no value: write [_, %s]", s.names[len(s.names)-1])
				t = typInvalid
			}
		}
		slot := fc.declare(name, t, s.constant, s.namePos[i]).slot
		targets[i] = &irTarget{kind: targetLocal, t: t, slot: slot, pos: s.namePos[i]}
	}
	return fc.awaitInto(promise, pt, targets[0], targets[1], a.pos)
}

// awaited checks what an await waits for: a promise, in an async function. Its type is invalid when it isn't one.
func (fc *funcCtx) awaited(a *awaitNode) (irExpr, *Type) {
	switch {
	case fc.fn == nil || !fc.fn.async:
		fc.errorf(a.pos, "await is only in async functions")
	case fc.header:
		fc.errorf(a.pos, "await can't be in a for loop's header")
	}
	x, t := fc.expr(a.x, nil)
	if t.kind != tPromise {
		if t.kind != tInvalid {
			fc.errorf(a.x.position(), "await waits for a promise, not a %s", t)
		}
		return x, typInvalid
	}
	return x, t
}

// awaitInto is the code of an await whose value goes to val and error to err (nil drops them).
func (fc *funcCtx) awaitInto(promise irExpr, pt *Type, val, err *irTarget, pos int) irStmt {
	if pt.kind != tPromise {
		return &irNop{}
	}
	return &irAwait{promise: promise, t: pt.elem, e: pt.key, val: val, err: err, pos: pos}
}

// await checks an await statement, which drops what the promise gives, or with ret, return await.
func (fc *funcCtx) await(a *awaitNode, ret bool) irStmt {
	promise, pt := fc.awaited(a)
	if pt.kind != tPromise {
		return &irNop{}
	}
	out := &irAwait{promise: promise, t: pt.elem, e: pt.key, ret: ret, pos: a.pos}
	if ret && fc.fn != nil && fc.fn.async && fc.fn.result.kind == tPromise {
		own := fc.fn.result
		if !assignable(pt.elem, own.elem) && !(pt.elem.kind == tVoid && own.elem.kind == tVoid) {
			fc.errorf(a.pos, "%s returns a %s, not a %s", fc.fn.name, own.elem, pt.elem)
		}
		if pt.key != nil && (own.key == nil || !assignable(pt.key, own.key)) {
			if own.key == nil {
				fc.errorf(a.pos, "that may fail with %s, and %s's promise can't fail: take the error, const [v, err] = await …", pt.key, fc.fn.name)
			} else {
				fc.errorf(a.pos, "that may fail with %s, and %s fails with %s", pt.key, fc.fn.name, own.key)
			}
		}
	}
	return out
}

func (fc *funcCtx) ret(s *returnStmt) irStmt {
	if fc.fn == nil {
		fc.errorf(s.pos, "return is only in functions")
		return &irReturn{pos: s.pos}
	}
	if a, ok := s.value.(*awaitNode); ok {
		if fc.fn.async {
			return fc.await(a, true)
		}
		fc.awaited(a)
		return &irReturn{pos: s.pos}
	}
	if fc.fn.async && fc.fn.result.kind == tPromise {
		return fc.retAsync(s)
	}
	if fc.fn.async {
		if s.value != nil {
			fc.expr(s.value, nil)
		}
		return &irReturn{pos: s.pos}
	}
	result := fc.fn.result
	if s.value == nil {
		if result.kind != tVoid {
			fc.errorf(s.pos, "%s returns a %s", fc.fn.name, result)
		}
		return &irReturn{pos: s.pos}
	}
	if result.kind == tVoid {
		fc.errorf(s.value.position(), "%s returns nothing; give it a result type to return a value", fc.fn.name)
	}
	want := result
	if want.kind == tVoid {
		want = nil
	}
	v, t := fc.expr(s.value, want)
	if want != nil && !assignable(t, want) {
		fc.errorf(s.value.position(), "%s returns a %s, not a %s", fc.fn.name, result, t)
	}
	if want == nil {
		return &irReturn{pos: s.pos}
	}
	return &irReturn{value: v, pos: s.pos}
}

// retAsync checks a return in an async function: a value fulfils its promise, an error (an object of an error
// class) rejects it.
func (fc *funcCtx) retAsync(s *returnStmt) irStmt {
	own := fc.fn.result
	t, e := own.elem, own.key
	if s.value == nil {
		if t.kind != tVoid {
			fc.errorf(s.pos, "%s returns a %s", fc.fn.name, t)
		}
		return &irReturn{pos: s.pos}
	}
	want := t
	if want.kind == tVoid {
		want = nil
	}
	v, vt := fc.expr(s.value, want)
	if vt.isErrorClass() {
		switch {
		case e == nil:
			fc.errorf(s.value.position(), "%s's promise can't fail: give it an error class to fail with, Promise<%s, %s>", fc.fn.name, t, vt)
		case !assignable(vt, e):
			fc.errorf(s.value.position(), "%s fails with %s, not %s", fc.fn.name, e, vt)
		}
		return &irReturn{value: v, reject: true, pos: s.pos}
	}
	switch {
	case vt.kind == tInvalid:
	case t.kind == tVoid:
		fc.errorf(s.value.position(), "%s returns nothing; give its promise a value type to return a value", fc.fn.name)
		return &irReturn{pos: s.pos}
	case !assignable(vt, t):
		fc.errorf(s.value.position(), "%s returns a %s, not a %s", fc.fn.name, t, vt)
	}
	return &irReturn{value: v, pos: s.pos}
}

func (fc *funcCtx) assign(s *assignStmt) irStmt {
	if a, ok := s.value.(*awaitNode); ok {
		return fc.assignAwait(s, a)
	}
	if arr, ok := s.target.(*arrayNode); ok {
		fc.errorf(arr.pos, "only what an await gives is taken apart: [value, err] = await …")
		fc.expr(s.value, nil)
		return &irNop{}
	}
	target := fc.target(s.target)
	if target == nil {
		if s.value != nil {
			fc.expr(s.value, nil)
		}
		return &irNop{}
	}
	t, pos := target.t, s.pos
	out := &irAssign{target: target, pos: pos}
	switch s.op {
	case "=":
		v, vt := fc.expr(s.value, t)
		if !assignable(vt, t) {
			fc.errorf(s.value.position(), "that is a %s, not a %s", vt, t)
		}
		out.op, out.value = '=', v
	case "++", "--":
		if t.kind != tNumber && t.kind != tInvalid {
			fc.errorf(pos, "%s wants a number, not a %s", s.op, t)
		}
		out.op = 'i'
		if s.op == "--" {
			out.op = 'd'
		}
	default:
		v, vt := fc.expr(s.value, nil)
		op, rt := fc.binaryOp(s.op[:1], &irBad{}, v, t, vt, pos)
		if op == bad {
			return &irNop{}
		}
		if !identical(rt, t) {
			fc.errorf(pos, "%s gives a %s, which doesn't go in a %s", s.op, rt, t)
		}
		out.op, out.value = s.op[0], v
	}
	return out
}

// assignAwait checks x = await p, and [v, err] = await p for a promise that may fail.
func (fc *funcCtx) assignAwait(s *assignStmt, a *awaitNode) irStmt {
	if s.op != "=" {
		fc.errorf(s.pos, "%s takes no await: put what it gives in a variable first", s.op)
		fc.awaited(a)
		return &irNop{}
	}
	promise, pt := fc.awaited(a)
	if arr, ok := s.target.(*arrayNode); ok {
		if len(arr.items) != 2 {
			fc.errorf(arr.pos, "an await gives two things: [value, err] = await …")
		}
		if pt.kind == tPromise && pt.key == nil {
			fc.errorf(a.pos, "%s can't fail: take its value alone, x = await …", pt)
		}
		var targets [2]*irTarget
		for i, item := range arr.items {
			if n, ok := item.(*nameNode); (ok && n.name == "_") || i > 1 {
				continue
			}
			t := fc.target(item)
			if t == nil || pt.kind != tPromise || pt.key == nil {
				continue
			}
			want := pt.elem
			if i == 1 {
				want = pt.key
			}
			if want.kind == tVoid {
				fc.errorf(item.position(), "that promise gives no value: write [_, err]")
				continue
			}
			if !assignable(want, t.t) {
				fc.errorf(item.position(), "that is a %s, not a %s", want, t.t)
			}
			targets[i] = t
		}
		return fc.awaitInto(promise, pt, targets[0], targets[1], a.pos)
	}
	target := fc.target(s.target)
	if target == nil || pt.kind != tPromise {
		return &irNop{}
	}
	switch {
	case pt.key != nil:
		fc.errorf(a.pos, "%s may fail with %s: take both, [x, err] = await …", pt, pt.key)
	case pt.elem.kind == tVoid:
		fc.errorf(a.pos, "that gives nothing to assign")
	case !assignable(pt.elem, target.t):
		fc.errorf(a.pos, "that is a %s, not a %s", pt.elem, target.t)
	}
	return fc.awaitInto(promise, pt, target, nil, a.pos)
}

// target checks the left side of an assignment: a variable, a field or an array element. It is nil when it
// failed (and said why).
func (fc *funcCtx) target(n node) *irTarget {
	switch n := n.(type) {
	case *nameNode:
		if fc.probing(n.name) {
			fc.c.rec.probe.names(fc)
			return nil
		}
		l := fc.lookup(n.name)
		if l == nil {
			if sym := fc.m.symbols[n.name]; sym != nil && sym.kind == symInvalid {
				return nil
			}
			if sym := fc.m.symbols[n.name]; sym != nil && sym.kind == symConst {
				fc.errorf(n.pos, "%s is a constant", n.name)
			} else {
				fc.errorf(n.pos, "unknown variable %s", n.name)
			}
			return nil
		}
		if l.constant {
			fc.errorf(n.pos, "%s is a constant", n.name)
		}
		fc.record(n.pos, n.name, localRef(l, n.name, fc.m, false))
		return &irTarget{kind: targetLocal, t: l.typ, slot: l.slot, pos: n.pos}
	case *memberNode:
		x, xt := fc.expr(n.x, nil)
		if fc.probing(n.name) {
			fc.c.rec.probe.members(fc, xt)
			return nil
		}
		var f *fieldInfo
		switch xt.kind {
		case tInvalid:
			return nil
		case tClass:
			f = xt.class.fieldByName[n.name]
		case tIface:
			f = xt.iface.fields[n.name]
		case tArray, tString, tMap:
			fc.errorf(n.pos, "%s can't be set", n.name)
			return nil
		}
		if f == nil {
			fc.errorf(n.pos, "%s has no field %s", xt, n.name)
			return nil
		}
		if xt.kind == tClass && !fc.canSee(f.owner, f.private) {
			fc.errorf(n.pos, "%s is private to %s", n.name, f.owner.name)
		}
		if xt.kind == tClass {
			fc.record(n.pos, n.name, fieldRef(f.owner.name, f, false))
		} else {
			fc.record(n.pos, n.name, fieldRef(xt.iface.name, f, false))
		}
		if f.readonly {
			_, onThis := n.x.(*thisNode)
			if !(onThis && fc.ctor && xt.kind == tClass && f.owner == fc.class) {
				fc.errorf(n.pos, "%s is readonly: only its class's constructor sets it", n.name)
			}
		}
		if xt.kind == tIface {
			return &irTarget{kind: targetIfaceField, t: f.typ, x: x, name: n.name, pos: n.pos}
		}
		return &irTarget{kind: targetField, t: f.typ, x: x, f: f, name: n.name, pos: n.pos}
	case *indexNode:
		x, xt := fc.expr(n.x, nil)
		i, it := fc.expr(n.i, nil)
		switch xt.kind {
		case tInvalid:
			return nil
		case tArray:
		case tMap:
			fc.errorf(n.pos, "set a map's entries with set(key, value), not [ ]")
			return nil
		default:
			fc.errorf(n.pos, "only arrays have elements, not a %s", xt)
			return nil
		}
		if it.kind != tNumber && it.kind != tInvalid {
			fc.errorf(n.i.position(), "an index is a number, not a %s", it)
		}
		return &irTarget{kind: targetIndex, t: xt.elem, x: x, i: i, pos: n.pos}
	}
	fc.errorf(n.position(), "only variables, fields and array elements can be set")
	return nil
}
