package language

import (
	"fmt"
	"unicode/utf8"
)

var sprintf = fmt.Sprintf

type local struct {
	slot     int
	typ      *Type
	constant bool
	param    bool
	pos      int // where it is declared
}

// funcCtx checks the code of one function, method, constructor, field value or module constant.
type funcCtx struct {
	c      *checker
	m      *moduleInfo
	class  *classInfo // the class whose code this is, for this and private members
	fn     *funcInfo  // nil for field values and constants
	scopes []map[string]*local
	// maxLocals is how many slots the function's frame needs: every variable gets its own
	maxLocals int
	loops     int
	ctor      bool // in a constructor, where its class's readonly fields may be set
	inInit    bool // in a field's value
}

func (c *checker) newFuncCtx(m *moduleInfo, class *classInfo, fn *funcInfo) *funcCtx {
	return &funcCtx{c: c, m: m, class: class, fn: fn}
}

func (fc *funcCtx) errorf(pos int, format string, args ...any) {
	fc.c.errorf(fc.m, pos, format, args...)
}

func (fc *funcCtx) push() { fc.scopes = append(fc.scopes, map[string]*local{}) }
func (fc *funcCtx) pop()  { fc.scopes = fc.scopes[:len(fc.scopes)-1] }

func (fc *funcCtx) declare(name string, t *Type, constant bool, pos int) *local {
	return fc.declareAs(name, t, constant, false, pos)
}

// declareParam declares a function's parameter.
func (fc *funcCtx) declareParam(name string, t *Type, pos int) {
	fc.declareAs(name, t, false, true, pos)
}

func (fc *funcCtx) declareAs(name string, t *Type, constant, param bool, pos int) *local {
	scope := fc.scopes[len(fc.scopes)-1]
	if _, dup := scope[name]; dup {
		fc.errorf(pos, "%s is already declared here", name)
	}
	l := &local{slot: fc.maxLocals, typ: t, constant: constant, param: param, pos: pos}
	fc.maxLocals++
	scope[name] = l
	fc.record(pos, name, localRef(l, name, fc.m, true))
	return l
}

// record notes a name's occurrence for Analyze.
func (fc *funcCtx) record(pos int, name string, ref Ref) {
	if fc.c.rec != nil {
		fc.c.rec.add(fc.m, pos, name, ref)
	}
}

// probing reports whether name is the completion probe, which Complete puts where the cursor is.
func (fc *funcCtx) probing(name string) bool {
	return fc.c.rec != nil && fc.c.rec.probe != nil && name == probeName
}

func (fc *funcCtx) lookup(name string) *local {
	for i := len(fc.scopes) - 1; i >= 0; i-- {
		if l, ok := fc.scopes[i][name]; ok {
			return l
		}
	}
	return nil
}

// canSee reports whether code here may use a member of owner that may be private.
func (fc *funcCtx) canSee(owner *classInfo, private bool) bool { return !private || fc.class == owner }

func bad(*frame) (Value, error) { return Value{}, nil }

func constant(v Value) evalFn { return func(*frame) (Value, error) { return v, nil } }

// expr checks an expression and returns its code and type. want, when not nil, is the type the place it goes
// expects, which gives empty arrays and null their types.
func (fc *funcCtx) expr(n node, want *Type) (evalFn, *Type) {
	m := fc.m
	switch n := n.(type) {
	case *literalNode:
		switch n.v.kind {
		case Number:
			return constant(n.v), typNumber
		case String:
			return constant(n.v), typString
		}
		return constant(n.v), typBool
	case *nullNode:
		return constant(null), typNull
	case *thisNode:
		if fc.class == nil {
			fc.errorf(n.pos, "this is only in a class's code")
			return bad, typInvalid
		}
		return func(fr *frame) (Value, error) { return fr.this, nil }, fc.class.typ
	case *nameNode:
		return fc.name(n)
	case *unaryNode:
		x, t := fc.expr(n.x, nil)
		if t.kind == tInvalid {
			return bad, t
		}
		if n.op == "!" {
			if t.kind != tBool {
				fc.errorf(n.pos, "! wants a bool, not a %s", t)
				return bad, typInvalid
			}
			return func(fr *frame) (Value, error) {
				v, err := x(fr)
				if err != nil {
					return Value{}, err
				}
				return Boolean(!v.b), nil
			}, typBool
		}
		if t.kind != tNumber {
			fc.errorf(n.pos, "- wants a number, not a %s", t)
			return bad, typInvalid
		}
		return func(fr *frame) (Value, error) {
			v, err := x(fr)
			if err != nil {
				return Value{}, err
			}
			return Num(-v.num), nil
		}, typNumber
	case *binaryNode:
		l, lt := fc.expr(n.l, nil)
		r, rt := fc.expr(n.r, nil)
		op, t := fc.binaryOp(n.op, lt, rt, n.pos)
		if op == nil {
			return bad, t
		}
		return func(fr *frame) (Value, error) {
			a, err := l(fr)
			if err != nil {
				return Value{}, err
			}
			b, err := r(fr)
			if err != nil {
				return Value{}, err
			}
			return op(fr, a, b)
		}, t
	case *logicalNode:
		l, lt := fc.expr(n.l, nil)
		r, rt := fc.expr(n.r, nil)
		if (lt.kind != tBool && lt.kind != tInvalid) || (rt.kind != tBool && rt.kind != tInvalid) {
			fc.errorf(n.pos, "%s wants bools, not a %s and a %s", n.op, lt, rt)
			return bad, typInvalid
		}
		and := n.op == "&&"
		return func(fr *frame) (Value, error) {
			a, err := l(fr)
			if err != nil || a.b != and {
				return a, err
			}
			return r(fr)
		}, typBool
	case *condNode:
		cond, ct := fc.expr(n.c, nil)
		if ct.kind != tBool && ct.kind != tInvalid {
			fc.errorf(n.pos, "? wants a bool, not a %s", ct)
		}
		a, at := fc.expr(n.a, want)
		b, bt := fc.expr(n.b, want)
		t := unify(at, bt)
		if t == nil {
			fc.errorf(n.pos, "the sides of ? : are a %s and a %s", at, bt)
			return bad, typInvalid
		}
		return func(fr *frame) (Value, error) {
			c, err := cond(fr)
			if err != nil {
				return Value{}, err
			}
			if c.b {
				return a(fr)
			}
			return b(fr)
		}, t
	case *callNode:
		return fc.call(n)
	case *memberNode:
		return fc.member(n)
	case *indexNode:
		x, xt := fc.expr(n.x, nil)
		i, it := fc.expr(n.i, nil)
		if xt.kind == tInvalid {
			return bad, xt
		}
		if xt.kind == tMap {
			fc.errorf(n.pos, "read a map with get(key), not [ ]")
			return bad, typInvalid
		}
		if xt.kind != tArray {
			fc.errorf(n.pos, "only arrays have elements, not a %s", xt)
			return bad, typInvalid
		}
		if it.kind != tNumber && it.kind != tInvalid {
			fc.errorf(n.i.position(), "an index is a number, not a %s", it)
		}
		pos := n.pos
		return func(fr *frame) (Value, error) {
			items, idx, err := index(fr, x, i, m, pos)
			if err != nil {
				return Value{}, err
			}
			return items[idx], nil
		}, xt.elem
	case *methodCallNode:
		return fc.methodCall(n)
	case *superCallNode:
		fc.errorf(n.pos, "super(…) is only the first statement of a constructor")
		return bad, typInvalid
	case *superMethodNode:
		return fc.superMethod(n)
	case *newNode:
		return fc.newExpr(n)
	case *arrayNode:
		return fc.array(n, want)
	}
	fc.errorf(n.position(), "unexpected expression")
	return bad, typInvalid
}

// index evaluates x[i] up to the element: the array's items and the index, checked.
func index(fr *frame, x, i evalFn, m *moduleInfo, pos int) ([]Value, int, error) {
	a, err := x(fr)
	if err != nil {
		return nil, 0, err
	}
	iv, err := i(fr)
	if err != nil {
		return nil, 0, err
	}
	var items []Value
	if a.kind == Array {
		items = a.ref.(*arrayVal).items
	}
	n := iv.num
	if n != float64(int(n)) || n < 0 || int(n) >= len(items) {
		return nil, 0, m.errAt(pos, "index %s is out of range (length %d)", iv, len(items))
	}
	return items, int(n), nil
}

func unify(a, b *Type) *Type {
	switch {
	case a.kind == tInvalid:
		return b
	case b.kind == tInvalid:
		return a
	case assignable(b, a):
		return a
	case assignable(a, b):
		return b
	}
	return nil
}

func (fc *funcCtx) name(n *nameNode) (evalFn, *Type) {
	if fc.probing(n.name) {
		fc.c.rec.probe.names(fc)
		return bad, typInvalid
	}
	if l := fc.lookup(n.name); l != nil {
		fc.record(n.pos, n.name, localRef(l, n.name, fc.m, false))
		slot := l.slot
		return func(fr *frame) (Value, error) { return fr.locals[slot], nil }, l.typ
	}
	sym := fc.m.symbols[n.name]
	switch {
	case sym != nil && sym.kind == symInvalid:
		return bad, typInvalid
	case sym == nil && stdSigs[n.name] != nil:
		fc.errorf(n.pos, "%s is a function: call it", n.name)
	case sym == nil:
		fc.errorf(n.pos, "unknown name %s", n.name)
	case sym.kind == symConst:
		k := sym.konst
		if k.typ == nil {
			fc.errorf(n.pos, "%s is used before it is declared", n.name)
			return bad, typInvalid
		}
		fc.record(n.pos, n.name, fc.c.symbolRef(sym, false, aliasLoc(fc.m, sym, n.name)))
		mod, slot := k.mod, k.slot
		return func(*frame) (Value, error) { return mod.globals[slot], nil }, k.typ
	case sym.kind == symFunc:
		fc.errorf(n.pos, "%s is a function: call it", n.name)
	case sym.kind == symClass:
		fc.errorf(n.pos, "%s is a class: make one with new %s(…)", n.name, n.name)
	default:
		fc.errorf(n.pos, "%s is a type, not a value", n.name)
	}
	return bad, typInvalid
}

// binaryOp checks a binary operator on operands of types lt and rt, and returns its code and result type, or
// nil code when it doesn't apply (and says so).
func (fc *funcCtx) binaryOp(op string, lt, rt *Type, pos int) (func(fr *frame, a, b Value) (Value, error), *Type) {
	m := fc.m
	if lt.kind == tInvalid || rt.kind == tInvalid {
		return nil, typInvalid
	}
	switch op {
	case "==", "!=":
		ok := identical(lt, rt) || (lt.kind == tNull && rt.isRef()) || (rt.kind == tNull && lt.isRef()) ||
			(lt.isRef() && rt.isRef() && (assignable(lt, rt) || assignable(rt, lt)))
		if !ok {
			fc.errorf(pos, "%s compares a %s with a %s", op, lt, rt)
			return nil, typInvalid
		}
		eq := op == "=="
		return func(_ *frame, a, b Value) (Value, error) { return Boolean(sameValue(a, b) == eq), nil }, typBool
	case "<", "<=", ">", ">=":
		if !((lt.kind == tNumber && rt.kind == tNumber) || (lt.kind == tString && rt.kind == tString)) {
			fc.errorf(pos, "%s compares numbers or strings, not a %s with a %s", op, lt, rt)
			return nil, typInvalid
		}
		cmp := &binaryNode{op: op}
		return func(_ *frame, a, b Value) (Value, error) { return cmp.compare(a, b) }, typBool
	case "+":
		if (lt.kind == tString || rt.kind == tString) && lt.primitive() && rt.primitive() {
			return func(fr *frame, a, b Value) (Value, error) {
				s := a.String() + b.String()
				if err := fr.vm.checkString(m, pos, s); err != nil {
					return Value{}, err
				}
				return Str(s), nil
			}, typString
		}
	}
	if lt.kind != tNumber || rt.kind != tNumber {
		if op == "+" {
			fc.errorf(pos, "+ wants numbers, or a string and a number, string or bool; not a %s and a %s", lt, rt)
		} else {
			fc.errorf(pos, "%s wants numbers, not a %s and a %s", op, lt, rt)
		}
		return nil, typInvalid
	}
	return func(_ *frame, a, b Value) (Value, error) {
		f, msg := arith(op, a.num, b.num)
		if msg != "" {
			return Value{}, m.errAt(pos, "%s", msg)
		}
		return Num(f), nil
	}, typNumber
}

// args checks a call's arguments against params, each given its parameter's type as the type it wants.
func (fc *funcCtx) args(pos int, what string, params []*Type, args []node) []evalFn {
	if len(args) != len(params) {
		fc.errorf(pos, "%s takes %s, not %d", what, plural(len(params)), len(args))
	}
	fns := make([]evalFn, len(args))
	for i, a := range args {
		var want *Type
		if i < len(params) {
			want = params[i]
		}
		fn, t := fc.expr(a, want)
		if want != nil && !assignable(t, want) {
			fc.errorf(a.position(), "argument %d of %s is a %s, not a %s", i+1, what, t, want)
		}
		fns[i] = fn
	}
	return fns
}

func evalArgs(fr *frame, fns []evalFn) ([]Value, error) {
	vals := make([]Value, len(fns))
	for i, fn := range fns {
		v, err := fn(fr)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

// call checks a call of a function by name: one of the module's, an imported one, or the standard library's.
func (fc *funcCtx) call(n *callNode) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	if fc.probing(n.name) {
		fc.c.rec.probe.names(fc)
		for _, a := range n.args {
			fc.expr(a, nil)
		}
		return bad, typInvalid
	}
	if fc.lookup(n.name) != nil {
		fc.errorf(pos, "%s is a variable, not a function", n.name)
		return bad, typInvalid
	}
	sym := fc.m.symbols[n.name]
	if sym != nil && sym.kind == symInvalid {
		for _, a := range n.args {
			fc.expr(a, nil)
		}
		return bad, typInvalid
	}
	if sym == nil {
		if sig := stdSigs[n.name]; sig != nil {
			fc.record(pos, n.name, stdRef(n.name, sig))
			return fc.callStd(n, sig)
		}
		fc.errorf(pos, "unknown function %s", n.name)
		return bad, typInvalid
	}
	if sym.kind != symFunc {
		if sym.kind == symClass {
			fc.errorf(pos, "make a %s with new %s(…)", n.name, n.name)
		} else {
			fc.errorf(pos, "%s is not a function", n.name)
		}
		return bad, typInvalid
	}
	fn := sym.fn
	if fn.result == nil {
		fc.c.resolveSig(fn, fn.decl, sym.mod)
	}
	fc.record(pos, n.name, fc.c.symbolRef(sym, false, aliasLoc(fc.m, sym, n.name)))
	args := fc.args(pos, n.name, fn.params, n.args)
	return func(fr *frame) (Value, error) {
		vals, err := evalArgs(fr, args)
		if err != nil {
			return Value{}, err
		}
		return fr.vm.call(fn, Value{}, vals, m, pos)
	}, fn.result
}

func (fc *funcCtx) member(n *memberNode) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	x, xt := fc.expr(n.x, nil)
	if fc.probing(n.name) {
		fc.c.rec.probe.members(fc, xt)
		return bad, typInvalid
	}
	switch xt.kind {
	case tInvalid:
		return bad, xt
	case tArray, tString, tMap:
		prop := builtinProps[xt.kind]
		if prop.name != n.name {
			if builtinMethods[xt.kind][n.name] != nil {
				fc.errorf(pos, "%s is a method: call it", n.name)
			} else {
				fc.errorf(pos, "a %s has no field %s", xt, n.name)
			}
			return bad, typInvalid
		}
		fc.record(pos, n.name, Ref{Kind: RefBuiltin, Detail: "(property) " + xt.String() + "." + n.name + ": number"})
		return func(fr *frame) (Value, error) {
			v, err := x(fr)
			if err != nil {
				return Value{}, err
			}
			switch v.kind {
			case String:
				return Num(float64(utf8.RuneCountInString(v.str))), nil
			case Array:
				return Num(float64(len(v.ref.(*arrayVal).items))), nil
			case Map:
				return Num(float64(len(v.ref.(*mapVal).keys))), nil
			}
			return Num(0), nil // a null array or map is empty
		}, typNumber
	case tClass:
		ci := xt.class
		f := ci.fieldByName[n.name]
		if f == nil {
			if ci.methods[n.name] != nil {
				fc.errorf(pos, "%s is a method: call it", n.name)
			} else {
				fc.errorf(pos, "%s has no field %s", ci.name, n.name)
			}
			return bad, typInvalid
		}
		if !fc.canSee(f.owner, f.private) {
			fc.errorf(pos, "%s is private to %s", n.name, f.owner.name)
		}
		fc.record(pos, n.name, fieldRef(f.owner.name, f, false))
		return func(fr *frame) (Value, error) {
			o, err := receiver(fr, x, m, pos, n.name)
			if err != nil {
				return Value{}, err
			}
			return fr.vm.readField(o, f, m, pos)
		}, f.typ
	case tIface:
		f := xt.iface.fields[n.name]
		if f == nil {
			if xt.iface.methods[n.name] != nil {
				fc.errorf(pos, "%s is a method: call it", n.name)
			} else {
				fc.errorf(pos, "%s has no field %s", xt.iface.name, n.name)
			}
			return bad, typInvalid
		}
		fc.record(pos, n.name, fieldRef(xt.iface.name, f, false))
		name := n.name
		return func(fr *frame) (Value, error) {
			o, err := receiver(fr, x, m, pos, name)
			if err != nil {
				return Value{}, err
			}
			return fr.vm.readField(o, o.class.fieldByName[name], m, pos)
		}, f.typ
	}
	fc.errorf(pos, "a %s has no fields", xt)
	return bad, typInvalid
}

// receiver evaluates the object a member is used on, refusing null.
func receiver(fr *frame, x evalFn, m *moduleInfo, pos int, member string) (*object, error) {
	v, err := x(fr)
	if err != nil {
		return nil, err
	}
	if v.kind != Object {
		return nil, m.errAt(pos, "%s of null", member)
	}
	return v.ref.(*object), nil
}

func (fc *funcCtx) methodCall(n *methodCallNode) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	x, xt := fc.expr(n.recv, nil)
	if fc.probing(n.name) {
		fc.c.rec.probe.members(fc, xt)
		for _, a := range n.args {
			fc.expr(a, nil)
		}
		return bad, typInvalid
	}
	var fn *funcInfo
	owner := ""
	switch xt.kind {
	case tInvalid:
		for _, a := range n.args {
			fc.expr(a, nil)
		}
		return bad, xt
	case tArray, tString, tMap:
		return fc.builtinMethod(n, x, xt)
	case tClass:
		fn = xt.class.methods[n.name]
		if fn == nil {
			fc.errorf(pos, "%s has no method %s", xt.class.name, n.name)
			return bad, typInvalid
		}
		if !fc.canSee(fn.owner, fn.private) {
			fc.errorf(pos, "%s is private to %s", n.name, fn.owner.name)
		}
		owner = fn.owner.name
	case tIface:
		fn = xt.iface.methods[n.name]
		if fn == nil {
			fc.errorf(pos, "%s has no method %s", xt.iface.name, n.name)
			return bad, typInvalid
		}
		if fn.optional {
			fc.errorf(pos, "%s is optional, so only the host calls it", n.name)
		}
		owner = xt.iface.name
	default:
		fc.errorf(pos, "a %s has no methods", xt)
		return bad, typInvalid
	}
	fc.record(pos, n.name, methodRef(owner, fn, false))
	args := fc.args(pos, n.name, fn.params, n.args)
	name := n.name
	return func(fr *frame) (Value, error) {
		o, err := receiver(fr, x, m, pos, name+"()")
		if err != nil {
			return Value{}, err
		}
		vals, err := evalArgs(fr, args)
		if err != nil {
			return Value{}, err
		}
		return fr.vm.call(o.class.methods[name], Value{kind: Object, ref: o}, vals, m, pos)
	}, fn.result
}

func (fc *funcCtx) superMethod(n *superMethodNode) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	if fc.class == nil || fc.class.super == nil || fc.inInit {
		fc.errorf(pos, "super is only in the methods of a class that extends another")
		return bad, typInvalid
	}
	if fc.probing(n.name) {
		fc.c.rec.probe.members(fc, fc.class.super.typ)
		return bad, typInvalid
	}
	fn := fc.class.super.methods[n.name]
	if fn == nil || fn.private {
		fc.errorf(pos, "%s has no method %s", fc.class.super.name, n.name)
		return bad, typInvalid
	}
	fc.record(pos, n.name, methodRef(fn.owner.name, fn, false))
	args := fc.args(pos, n.name, fn.params, n.args)
	return func(fr *frame) (Value, error) {
		vals, err := evalArgs(fr, args)
		if err != nil {
			return Value{}, err
		}
		return fr.vm.call(fn, fr.this, vals, m, pos)
	}, fn.result
}

// superCall checks super(args) at the start of a constructor.
func (fc *funcCtx) superCall(n *superCallNode) execFn {
	m, pos := fc.m, n.pos
	super := fc.class.super
	args := fc.args(pos, "super", super.ctor.params, n.args)
	return func(fr *frame) (flow, error) {
		vals, err := evalArgs(fr, args)
		if err != nil {
			return flowNext, err
		}
		_, err = fr.vm.call(super.ctor, fr.this, vals, m, pos)
		return flowNext, err
	}
}

func (fc *funcCtx) newExpr(n *newNode) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	t := fc.c.resolveType(m, n.typ)
	switch t.kind {
	case tInvalid:
		return bad, t
	case tMap:
		if len(n.args) > 0 {
			fc.errorf(pos, "new Map takes no arguments")
		}
		return func(fr *frame) (Value, error) {
			if err := fr.vm.alloc(m, pos, valueSize); err != nil {
				return Value{}, err
			}
			return Value{kind: Map, ref: &mapVal{index: map[mapKey]int{}}}, nil
		}, t
	case tClass:
		ci := t.class
		if ci.native {
			fc.errorf(pos, "%s is the host's: scripts can't make one", ci.name)
			return bad, typInvalid
		}
		if ci.ctor == nil {
			return bad, typInvalid // it failed to resolve
		}
		args := fc.args(pos, "new "+ci.name, ci.ctor.params, n.args)
		return func(fr *frame) (Value, error) {
			vals, err := evalArgs(fr, args)
			if err != nil {
				return Value{}, err
			}
			obj, err := fr.vm.newObject(ci, m, pos)
			if err != nil {
				return Value{}, err
			}
			if _, err := fr.vm.call(ci.ctor, obj, vals, m, pos); err != nil {
				return Value{}, err
			}
			return obj, nil
		}, t
	}
	fc.errorf(pos, "only classes and maps are made with new, not %s", t)
	return bad, typInvalid
}

func (fc *funcCtx) array(n *arrayNode, want *Type) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	var elem *Type
	if want != nil && want.kind == tArray {
		elem = want.elem
	}
	items := make([]evalFn, len(n.items))
	for i, item := range n.items {
		fn, t := fc.expr(item, elem)
		items[i] = fn
		switch {
		case elem == nil && t.kind != tNull:
			elem = t
		case elem == nil:
		case !assignable(t, elem):
			fc.errorf(item.position(), "an array of %s can't hold a %s", elem, t)
		}
	}
	if elem == nil {
		fc.errorf(pos, "give the array a type, as in let xs: number[] = []")
		return bad, typInvalid
	}
	return func(fr *frame) (Value, error) {
		if err := fr.vm.checkLength(m, pos, len(items), "array"); err != nil {
			return Value{}, err
		}
		if err := fr.vm.alloc(m, pos, valueSize*(len(items)+1)); err != nil {
			return Value{}, err
		}
		vals, err := evalArgs(fr, items)
		if err != nil {
			return Value{}, err
		}
		return Value{kind: Array, ref: &arrayVal{items: vals}}, nil
	}, arrayOf(elem)
}
