package language

import "github.com/krotovic/kiln/wasm"

// std compiles a call of the standard library.
func (fg *fgen) std(e *irStd) {
	f := fg.f
	arg := func(i int) { fg.expr(e.args[i]) }
	switch e.name {
	case "min", "max":
		op := byte(wasm.OpF64Min)
		if e.name == "max" {
			op = wasm.OpF64Max
		}
		arg(0)
		for i := 1; i < len(e.args); i++ {
			arg(i)
			f.Op(op)
		}
	case "floor":
		arg(0)
		f.Op(wasm.OpF64Flr)
	case "ceil":
		arg(0)
		f.Op(wasm.OpF64Ceil)
	case "abs":
		arg(0)
		f.Op(wasm.OpF64Abs)
	case "round":
		arg(0)
		fg.call("round")
	case "clamp":
		// math.Max(lo, math.Min(hi, x)), with x, lo and hi worked out in that order
		x, lo, hi := fg.tmp(wasm.F64), fg.tmp(wasm.F64), fg.tmp(wasm.F64)
		arg(0)
		f.LocalSet(x)
		arg(1)
		f.LocalSet(lo)
		arg(2)
		f.LocalSet(hi)
		f.LocalGet(lo).LocalGet(hi).LocalGet(x).Op(wasm.OpF64Min).Op(wasm.OpF64Max)
		fg.release(wasm.F64, x)
		fg.release(wasm.F64, lo)
		fg.release(wasm.F64, hi)
	case "num", "int":
		arg(0)
		switch e.args[0].typeOf().kind {
		case tBool:
			f.Op(wasm.OpF64ConvertI32U)
		case tString:
			fg.str(e.name)
			f.I32Const(fg.pos(e.pos))
			fg.call("num_parse")
		}
		if e.name == "int" {
			f.Op(wasm.OpF64Trnc)
		}
	case "str":
		arg(0)
		fg.toString(e.args[0].typeOf(), e.pos)
	case "bool":
		arg(0)
		switch e.args[0].typeOf().kind {
		case tNumber:
			f.F64Const(0).Op(wasm.OpF64Ne)
		case tString:
			f.I32Const(fg.pos(e.pos))
			fg.call("bool_parse")
		}
	case "len":
		arg(0)
		fg.call("str_runes")
		f.Op(wasm.OpF64ConvertI32U)
	case "concat":
		arg(0)
		fg.toString(e.args[0].typeOf(), e.pos)
		for i := 1; i < len(e.args); i++ {
			arg(i)
			fg.toString(e.args[i].typeOf(), e.pos)
			f.I32Const(fg.pos(e.pos))
			fg.call("str_concat")
		}
		if len(e.args) == 1 {
			// one argument is its own text form, but the result must still fit
			s := fg.tmp(wasm.I32)
			f.LocalTee(s).I32Load(12).I32Const(fg.pos(e.pos))
			fg.call("str_check")
			f.LocalGet(s)
			fg.release(wasm.I32, s)
		}
	default:
		f.Unreachable()
	}
}

// valueKind is how indexOf compares an element type and join writes it.
func elemKind(t *Type) int32 {
	switch t.kind {
	case tNumber:
		return 0
	case tBool:
		return 1
	}
	return 2
}

// builtin compiles a call of a method of an array, string or map.
func (fg *fgen) builtin(e *irBuiltin) {
	f := fg.f
	pos := fg.pos(e.pos)
	rt := e.recvT
	args := func() {
		for _, a := range e.args {
			fg.expr(a)
		}
	}
	// the slice arguments, both always passed, with how many were given
	sliceArgs := func() {
		for i := 0; i < 2; i++ {
			if i < len(e.args) {
				fg.expr(e.args[i])
			} else {
				f.F64Const(0)
			}
		}
		f.I32Const(int32(len(e.args)))
	}
	// writes refuse a null receiver, before the arguments are worked out
	recvChecked := func() uint32 {
		o := fg.tmp(wasm.I32)
		fg.expr(e.recv)
		f.LocalTee(o).Op(wasm.OpI32Eqz)
		f.If(wasm.Void)
		fg.str(e.name)
		fg.str(rt.String())
		f.I32Const(pos)
		fg.call("fail_null_write")
		f.End()
		return o
	}
	switch rt.kind {
	case tArray:
		elem := rt.elem
		switch e.name {
		case "push":
			o := recvChecked()
			var temps []uint32
			for _, a := range e.args {
				l := fg.tmp(wt(elem))
				fg.expr(a)
				f.LocalSet(l)
				temps = append(temps, l)
			}
			n, base := fg.tmp(wasm.I32), fg.tmp(wasm.I32)
			f.LocalGet(o).I32Load(12).LocalSet(n)
			f.LocalGet(o).LocalGet(n).I32Const(int32(len(temps))).Op(wasm.OpI32Add).I32Const(pos)
			fg.call("arr_reserve")
			f.LocalGet(o).I32Load(16).LocalGet(n).I32Const(3).Op(wasm.OpI32Shl).Op(wasm.OpI32Add).LocalSet(base)
			for i, l := range temps {
				f.LocalGet(base).LocalGet(l)
				store(f, elem, uint32(headerSize+8*i))
			}
			f.LocalGet(o).LocalGet(n).I32Const(int32(len(temps))).Op(wasm.OpI32Add).I32Store(12)
			for _, l := range temps {
				fg.release(wt(elem), l)
			}
			fg.release(wasm.I32, n)
			fg.release(wasm.I32, base)
			fg.release(wasm.I32, o)
		case "pop":
			o := recvChecked()
			f.LocalGet(o).I32Const(pos)
			fg.call("arr_pop")
			load(f, elem, 0)
			fg.release(wasm.I32, o)
		case "slice":
			fg.expr(e.recv)
			sliceArgs()
			f.I32Const(arrayTag(rt)).I32Const(pos)
			fg.call("arr_slice")
		case "indexOf", "includes":
			fg.expr(e.recv)
			args()
			f.I32Const(pos)
			switch elem.kind {
			case tNumber:
				fg.call("arr_find_f64")
			case tString:
				fg.call("arr_find_str")
			default:
				fg.call("arr_find_i32")
			}
			if e.name == "includes" {
				f.F64Const(0).Op(wasm.OpF64Ge)
			}
		case "join":
			fg.expr(e.recv)
			args()
			f.I32Const(elemKind(elem)).I32Const(pos)
			fg.call("arr_join")
		}
	case tMap:
		strKeys := rt.key.kind == tString
		find, add, del := "map_find_num", "map_add_num", "map_del_num"
		if strKeys {
			find, add, del = "map_find_str", "map_add_str", "map_del_str"
		}
		switch e.name {
		case "get":
			m, i := fg.tmp(wasm.I32), fg.tmp(wasm.I32)
			fg.expr(e.recv)
			f.LocalTee(m)
			args()
			fg.call(find)
			f.LocalTee(i).I32Const(0).Op(wasm.OpI32LtS)
			f.If(wasm.Result(wt(rt.elem)))
			fg.zero(rt.elem)
			f.Else()
			f.LocalGet(m).LocalGet(i)
			fg.call("map_val")
			load(f, rt.elem, 0)
			f.End()
			fg.release(wasm.I32, m)
			fg.release(wasm.I32, i)
		case "set":
			o := recvChecked()
			k, v := fg.tmp(wt(rt.key)), fg.tmp(wt(rt.elem))
			fg.expr(e.args[0])
			f.LocalSet(k)
			fg.expr(e.args[1])
			f.LocalSet(v)
			f.LocalGet(o).LocalGet(k).I32Const(pos)
			fg.call(add)
			f.LocalGet(v)
			store(f, rt.elem, 0)
			fg.release(wt(rt.key), k)
			fg.release(wt(rt.elem), v)
			fg.release(wasm.I32, o)
		case "has":
			fg.expr(e.recv)
			args()
			fg.call(find)
			f.I32Const(0).Op(wasm.OpI32GeS)
		case "delete":
			o := recvChecked()
			f.LocalGet(o)
			args()
			f.I32Const(pos)
			fg.call(del)
			fg.release(wasm.I32, o)
		case "keys", "values":
			fg.expr(e.recv)
			t := rt.key
			vals := int32(0)
			if e.name == "values" {
				t, vals = rt.elem, 1
			}
			f.I32Const(vals).I32Const(arrayTag(arrayOf(t))).I32Const(pos)
			fg.call("map_list")
		}
	case tString:
		fg.expr(e.recv)
		switch e.name {
		case "slice":
			sliceArgs()
			f.I32Const(pos)
			fg.call("str_slice")
		case "indexOf":
			args()
			fg.call("str_indexof")
		case "includes":
			args()
			fg.call("str_find")
			f.I32Const(0).Op(wasm.OpI32GeS)
		case "startsWith":
			args()
			fg.call("str_starts")
		case "endsWith":
			args()
			fg.call("str_ends")
		case "toUpperCase", "toLowerCase":
			lower := int32(0)
			if e.name == "toLowerCase" {
				lower = 1
			}
			f.I32Const(lower).I32Const(pos)
			fg.call("str_case")
		case "trim":
			f.I32Const(pos)
			fg.call("str_trim")
		case "split":
			args()
			f.I32Const(pos)
			fg.call("str_split")
		}
	}
}
