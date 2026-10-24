package language

// stdSig is the type of a standard library function in scripts. A nil parameter takes any number, string or
// bool; with variadic, the last parameter repeats. Scripts compile the calls (gen_builtins.go); expressions call
// the same functions in library.go.
type stdSig struct {
	params   []*Type
	variadic bool
	result   *Type
}

// stdSigs types the standard library for scripts.
var stdSigs = func() map[string]*stdSig {
	num1 := []*Type{typNumber}
	return map[string]*stdSig{
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
}()

// builtinNames can't be declared by modules: they are the standard library's.
var builtinNames = func() map[string]bool {
	out := map[string]bool{}
	for name := range stdSigs {
		out[name] = true
	}
	return out
}()

func (fc *funcCtx) callStd(n *callNode, sig *stdSig) (irExpr, *Type) {
	pos := n.pos
	if len(n.args) < len(sig.params) || (!sig.variadic && len(n.args) > len(sig.params)) {
		want := plural(len(sig.params))
		if sig.variadic {
			want += " or more"
		}
		fc.errorf(pos, "%s takes %s, not %d", n.name, want, len(n.args))
	}
	args := make([]irExpr, len(n.args))
	for i, a := range n.args {
		p := sig.params[min(i, len(sig.params)-1)]
		e, t := fc.expr(a, p)
		args[i] = e
		switch {
		case t.kind == tInvalid:
		case p == nil && !t.primitive():
			fc.errorf(a.position(), "argument %d of %s is a number, string or bool, not a %s", i+1, n.name, t)
		case p != nil && !assignable(t, p):
			fc.errorf(a.position(), "argument %d of %s is a %s, not a %s", i+1, n.name, t, p)
		}
	}
	return &irStd{irBase{sig.result}, n.name, args, pos}, sig.result
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
}

func elemOf(t *Type) *Type { return t.elem }
func same(t *Type) *Type   { return t }
func types(ts ...*Type) func(*Type) []*Type {
	return func(*Type) []*Type { return ts }
}
func result(t *Type) func(*Type) *Type { return func(*Type) *Type { return t } }

func elemParam(t *Type) []*Type { return []*Type{t.elem} }
func keyParam(t *Type) []*Type  { return []*Type{t.key} }

var builtinMethods = map[typeKind]map[string]*builtinMethod{
	tArray: {
		"push":     {minArgs: 1, maxArgs: -1, writes: true, params: elemParam, result: result(typVoid)},
		"pop":      {writes: true, params: types(), result: elemOf},
		"slice":    {maxArgs: 2, params: types(typNumber, typNumber), result: same},
		"indexOf":  {minArgs: 1, maxArgs: 1, params: elemParam, result: result(typNumber)},
		"includes": {minArgs: 1, maxArgs: 1, params: elemParam, result: result(typBool)},
		"join":     {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typString)},
	},
	tMap: {
		"get": {minArgs: 1, maxArgs: 1, params: keyParam, result: elemOf},
		"set": {minArgs: 2, maxArgs: 2, writes: true, params: func(t *Type) []*Type { return []*Type{t.key, t.elem} },
			result: result(typVoid)},
		"has":    {minArgs: 1, maxArgs: 1, params: keyParam, result: result(typBool)},
		"delete": {minArgs: 1, maxArgs: 1, writes: true, params: keyParam, result: result(typBool)},
		"keys":   {params: types(), result: func(t *Type) *Type { return arrayOf(t.key) }},
		"values": {params: types(), result: func(t *Type) *Type { return arrayOf(t.elem) }},
	},
	tString: {
		"slice":       {maxArgs: 2, params: types(typNumber, typNumber), result: result(typString)},
		"indexOf":     {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typNumber)},
		"includes":    {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typBool)},
		"startsWith":  {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typBool)},
		"endsWith":    {minArgs: 1, maxArgs: 1, params: types(typString), result: result(typBool)},
		"toUpperCase": {params: types(), result: result(typString)},
		"toLowerCase": {params: types(), result: result(typString)},
		"trim":        {params: types(), result: result(typString)},
		"split":       {minArgs: 1, maxArgs: 1, params: types(typString), result: result(arrayOf(typString))},
	},
}

// builtinMethod checks a call of a method of an array, string or map.
func (fc *funcCtx) builtinMethod(n *methodCallNode, x irExpr, xt *Type) (irExpr, *Type) {
	pos := n.pos
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
	args := make([]irExpr, len(n.args))
	for i, a := range n.args {
		if len(params) == 0 || (i >= len(params) && bm.maxArgs >= 0) {
			args[i], _ = fc.expr(a, nil) // too many, and said so
			continue
		}
		p := params[min(i, len(params)-1)]
		e, t := fc.expr(a, p)
		if !assignable(t, p) {
			fc.errorf(a.position(), "argument %d of %s is a %s, not a %s", i+1, n.name, t, p)
		}
		args[i] = e
	}
	if n.name == "join" && !xt.elem.primitive() && xt.elem.kind != tInvalid {
		fc.errorf(pos, "join wants an array of numbers, strings or bools, not of %s", xt.elem)
	}
	res := bm.result(xt)
	return &irBuiltin{irBase{res}, n.name, x, xt, args, pos}, res
}
