package language

// stmts checks statements in the current scope. It returns their code, and whether they always return.
func (fc *funcCtx) stmts(list []stmt) (execFn, bool) {
	code := make([]execFn, 0, len(list))
	returns := false
	for _, s := range list {
		fn, r := fc.stmt(s)
		code = append(code, fn)
		returns = returns || r
	}
	return func(fr *frame) (flow, error) {
		for _, fn := range code {
			if f, err := fn(fr); err != nil || f != flowNext {
				return f, err
			}
		}
		return flowNext, nil
	}, returns
}

func (fc *funcCtx) block(b *blockStmt) (execFn, bool) {
	fc.push()
	defer fc.pop()
	return fc.stmts(b.stmts)
}

// stepped charges a statement against the step budget before running it.
func (fc *funcCtx) stepped(pos int, fn execFn) execFn {
	m := fc.m
	return func(fr *frame) (flow, error) {
		if err := fr.vm.step(m, pos); err != nil {
			return flowNext, err
		}
		return fn(fr)
	}
}

func (fc *funcCtx) cond(n node) evalFn {
	fn, t := fc.expr(n, nil)
	if t.kind != tBool && t.kind != tInvalid {
		fc.errorf(n.position(), "a condition is a bool, not a %s", t)
	}
	return fn
}

func (fc *funcCtx) stmt(s stmt) (execFn, bool) {
	m := fc.m
	switch s := s.(type) {
	case *blockStmt:
		return fc.block(s)
	case *letStmt:
		return fc.stepped(s.pos, fc.let(s)), false
	case *assignStmt:
		return fc.stepped(s.pos, fc.assign(s)), false
	case *exprStmt:
		switch s.x.(type) {
		case *callNode, *methodCallNode, *superMethodNode, *newNode:
		case *superCallNode:
			fc.errorf(s.pos, "super(…) is only the first statement of a constructor")
		default:
			fc.errorf(s.pos, "this does nothing: assign its value, or call something")
		}
		x, _ := fc.expr(s.x, nil)
		return fc.stepped(s.pos, func(fr *frame) (flow, error) {
			_, err := x(fr)
			return flowNext, err
		}), false
	case *ifStmt:
		cond := fc.cond(s.cond)
		then, thenReturns := fc.block(s.then)
		var els execFn
		elsReturns := false
		if s.els != nil {
			els, elsReturns = fc.stmt(s.els)
		}
		return fc.stepped(s.pos, func(fr *frame) (flow, error) {
			c, err := cond(fr)
			if err != nil {
				return flowNext, err
			}
			if c.b {
				return then(fr)
			}
			if els != nil {
				return els(fr)
			}
			return flowNext, nil
		}), thenReturns && elsReturns
	case *whileStmt:
		cond := fc.cond(s.cond)
		fc.loops++
		body, _ := fc.block(s.body)
		fc.loops--
		return fc.loop(s.pos, nil, cond, nil, body), false
	case *forStmt:
		fc.push()
		defer fc.pop()
		var init, step execFn
		if s.init != nil {
			init, _ = fc.stmt(s.init)
		}
		var cond evalFn
		if s.cond != nil {
			cond = fc.cond(s.cond)
		}
		if s.step != nil {
			step, _ = fc.stmt(s.step)
		}
		fc.loops++
		body, _ := fc.block(s.body)
		fc.loops--
		return fc.loop(s.pos, init, cond, step, body), false
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
		pos := s.pos
		return func(fr *frame) (flow, error) {
			if err := fr.vm.step(m, pos); err != nil {
				return flowNext, err
			}
			v, err := iter(fr)
			if err != nil {
				return flowNext, err
			}
			if v.kind != Array {
				return flowNext, nil // a null array is empty
			}
			for _, item := range v.ref.(*arrayVal).items {
				if err := fr.vm.step(m, pos); err != nil {
					return flowNext, err
				}
				fr.locals[slot] = item
				f, err := body(fr)
				if err != nil || f == flowReturn {
					return f, err
				}
				if f == flowBreak {
					break
				}
			}
			return flowNext, nil
		}, false
	case *breakStmt, *continueStmt:
		f := flowBreak
		if _, ok := s.(*continueStmt); ok {
			f = flowContinue
		}
		if fc.loops == 0 {
			fc.errorf(s.stmtPos(), "break and continue are only in loops")
		}
		return func(*frame) (flow, error) { return f, nil }, false
	case *returnStmt:
		return fc.stepped(s.pos, fc.ret(s)), true
	}
	fc.errorf(s.stmtPos(), "unexpected statement")
	return func(*frame) (flow, error) { return flowNext, nil }, false
}

// loop runs init, then body while cond holds (always without one), with step after each turn.
func (fc *funcCtx) loop(pos int, init execFn, cond evalFn, step, body execFn) execFn {
	m := fc.m
	return func(fr *frame) (flow, error) {
		if init != nil {
			if _, err := init(fr); err != nil {
				return flowNext, err
			}
		}
		for {
			if err := fr.vm.step(m, pos); err != nil {
				return flowNext, err
			}
			if cond != nil {
				c, err := cond(fr)
				if err != nil {
					return flowNext, err
				}
				if !c.b {
					return flowNext, nil
				}
			}
			f, err := body(fr)
			if err != nil || f == flowReturn {
				return f, err
			}
			if f == flowBreak {
				return flowNext, nil
			}
			if step != nil {
				if _, err := step(fr); err != nil {
					return flowNext, err
				}
			}
		}
	}
}

func (fc *funcCtx) let(s *letStmt) execFn {
	var want *Type
	if s.typ != nil {
		want = fc.c.resolveType(fc.m, s.typ)
	}
	var init evalFn
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
	if init == nil {
		zero := zeroOf(t)
		return func(fr *frame) (flow, error) {
			fr.locals[slot] = zero
			return flowNext, nil
		}
	}
	return func(fr *frame) (flow, error) {
		v, err := init(fr)
		if err != nil {
			return flowNext, err
		}
		fr.locals[slot] = v
		return flowNext, nil
	}
}

func (fc *funcCtx) ret(s *returnStmt) execFn {
	if fc.fn == nil {
		fc.errorf(s.pos, "return is only in functions")
		return func(*frame) (flow, error) { return flowReturn, nil }
	}
	result := fc.fn.result
	if s.value == nil {
		if result.kind != tVoid {
			fc.errorf(s.pos, "%s returns a %s", fc.fn.name, result)
		}
		return func(*frame) (flow, error) { return flowReturn, nil }
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
	return func(fr *frame) (flow, error) {
		out, err := v(fr)
		if err != nil {
			return flowNext, err
		}
		fr.ret = out
		return flowReturn, nil
	}
}

// update computes a target's new value from its current one.
type update func(fr *frame, cur Value) (Value, error)

func (fc *funcCtx) assign(s *assignStmt) execFn {
	t, apply := fc.target(s.target)
	if apply == nil {
		if s.value != nil {
			fc.expr(s.value, nil)
		}
		return func(*frame) (flow, error) { return flowNext, nil }
	}
	m, pos := fc.m, s.pos
	var up update
	switch s.op {
	case "=":
		v, vt := fc.expr(s.value, t)
		if !assignable(vt, t) {
			fc.errorf(s.value.position(), "that is a %s, not a %s", vt, t)
		}
		up = func(fr *frame, _ Value) (Value, error) { return v(fr) }
	case "++", "--":
		if t.kind != tNumber && t.kind != tInvalid {
			fc.errorf(pos, "%s wants a number, not a %s", s.op, t)
		}
		op := s.op[:1]
		up = func(_ *frame, cur Value) (Value, error) {
			f, msg := arith(op, cur.num, 1)
			if msg != "" {
				return Value{}, m.errAt(pos, "%s", msg)
			}
			return Num(f), nil
		}
	default:
		v, vt := fc.expr(s.value, nil)
		op, rt := fc.binaryOp(s.op[:1], t, vt, pos)
		if op == nil {
			break
		}
		if !identical(rt, t) {
			fc.errorf(pos, "%s gives a %s, which doesn't go in a %s", s.op, rt, t)
		}
		up = func(fr *frame, cur Value) (Value, error) {
			r, err := v(fr)
			if err != nil {
				return Value{}, err
			}
			return op(fr, cur, r)
		}
	}
	if up == nil {
		return func(*frame) (flow, error) { return flowNext, nil }
	}
	return func(fr *frame) (flow, error) { return flowNext, apply(fr, up) }
}

// target checks the left side of an assignment: a variable, a field or an array element. apply runs the
// update on it, evaluating the object or array and the index once.
func (fc *funcCtx) target(n node) (*Type, func(fr *frame, up update) error) {
	m := fc.m
	switch n := n.(type) {
	case *nameNode:
		l := fc.lookup(n.name)
		if l == nil {
			if sym := fc.m.symbols[n.name]; sym != nil && sym.kind == symConst {
				fc.errorf(n.pos, "%s is a constant", n.name)
			} else {
				fc.errorf(n.pos, "unknown variable %s", n.name)
			}
			return typInvalid, nil
		}
		if l.constant {
			fc.errorf(n.pos, "%s is a constant", n.name)
		}
		slot := l.slot
		return l.typ, func(fr *frame, up update) error {
			v, err := up(fr, fr.locals[slot])
			if err != nil {
				return err
			}
			fr.locals[slot] = v
			return nil
		}
	case *memberNode:
		x, xt := fc.expr(n.x, nil)
		var f *fieldInfo
		switch xt.kind {
		case tInvalid:
			return typInvalid, nil
		case tClass:
			f = xt.class.fieldByName[n.name]
		case tIface:
			f = xt.iface.fields[n.name]
		case tArray, tString, tMap:
			fc.errorf(n.pos, "%s can't be set", n.name)
			return typInvalid, nil
		}
		if f == nil {
			fc.errorf(n.pos, "%s has no field %s", xt, n.name)
			return typInvalid, nil
		}
		if xt.kind == tClass && !fc.canSee(f.owner, f.private) {
			fc.errorf(n.pos, "%s is private to %s", n.name, f.owner.name)
		}
		if f.readonly {
			_, onThis := n.x.(*thisNode)
			if !(onThis && fc.ctor && xt.kind == tClass && f.owner == fc.class) {
				fc.errorf(n.pos, "%s is readonly: only its class's constructor sets it", n.name)
			}
		}
		name, pos, static := n.name, n.pos, f
		byName := xt.kind == tIface
		return f.typ, func(fr *frame, up update) error {
			o, err := receiver(fr, x, m, pos, name)
			if err != nil {
				return err
			}
			f := static
			if byName {
				f = o.class.fieldByName[name]
			}
			if f.getter != nil {
				return m.errAt(pos, "%s is the host's, and can't be set", name)
			}
			v, err := up(fr, o.fields[f.slot])
			if err != nil {
				return err
			}
			o.fields[f.slot] = v
			return nil
		}
	case *indexNode:
		x, xt := fc.expr(n.x, nil)
		i, it := fc.expr(n.i, nil)
		switch xt.kind {
		case tInvalid:
			return typInvalid, nil
		case tArray:
		case tMap:
			fc.errorf(n.pos, "set a map's entries with set(key, value), not [ ]")
			return typInvalid, nil
		default:
			fc.errorf(n.pos, "only arrays have elements, not a %s", xt)
			return typInvalid, nil
		}
		if it.kind != tNumber && it.kind != tInvalid {
			fc.errorf(n.i.position(), "an index is a number, not a %s", it)
		}
		pos := n.pos
		return xt.elem, func(fr *frame, up update) error {
			a, err := x(fr)
			if err != nil {
				return err
			}
			items, idx, err := index(fr, constant(a), i, m, pos)
			if err != nil {
				return err
			}
			v, err := up(fr, items[idx])
			if err != nil {
				return err
			}
			// The value may have grown or shrunk the array: set the element of the array as it is now
			items = a.ref.(*arrayVal).items
			if idx >= len(items) {
				return m.errAt(pos, "index %d is out of range (length %d)", idx, len(items))
			}
			items[idx] = v
			return nil
		}
	}
	fc.errorf(n.position(), "only variables, fields and array elements can be set")
	return typInvalid, nil
}
