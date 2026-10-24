// Package wasm writes WebAssembly modules in the binary format: types, imported and defined functions, a function
// table, one memory, globals, exports, element and data segments, and custom sections. Function bodies are built
// instruction by instruction (code.go), or assembled from a subset of the text format (wat.go). It knows nothing
// about the language or the game.
package wasm

import (
	"bytes"
	"fmt"
	"strings"
)

// ValType is a value type.
type ValType byte

const (
	I32     ValType = 0x7f
	I64     ValType = 0x7e
	F32     ValType = 0x7d
	F64     ValType = 0x7c
	FuncRef ValType = 0x70
)

func (t ValType) String() string {
	switch t {
	case I32:
		return "i32"
	case I64:
		return "i64"
	case F32:
		return "f32"
	case F64:
		return "f64"
	case FuncRef:
		return "funcref"
	}
	return fmt.Sprintf("valtype(%#x)", byte(t))
}

// FuncType is a function's signature.
type FuncType struct {
	Params, Results []ValType
}

func (t FuncType) key() string {
	var b strings.Builder
	for _, p := range t.Params {
		b.WriteByte(byte(p))
	}
	b.WriteByte(0)
	for _, r := range t.Results {
		b.WriteByte(byte(r))
	}
	return b.String()
}

// Export kinds.
const (
	ExportFunc   byte = 0
	ExportTable  byte = 1
	ExportMemory byte = 2
	ExportGlobal byte = 3
)

type funcImport struct {
	module, name string
	typ          uint32
}

type global struct {
	typ     ValType
	mutable bool
	i       int64 // the initial value of an integer global
	f       float64
}

type export struct {
	name string
	kind byte
	idx  uint32
}

type data struct {
	offset uint32
	bytes  []byte
}

type custom struct {
	name string
	data []byte
}

// Module is a module being built. Imports come first: once a function is defined, nothing more can be imported,
// so function indices never move.
type Module struct {
	types   []FuncType
	typeIdx map[string]uint32
	imports []funcImport
	funcs   []*Func
	elems   []uint32 // the table's entries, from 0
	tableN  uint32
	hasMem  bool
	memMin  uint32
	memMax  uint32
	globals []global
	exports []export
	datas   []data
	customs []custom
}

// NewModule makes an empty module.
func NewModule() *Module {
	return &Module{typeIdx: map[string]uint32{}}
}

// Type returns the index of the signature, adding it once.
func (m *Module) Type(t FuncType) uint32 {
	k := t.key()
	if i, ok := m.typeIdx[k]; ok {
		return i
	}
	i := uint32(len(m.types))
	m.types = append(m.types, FuncType{Params: append([]ValType(nil), t.Params...), Results: append([]ValType(nil), t.Results...)})
	m.typeIdx[k] = i
	return i
}

// TypeOf is the signature at index i.
func (m *Module) TypeOf(i uint32) FuncType { return m.types[i] }

// ImportFunc imports a function and returns its index.
func (m *Module) ImportFunc(module, name string, t FuncType) uint32 {
	if len(m.funcs) > 0 {
		panic("wasm: import after a function was defined")
	}
	m.imports = append(m.imports, funcImport{module: module, name: name, typ: m.Type(t)})
	return uint32(len(m.imports) - 1)
}

// Func defines a function with signature t, whose body the caller writes. Its index is known at once, so
// functions may call each other in any order.
func (m *Module) Func(name string, t FuncType) *Func {
	f := &Func{m: m, Name: name, Index: uint32(len(m.imports) + len(m.funcs)), typ: m.Type(t), sig: t}
	m.funcs = append(m.funcs, f)
	return f
}

// Funcs are the defined functions, in index order.
func (m *Module) Funcs() []*Func { return m.funcs }

// Memory gives the module a memory of min to max pages of 64 KiB.
func (m *Module) Memory(min, max uint32) {
	m.hasMem, m.memMin, m.memMax = true, min, max
}

// GlobalI32 adds an i32 global and returns its index.
func (m *Module) GlobalI32(mutable bool, init int32) uint32 {
	m.globals = append(m.globals, global{typ: I32, mutable: mutable, i: int64(init)})
	return uint32(len(m.globals) - 1)
}

// GlobalI64 adds an i64 global and returns its index.
func (m *Module) GlobalI64(mutable bool, init int64) uint32 {
	m.globals = append(m.globals, global{typ: I64, mutable: mutable, i: init})
	return uint32(len(m.globals) - 1)
}

// GlobalF64 adds an f64 global and returns its index.
func (m *Module) GlobalF64(mutable bool, init float64) uint32 {
	m.globals = append(m.globals, global{typ: F64, mutable: mutable, f: init})
	return uint32(len(m.globals) - 1)
}

// SetGlobalI32 changes the initial value of i32 global i, for values only known once the module is built.
func (m *Module) SetGlobalI32(i uint32, v int32) { m.globals[i].i = int64(v) }

// GlobalType is the type of global i.
func (m *Module) GlobalType(i uint32) ValType { return m.globals[i].typ }

// Export exports a function, the memory or a global under name.
func (m *Module) Export(name string, kind byte, idx uint32) {
	m.exports = append(m.exports, export{name: name, kind: kind, idx: idx})
}

// Table sets the function table's entries, from index 0; entry i calls function elems[i].
func (m *Module) Table(elems []uint32) {
	m.elems = append([]uint32(nil), elems...)
	m.tableN = uint32(len(elems))
}

// Data puts bytes into the memory at offset when the module is instantiated.
func (m *Module) Data(offset uint32, b []byte) {
	m.datas = append(m.datas, data{offset: offset, bytes: append([]byte(nil), b...)})
}

// Custom adds a custom section.
func (m *Module) Custom(name string, b []byte) {
	m.customs = append(m.customs, custom{name: name, data: append([]byte(nil), b...)})
}

// Encode writes the module in the binary format.
func (m *Module) Encode() ([]byte, error) {
	for _, f := range m.funcs {
		if f.depth != 0 {
			return nil, fmt.Errorf("wasm: function %s has %d unclosed blocks", f.Name, f.depth)
		}
	}
	var out bytes.Buffer
	out.Write([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})
	section := func(id byte, body []byte) {
		out.WriteByte(id)
		writeU32(&out, uint32(len(body)))
		out.Write(body)
	}
	var b bytes.Buffer

	writeU32(&b, uint32(len(m.types)))
	for _, t := range m.types {
		b.WriteByte(0x60)
		writeU32(&b, uint32(len(t.Params)))
		for _, p := range t.Params {
			b.WriteByte(byte(p))
		}
		writeU32(&b, uint32(len(t.Results)))
		for _, r := range t.Results {
			b.WriteByte(byte(r))
		}
	}
	section(1, b.Bytes())

	if len(m.imports) > 0 {
		b.Reset()
		writeU32(&b, uint32(len(m.imports)))
		for _, im := range m.imports {
			writeName(&b, im.module)
			writeName(&b, im.name)
			b.WriteByte(0x00)
			writeU32(&b, im.typ)
		}
		section(2, b.Bytes())
	}

	b.Reset()
	writeU32(&b, uint32(len(m.funcs)))
	for _, f := range m.funcs {
		writeU32(&b, f.typ)
	}
	section(3, b.Bytes())

	if m.tableN > 0 {
		b.Reset()
		b.WriteByte(1)
		b.WriteByte(byte(FuncRef))
		b.WriteByte(0x01)
		writeU32(&b, m.tableN)
		writeU32(&b, m.tableN)
		section(4, b.Bytes())
	}

	if m.hasMem {
		b.Reset()
		b.WriteByte(1)
		b.WriteByte(0x01)
		writeU32(&b, m.memMin)
		writeU32(&b, m.memMax)
		section(5, b.Bytes())
	}

	if len(m.globals) > 0 {
		b.Reset()
		writeU32(&b, uint32(len(m.globals)))
		for _, g := range m.globals {
			b.WriteByte(byte(g.typ))
			if g.mutable {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
			switch g.typ {
			case I32:
				b.WriteByte(opI32Const)
				writeS64(&b, g.i)
			case I64:
				b.WriteByte(opI64Const)
				writeS64(&b, g.i)
			case F64:
				b.WriteByte(opF64Const)
				writeF64(&b, g.f)
			}
			b.WriteByte(opEnd)
		}
		section(6, b.Bytes())
	}

	if len(m.exports) > 0 {
		b.Reset()
		writeU32(&b, uint32(len(m.exports)))
		for _, e := range m.exports {
			writeName(&b, e.name)
			b.WriteByte(e.kind)
			writeU32(&b, e.idx)
		}
		section(7, b.Bytes())
	}

	if m.tableN > 0 {
		b.Reset()
		writeU32(&b, 1)
		b.WriteByte(0x00)
		b.WriteByte(opI32Const)
		writeS64(&b, 0)
		b.WriteByte(opEnd)
		writeU32(&b, uint32(len(m.elems)))
		for _, e := range m.elems {
			writeU32(&b, e)
		}
		section(9, b.Bytes())
	}

	b.Reset()
	writeU32(&b, uint32(len(m.funcs)))
	var body bytes.Buffer
	for _, f := range m.funcs {
		body.Reset()
		// locals, run-length encoded by type
		type run struct {
			n uint32
			t ValType
		}
		var runs []run
		for _, l := range f.locals {
			if len(runs) > 0 && runs[len(runs)-1].t == l {
				runs[len(runs)-1].n++
			} else {
				runs = append(runs, run{1, l})
			}
		}
		writeU32(&body, uint32(len(runs)))
		for _, r := range runs {
			writeU32(&body, r.n)
			body.WriteByte(byte(r.t))
		}
		body.Write(f.code.Bytes())
		body.WriteByte(opEnd)
		writeU32(&b, uint32(body.Len()))
		b.Write(body.Bytes())
	}
	section(10, b.Bytes())

	if len(m.datas) > 0 {
		b.Reset()
		writeU32(&b, uint32(len(m.datas)))
		for _, d := range m.datas {
			b.WriteByte(0x00)
			b.WriteByte(opI32Const)
			writeS64(&b, int64(int32(d.offset)))
			b.WriteByte(opEnd)
			writeU32(&b, uint32(len(d.bytes)))
			b.Write(d.bytes)
		}
		section(11, b.Bytes())
	}

	for _, c := range m.customs {
		b.Reset()
		writeName(&b, c.name)
		b.Write(c.data)
		section(0, b.Bytes())
	}
	return out.Bytes(), nil
}

// CustomSection finds a custom section's contents in an encoded module.
func CustomSection(module []byte, name string) ([]byte, bool) {
	if len(module) < 8 || !bytes.Equal(module[:4], []byte{0x00, 0x61, 0x73, 0x6d}) {
		return nil, false
	}
	r := &reader{b: module, i: 8}
	for r.i < len(r.b) {
		id := r.b[r.i]
		r.i++
		size, ok := r.u32()
		if !ok || r.i+int(size) > len(r.b) {
			return nil, false
		}
		end := r.i + int(size)
		if id == 0 {
			n, ok := r.u32()
			if ok && r.i+int(n) <= end && string(r.b[r.i:r.i+int(n)]) == name {
				return r.b[r.i+int(n) : end], true
			}
		}
		r.i = end
	}
	return nil, false
}

type reader struct {
	b []byte
	i int
}

func (r *reader) u32() (uint32, bool) {
	var x uint32
	var shift uint
	for {
		if r.i >= len(r.b) || shift > 28 {
			return 0, false
		}
		c := r.b[r.i]
		r.i++
		x |= uint32(c&0x7f) << shift
		if c&0x80 == 0 {
			return x, true
		}
		shift += 7
	}
}

func writeName(b *bytes.Buffer, s string) {
	writeU32(b, uint32(len(s)))
	b.WriteString(s)
}
