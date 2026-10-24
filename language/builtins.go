package language

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// stdSig is the type of a standard library function in scripts. A nil parameter takes any number, string or
// bool; with variadic, the last parameter repeats.
type stdSig struct {
	fn       Func
	params   []*Type
	variadic bool
	result   *Type
}

// stdSigs types the standard library (library.go) for scripts, which call the same functions.
var stdSigs = func() map[string]*stdSig {
	num1 := []*Type{typNumber}
	sigs := map[string]*stdSig{
		"min":    {params: num1, variadic: true, result: typNumber},
		"max":    {params: num1, variadic: true, result: typNumber},
		"floor":  {params: num1, result: typNumber},
		"ceil":   {params: num1, result: typNumber},
		"round":  {params: num1, result: typNumber},
		"abs":    {params: num1, result: typNumber},
		"clamp":  {params: []*Type{typNumber, typNumber, typNumber}, result: typNumber},
		"num":    {params: []*Type{nil}, result: typNumber},
		"int":    {params: []*Type{nil}, result: typNumber},
		"str":    {params: []*Type{nil}, result: typString},
		"bool":   {params: []*Type{nil}, result: typBool},
		"len":    {params: []*Type{typString}, result: typNumber},
		"concat": {params: []*Type{nil}, variadic: true, result: typString},
	}
	for name, sig := range sigs {
		sig.fn, _ = std.Lookup(name)
	}
	return sigs
}()

// builtinNames can't be declared by modules: they are the standard library's.
var builtinNames = func() map[string]bool {
	out := map[string]bool{}
	for name := range stdSigs {
		out[name] = true
	}
	return out
}()

func (fc *funcCtx) callStd(n *callNode, sig *stdSig) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	if len(n.args) < len(sig.params) || (!sig.variadic && len(n.args) > len(sig.params)) {
		want := plural(len(sig.params))
		if sig.variadic {
			want += " or more"
		}
		fc.errorf(pos, "%s takes %s, not %d", n.name, want, len(n.args))
	}
	args := make([]evalFn, len(n.args))
	for i, a := range n.args {
		p := sig.params[min(i, len(sig.params)-1)]
		fn, t := fc.expr(a, p)
		args[i] = fn
		switch {
		case t.kind == tInvalid:
		case p == nil && !t.primitive():
			fc.errorf(a.position(), "argument %d of %s is a number, string or bool, not a %s", i+1, n.name, t)
		case p != nil && !assignable(t, p):
			fc.errorf(a.position(), "argument %d of %s is a %s, not a %s", i+1, n.name, t, p)
		}
	}
	name, call := n.name, sig.fn.Call
	return func(fr *frame) (Value, error) {
		vals, err := evalArgs(fr, args)
		if err != nil {
			return Value{}, err
		}
		v, err := call(vals)
		if err != nil {
			return Value{}, m.errAt(pos, "%s: %v", name, err)
		}
		if f, ok := v.AsNumber(); ok && (math.IsInf(f, 0) || math.IsNaN(f)) {
			return Value{}, m.errAt(pos, "%s gives a number too large", name)
		}
		if s, ok := v.AsString(); ok {
			if err := fr.vm.checkString(m, pos, s); err != nil {
				return Value{}, err
			}
		}
		return v, nil
	}, sig.result
}

// builtinProp is the one field of arrays, strings and maps: their length or size.
type builtinProp struct{ name string }

var builtinProps = map[typeKind]builtinProp{tArray: {"length"}, tString: {"length"}, tMap: {"size"}}

// builtinMethod is a method of arrays, strings or maps. params and result get the receiver's type; a null
// receiver (an array or map) is empty, and writes refuse it.
type builtinMethod struct {
	minArgs, maxArgs int // maxArgs -1: the last parameter repeats
	params           func(recv *Type) []*Type
	result           func(recv *Type) *Type
	writes           bool
	call             func(c *builtinCall, recv Value, args []Value) (Value, error)
}

// builtinCall is what a built-in method may charge: its steps and allocations.
type builtinCall struct {
	vm  *vm
	m   *moduleInfo
	pos int
}

func (c *builtinCall) cost(n int) error {
	c.vm.steps += n / 16
	return c.vm.step(c.m, c.pos)
}

func (c *builtinCall) array(items []Value) (Value, error) {
	if err := c.vm.checkLength(c.m, c.pos, len(items), "array"); err != nil {
		return Value{}, err
	}
	if err := c.vm.alloc(c.m, c.pos, valueSize*(len(items)+1)); err != nil {
		return Value{}, err
	}
	return Value{kind: Array, ref: &arrayVal{items: items}}, nil
}

func (c *builtinCall) str(s string) (Value, error) {
	if err := c.vm.checkString(c.m, c.pos, s); err != nil {
		return Value{}, err
	}
	return Str(s), nil
}

func elemOf(t *Type) *Type { return t.elem }
func same(t *Type) *Type   { return t }
func types(ts ...*Type) func(*Type) []*Type {
	return func(*Type) []*Type { return ts }
}
func result(t *Type) func(*Type) *Type { return func(*Type) *Type { return t } }

func itemsOf(v Value) []Value {
	if v.kind != Array {
		return nil
	}
	return v.ref.(*arrayVal).items
}

func mapOfValue(v Value) *mapVal {
	if v.kind != Map {
		return nil
	}
	return v.ref.(*mapVal)
}

// sliceRange turns slice's start and end (negative ones counting from the end, as in TypeScript) into bounds
// within 0..n.
func sliceRange(args []Value, n int) (int, int, error) {
	bound := func(i int, def int) (int, error) {
		if i >= len(args) {
			return def, nil
		}
		f := args[i].num
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("slice wants whole numbers, not %s", args[i])
		}
		x := int(math.Max(math.Min(f, float64(n)), float64(-n)))
		if x < 0 {
			x += n
		}
		return x, nil
	}
	start, err := bound(0, 0)
	if err != nil {
		return 0, 0, err
	}
	end, err := bound(1, n)
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		end = start
	}
	return start, end, nil
}

var builtinMethods = map[typeKind]map[string]*builtinMethod{
	tArray: {
		"push": {minArgs: 1, maxArgs: -1, writes: true,
			params: func(t *Type) []*Type { return []*Type{t.elem} }, result: result(typVoid),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				a := recv.ref.(*arrayVal)
				if err := c.vm.checkLength(c.m, c.pos, len(a.items)+len(args), "array"); err != nil {
					return Value{}, err
				}
				if err := c.vm.alloc(c.m, c.pos, valueSize*len(args)); err != nil {
					return Value{}, err
				}
				a.items = append(a.items, args...)
				return Value{}, nil
			}},
		"pop": {writes: true, params: types(), result: elemOf,
			call: func(c *builtinCall, recv Value, _ []Value) (Value, error) {
				a := recv.ref.(*arrayVal)
				if len(a.items) == 0 {
					return Value{}, c.m.errAt(c.pos, "pop on an empty array")
				}
				last := a.items[len(a.items)-1]
				a.items = a.items[:len(a.items)-1]
				return last, nil
			}},
		"slice": {maxArgs: 2, params: types(typNumber, typNumber), result: same,
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				items := itemsOf(recv)
				start, end, err := sliceRange(args, len(items))
				if err != nil {
					return Value{}, c.m.errAt(c.pos, "%v", err)
				}
				if err := c.cost(end - start); err != nil {
					return Value{}, err
				}
				return c.array(append([]Value(nil), items[start:end]...))
			}},
		"indexOf": {minArgs: 1, maxArgs: 1, params: func(t *Type) []*Type { return []*Type{t.elem} }, result: result(typNumber),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				items := itemsOf(recv)
				if err := c.cost(len(items)); err != nil {
					return Value{}, err
				}
				for i, item := range items {
					if sameValue(item, args[0]) {
						return Num(float64(i)), nil
					}
				}
				return Num(-1), nil
			}},
		"includes": {minArgs: 1, maxArgs: 1, params: func(t *Type) []*Type { return []*Type{t.elem} }, result: result(typBool),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				items := itemsOf(recv)
				if err := c.cost(len(items)); err != nil {
					return Value{}, err
				}
				for _, item := range items {
					if sameValue(item, args[0]) {
						return Boolean(true), nil
					}
				}
				return Boolean(false), nil
			}},
		"join": {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typString),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				items := itemsOf(recv)
				if err := c.cost(len(items)); err != nil {
					return Value{}, err
				}
				parts := make([]string, len(items))
				size := 0
				for i, item := range items {
					parts[i] = item.String()
					if size += len(parts[i]) + len(args[0].str); c.vm.limits.MaxString > 0 && size > c.vm.limits.MaxString {
						return Value{}, c.m.errAt(c.pos, "a string longer than %d characters", c.vm.limits.MaxString)
					}
				}
				return c.str(strings.Join(parts, args[0].str))
			}},
	},
	tMap: {
		"get": {minArgs: 1, maxArgs: 1, params: func(t *Type) []*Type { return []*Type{t.key} }, result: elemOf,
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				if v, ok := mapOfValue(recv).get(args[0]); ok {
					return v, nil
				}
				return Value{}, nil // the caller gives the zero value of the map's value type
			}},
		"set": {minArgs: 2, maxArgs: 2, writes: true, params: func(t *Type) []*Type { return []*Type{t.key, t.elem} }, result: result(typVoid),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				mv := recv.ref.(*mapVal)
				key := keyOf(args[0])
				if i, ok := mv.index[key]; ok {
					mv.vals[i] = args[1]
					return Value{}, nil
				}
				if err := c.vm.checkLength(c.m, c.pos, len(mv.keys)+1, "map"); err != nil {
					return Value{}, err
				}
				if err := c.vm.alloc(c.m, c.pos, 2*valueSize); err != nil {
					return Value{}, err
				}
				mv.index[key] = len(mv.keys)
				mv.keys = append(mv.keys, args[0])
				mv.vals = append(mv.vals, args[1])
				return Value{}, nil
			}},
		"has": {minArgs: 1, maxArgs: 1, params: func(t *Type) []*Type { return []*Type{t.key} }, result: result(typBool),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				_, ok := mapOfValue(recv).get(args[0])
				return Boolean(ok), nil
			}},
		"delete": {minArgs: 1, maxArgs: 1, writes: true, params: func(t *Type) []*Type { return []*Type{t.key} }, result: result(typBool),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				mv := recv.ref.(*mapVal)
				if err := c.cost(len(mv.keys)); err != nil {
					return Value{}, err
				}
				return Boolean(mv.delete(args[0])), nil
			}},
		"keys": {params: types(), result: func(t *Type) *Type { return arrayOf(t.key) },
			call: func(c *builtinCall, recv Value, _ []Value) (Value, error) {
				var keys []Value
				if mv := mapOfValue(recv); mv != nil {
					keys = mv.keys
				}
				if err := c.cost(len(keys)); err != nil {
					return Value{}, err
				}
				return c.array(append([]Value{}, keys...))
			}},
		"values": {params: types(), result: func(t *Type) *Type { return arrayOf(t.elem) },
			call: func(c *builtinCall, recv Value, _ []Value) (Value, error) {
				var vals []Value
				if mv := mapOfValue(recv); mv != nil {
					vals = mv.vals
				}
				if err := c.cost(len(vals)); err != nil {
					return Value{}, err
				}
				return c.array(append([]Value{}, vals...))
			}},
	},
	tString: {
		"slice": {maxArgs: 2, params: types(typNumber, typNumber), result: result(typString),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				runes := []rune(recv.str)
				start, end, err := sliceRange(args, len(runes))
				if err != nil {
					return Value{}, c.m.errAt(c.pos, "%v", err)
				}
				return c.str(string(runes[start:end]))
			}},
		"indexOf": {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typNumber),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				i := strings.Index(recv.str, args[0].str)
				if i < 0 {
					return Num(-1), nil
				}
				return Num(float64(utf8.RuneCountInString(recv.str[:i]))), nil
			}},
		"includes":    stringTest(strings.Contains),
		"startsWith":  stringTest(strings.HasPrefix),
		"endsWith":    stringTest(strings.HasSuffix),
		"toUpperCase": stringMap(strings.ToUpper),
		"toLowerCase": stringMap(strings.ToLower),
		"trim":        stringMap(strings.TrimSpace),
		"split": {minArgs: 1, maxArgs: 1, params: types(typString), result: result(arrayOf(typString)),
			call: func(c *builtinCall, recv Value, args []Value) (Value, error) {
				parts := strings.Split(recv.str, args[0].str)
				if err := c.vm.checkLength(c.m, c.pos, len(parts), "array"); err != nil {
					return Value{}, err
				}
				items := make([]Value, len(parts))
				for i, p := range parts {
					items[i] = Str(p)
				}
				return c.array(items)
			}},
	},
}

func stringTest(test func(s, sub string) bool) *builtinMethod {
	return &builtinMethod{minArgs: 1, maxArgs: 1, params: types(typString), result: result(typBool),
		call: func(_ *builtinCall, recv Value, args []Value) (Value, error) {
			return Boolean(test(recv.str, args[0].str)), nil
		}}
}

func stringMap(fn func(string) string) *builtinMethod {
	return &builtinMethod{params: types(), result: result(typString),
		call: func(c *builtinCall, recv Value, _ []Value) (Value, error) { return c.str(fn(recv.str)) }}
}

// builtinMethod checks a call of a method of an array, string or map.
func (fc *funcCtx) builtinMethod(n *methodCallNode, x evalFn, xt *Type) (evalFn, *Type) {
	m, pos := fc.m, n.pos
	bm := builtinMethods[xt.kind][n.name]
	if bm == nil {
		fc.errorf(pos, "a %s has no method %s", xt, n.name)
		return bad, typInvalid
	}
	fc.record(pos, n.name, builtinMethodRef(xt, n.name, bm))
	params := bm.params(xt)
	if len(n.args) < bm.minArgs || (bm.maxArgs >= 0 && len(n.args) > bm.maxArgs) {
		want := plural(bm.minArgs)
		switch {
		case bm.maxArgs < 0:
			want += " or more"
		case bm.maxArgs != bm.minArgs:
			want = plural(bm.minArgs) + " to " + plural(bm.maxArgs)
		}
		fc.errorf(pos, "%s takes %s, not %d", n.name, want, len(n.args))
	}
	args := make([]evalFn, len(n.args))
	for i, a := range n.args {
		if len(params) == 0 || (i >= len(params) && bm.maxArgs >= 0) {
			args[i], _ = fc.expr(a, nil) // too many, and said so
			continue
		}
		p := params[min(i, len(params)-1)]
		fn, t := fc.expr(a, p)
		if !assignable(t, p) {
			fc.errorf(a.position(), "argument %d of %s is a %s, not a %s", i+1, n.name, t, p)
		}
		args[i] = fn
	}
	if n.name == "join" && !xt.elem.primitive() && xt.elem.kind != tInvalid {
		fc.errorf(pos, "join wants an array of numbers, strings or bools, not of %s", xt.elem)
	}
	res := bm.result(xt)
	zero := Value{}
	if res.kind != tVoid {
		zero = zeroOf(res)
	}
	name := n.name
	return func(fr *frame) (Value, error) {
		recv, err := x(fr)
		if err != nil {
			return Value{}, err
		}
		if bm.writes && recv.kind == Null {
			return Value{}, m.errAt(pos, "%s on a null %s", name, xt)
		}
		vals, err := evalArgs(fr, args)
		if err != nil {
			return Value{}, err
		}
		out, err := bm.call(&builtinCall{vm: fr.vm, m: m, pos: pos}, recv, vals)
		if err != nil {
			return Value{}, err
		}
		if out.kind == 0 {
			return zero, nil
		}
		return out, nil
	}, res
}
