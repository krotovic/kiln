package language

import (
	"fmt"
	"math"
	"unsafe"
)

// Scripts are checked into trees of closures (check_expr.go, check_stmt.go): each expression becomes an evalFn
// and each statement an execFn, with names already resolved to local slots, fields to their slots and operators
// to the code for their types. Running a script calls them.

type evalFn func(fr *frame) (Value, error)

type execFn func(fr *frame) (flow, error)

// flow says how a statement ended.
type flow uint8

const (
	flowNext flow = iota
	flowBreak
	flowContinue
	flowReturn
)

// object is an instance of a class. A native class's objects carry the host's payload instead of fields.
type object struct {
	class   *classInfo
	fields  []Value
	payload any
}

type arrayVal struct{ items []Value }

// mapVal keeps its entries in insertion order, so iterating a map is deterministic.
type mapVal struct {
	keys, vals []Value
	index      map[mapKey]int
}

type mapKey struct {
	kind Kind
	num  float64
	str  string
}

func keyOf(v Value) mapKey {
	if v.kind == Number {
		n := v.num
		if n == 0 {
			n = 0 // -0 and 0 are one key
		}
		return mapKey{kind: Number, num: n}
	}
	return mapKey{kind: String, str: v.str}
}

func (m *mapVal) get(k Value) (Value, bool) {
	if m == nil {
		return Value{}, false
	}
	i, ok := m.index[keyOf(k)]
	if !ok {
		return Value{}, false
	}
	return m.vals[i], true
}

func (m *mapVal) delete(k Value) bool {
	key := keyOf(k)
	i, ok := m.index[key]
	if !ok {
		return false
	}
	delete(m.index, key)
	m.keys = append(m.keys[:i], m.keys[i+1:]...)
	m.vals = append(m.vals[:i], m.vals[i+1:]...)
	for j := i; j < len(m.keys); j++ {
		m.index[keyOf(m.keys[j])] = j
	}
	return true
}

// vm is one call from the host into a script, with its budgets.
type vm struct {
	limits    ScriptLimits
	steps     int
	depth     int
	allocated int // bytes allocated in this call
	data      any
}

type frame struct {
	vm     *vm
	locals []Value
	this   Value
	ret    Value
}

// valueSize is what one value costs in the memory budget; strings add their bytes.
const valueSize = int(unsafe.Sizeof(Value{}))

// step counts one statement, loop turn or call against the budget.
func (v *vm) step(m *moduleInfo, pos int) error {
	v.steps++
	if v.steps > v.limits.MaxSteps {
		return m.errAt(pos, "ran more than %d steps", v.limits.MaxSteps)
	}
	return nil
}

// alloc counts bytes against the call's memory budget.
func (v *vm) alloc(m *moduleInfo, pos int, bytes int) error {
	v.allocated += bytes
	if v.allocated > v.limits.MaxHeap {
		return m.errAt(pos, "used more than %d bytes of memory", v.limits.MaxHeap)
	}
	return nil
}

// checkLength refuses arrays, maps and strings over their limits.
func (v *vm) checkLength(m *moduleInfo, pos int, n int, what string) error {
	if n > v.limits.MaxLength {
		return m.errAt(pos, "an %s longer than %d", what, v.limits.MaxLength)
	}
	return nil
}

func (v *vm) checkString(m *moduleInfo, pos int, s string) error {
	if v.limits.MaxString > 0 && len(s) > v.limits.MaxString {
		return m.errAt(pos, "a string longer than %d characters", v.limits.MaxString)
	}
	return v.alloc(m, pos, len(s))
}

// call runs f with this and args.
func (v *vm) call(f *funcInfo, this Value, args []Value, m *moduleInfo, pos int) (Value, error) {
	if err := v.step(m, pos); err != nil {
		return Value{}, err
	}
	if v.depth >= v.limits.MaxDepth {
		return Value{}, m.errAt(pos, "calls nested deeper than %d", v.limits.MaxDepth)
	}
	v.depth++
	defer func() { v.depth-- }()
	if f.native != nil {
		out, err := f.native(&Call{vm: v}, this, args)
		if err != nil {
			if _, ok := err.(*ScriptError); ok {
				return Value{}, err
			}
			return Value{}, m.errAt(pos, "%s: %v", f.name, err)
		}
		if f.result.kind == tVoid {
			return Value{}, nil
		}
		if !fits(out, f.result) {
			return Value{}, m.errAt(pos, "%s gave a %s, not a %s", f.name, out.Kind(), f.result)
		}
		return out, nil
	}
	fr := &frame{vm: v, locals: make([]Value, f.nlocals), this: this}
	copy(fr.locals, args)
	if _, err := f.body(fr); err != nil {
		return Value{}, err
	}
	if f.result.kind != tVoid && fr.ret.kind == 0 {
		return zeroOf(f.result), nil
	}
	return fr.ret, nil
}

// newObject makes an object of c with every field at its zero value, without running its constructor.
func (v *vm) newObject(c *classInfo, m *moduleInfo, pos int) (Value, error) {
	o := &object{class: c, fields: make([]Value, len(c.fields))}
	for i, f := range c.fields {
		o.fields[i] = zeroOf(f.typ)
	}
	if err := v.alloc(m, pos, valueSize*(len(o.fields)+1)); err != nil {
		return Value{}, err
	}
	return Value{kind: Object, ref: o}, nil
}

// readField reads f of the object o, through the host for a native class's field.
func (v *vm) readField(o *object, f *fieldInfo, m *moduleInfo, pos int) (Value, error) {
	if f.getter != nil {
		out, err := f.getter(&Call{vm: v}, Value{kind: Object, ref: o}, nil)
		if err != nil {
			return Value{}, m.errAt(pos, "%s: %v", f.name, err)
		}
		if !fits(out, f.typ) {
			return Value{}, m.errAt(pos, "%s gave a %s, not a %s", f.name, out.Kind(), f.typ)
		}
		return out, nil
	}
	return o.fields[f.slot], nil
}

// fits reports whether a value the host gave has the kind of t (null for a reference type).
func fits(v Value, t *Type) bool {
	return v.kind == t.valueKind() || (v.kind == Null && t.isRef())
}

// arith applies a numeric operator, refusing results that aren't finite.
func arith(op string, a, b float64) (float64, string) {
	var f float64
	switch op {
	case "+":
		f = a + b
	case "-":
		f = a - b
	case "*":
		f = a * b
	case "/":
		if b == 0 {
			return 0, "division by zero"
		}
		f = a / b
	case "%":
		if b == 0 {
			return 0, "division by zero"
		}
		f = math.Mod(a, b)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, op + " gives a number too large"
	}
	return f, ""
}

// sameValue is == in scripts: primitives by value, references by identity, and null equal only to null.
func sameValue(l, r Value) bool {
	if l.kind != r.kind {
		return false
	}
	switch l.kind {
	case Null:
		return true
	case Array, Map, Object:
		return l.ref == r.ref
	}
	return equal(l, r)
}

// heapSize is the memory the values reachable from root take, each counted once.
func heapSize(root Value, limit int) int {
	seen := map[any]bool{}
	size := 0
	stack := []Value{root}
	for len(stack) > 0 && size <= limit {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		size += valueSize + len(v.str)
		if v.ref == nil || seen[v.ref] {
			continue
		}
		seen[v.ref] = true
		switch r := v.ref.(type) {
		case *object:
			stack = append(stack, r.fields...)
		case *arrayVal:
			stack = append(stack, r.items...)
		case *mapVal:
			stack = append(stack, r.keys...)
			stack = append(stack, r.vals...)
		}
	}
	return size
}

// ScriptError is an error while a script runs: a failed operation, a null used, or a budget used up. It names
// the module, line and column, and matches ErrEval.
type ScriptError struct {
	Module string
	Line   int
	Col    int
	Msg    string
}

func (e *ScriptError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.Module, e.Line, e.Col, e.Msg)
}
func (e *ScriptError) Is(target error) bool { return target == ErrEval }

func (m *moduleInfo) errAt(pos int, format string, args ...any) error {
	line, col := m.lineCol(pos)
	return &ScriptError{Module: m.id, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}
