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
		if s.init != nil {
			loop.init, _ = fc.stmt(s.init)
		}
		if s.cond != nil {
			loop.cond = fc.cond(s.cond)
		}
		if s.step != nil {
			loop.step, _ = fc.stmt(s.step)
		}
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
	var want *Type
	if s.typ != nil {
		want = fc.c.resolveType(fc.m, s.typ)
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

func (fc *funcCtx) ret(s *returnStmt) irStmt {
	if fc.fn == nil {
		fc.errorf(s.pos, "return is only in functions")
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

func (fc *funcCtx) assign(s *assignStmt) irStmt {
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
