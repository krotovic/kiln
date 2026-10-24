package wasm

import (
	"bytes"
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestLEB128(t *testing.T) {
	cases := []struct {
		signed bool
		v      int64
		want   []byte
	}{
		{false, 0, []byte{0}},
		{false, 127, []byte{0x7f}},
		{false, 128, []byte{0x80, 0x01}},
		{false, 624485, []byte{0xe5, 0x8e, 0x26}},
		{true, -1, []byte{0x7f}},
		{true, 63, []byte{0x3f}},
		{true, 64, []byte{0xc0, 0x00}},
		{true, -64, []byte{0x40}},
		{true, -65, []byte{0xbf, 0x7f}},
		{true, -123456, []byte{0xc0, 0xbb, 0x78}},
	}
	for _, c := range cases {
		var b bytes.Buffer
		if c.signed {
			writeS64(&b, c.v)
		} else {
			writeU32(&b, uint32(c.v))
		}
		if !bytes.Equal(b.Bytes(), c.want) {
			t.Errorf("leb(%v, %d) = %x, want %x", c.signed, c.v, b.Bytes(), c.want)
		}
	}
}

type resolver struct {
	funcs   map[string]uint32
	globals map[string]uint32
	consts  map[string]int64
}

func (r resolver) Func(n string) (uint32, bool)   { i, ok := r.funcs[n]; return i, ok }
func (r resolver) Global(n string) (uint32, bool) { i, ok := r.globals[n]; return i, ok }
func (r resolver) Const(n string) (int64, bool)   { v, ok := r.consts[n]; return v, ok }

const testWat = `
;; sums 1..n with a loop, through a global
(func $sum (param $n i32) (result i32) (local $i i32)
  (global.set $acc (i32.const 0))
  (block $done
    (loop $next
      (br_if $done (i32.gt_s (local.get $i) (local.get $n)))
      (global.set $acc (i32.add (global.get $acc) (local.get $i)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
  (global.get $acc))

(func $pick (param $c i32) (result f64)
  (if (result f64) (local.get $c)
    (then (f64.const 1.5))
    (else (f64.neg (f64.const 2.5)))))

;; the host doubles a number by calling back into $twice
(func $viaHost (param $x i32) (result i32)
  (call $host (local.get $x)))

(func $twice (param $x i32) (result i32)
  (i32.mul (local.get $x) (i32.const $TWO)))

(func $bytes (result i32)
  (i32.store8 offset=101 (i32.const 0) (i32.const 66))
  (i32.add (i32.load8_u offset=100 (i32.const 0)) (i32.load8_u offset=101 (i32.const 0))))
`

func TestModuleRunsInWazero(t *testing.T) {
	m := NewModule()
	host := m.ImportFunc("env", "host", FuncType{Params: []ValType{I32}, Results: []ValType{I32}})
	funcs, err := ParseWat(testWat)
	if err != nil {
		t.Fatal(err)
	}
	r := resolver{funcs: map[string]uint32{"host": host}, globals: map[string]uint32{}, consts: map[string]int64{"TWO": 2}}
	r.globals["acc"] = m.GlobalI32(true, 0)
	order := []string{"sum", "pick", "viaHost", "twice", "bytes"}
	defs := map[string]*Func{}
	for _, name := range order {
		defs[name] = m.Func(name, funcs[name].Type())
		r.funcs[name] = defs[name].Index
	}
	for _, name := range order {
		if err := funcs[name].Emit(defs[name], r); err != nil {
			t.Fatal(err)
		}
		m.Export(name, ExportFunc, defs[name].Index)
	}
	// a function written with the builder, called through the table
	sig := m.Type(FuncType{Results: []ValType{I32}})
	seven := m.Func("seven", FuncType{Results: []ValType{I32}})
	seven.I32Const(7)
	viaTable := m.Func("viaTable", FuncType{Results: []ValType{I32}})
	viaTable.I32Const(0).CallIndirect(sig)
	m.Export("viaTable", ExportFunc, viaTable.Index)
	m.Table([]uint32{seven.Index})
	m.Memory(1, 2)
	m.Export("memory", ExportMemory, 0)
	m.Data(100, []byte{65})
	m.Custom("meta", []byte(`{"ok":true}`))
	bin, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := CustomSection(bin, "meta"); !ok || string(got) != `{"ok":true}` {
		t.Fatalf("CustomSection = %q, %v", got, ok)
	}

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	_, err = rt.NewHostModuleBuilder("env").NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			out, err := mod.ExportedFunction("twice").Call(ctx, stack[0])
			if err != nil {
				panic(err)
			}
			stack[0] = out[0]
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("host").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := rt.Instantiate(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args ...uint64) uint64 {
		out, err := mod.ExportedFunction(name).Call(ctx, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out[0]
	}
	if got := call("sum", 10); got != 55 {
		t.Errorf("sum(10) = %d", got)
	}
	if got := api.DecodeF64(call("pick", 0)); got != -2.5 {
		t.Errorf("pick(0) = %v", got)
	}
	if got := call("viaHost", 21); got != 42 {
		t.Errorf("viaHost(21) = %d", got)
	}
	if got := call("viaTable"); got != 7 {
		t.Errorf("viaTable = %d", got)
	}
	if got := call("bytes"); got != 65+66 {
		t.Errorf("bytes = %d", got)
	}
}

func TestAssemblerErrors(t *testing.T) {
	for _, src := range []string{
		"(func $f (local.get $nope))",
		"(func $f (br $nowhere))",
		"(func $f (i32.frobnicate))",
		"(func $f (call $missing))",
		"(func $f (param $x i33))",
	} {
		funcs, err := ParseWat(src)
		if err != nil {
			continue
		}
		m := NewModule()
		f := m.Func("f", funcs["f"].Type())
		if err := funcs["f"].Emit(f, resolver{}); err == nil {
			t.Errorf("%s assembled", src)
		}
	}
	if _, err := ParseWat("(func $f) (func $f)"); err == nil {
		t.Error("a function defined twice was accepted")
	}
}
