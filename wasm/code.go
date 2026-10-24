package wasm

import (
	"bytes"
	"math"
)

// Func is a defined function whose body is being written. Parameters are locals 0..n-1; Local adds more.
type Func struct {
	m     *Module
	Name  string
	Index uint32
	typ   uint32
	sig   FuncType

	locals []ValType
	code   bytes.Buffer
	depth  int // blocks open
}

// Label is a block, loop or if that branches can target.
type Label struct{ depth int }

// BlockType is a block's result: none, or one value type.
type BlockType struct {
	t   ValType
	has bool
}

// Void is the type of a block that leaves nothing.
var Void = BlockType{}

// Result is the type of a block that leaves one t.
func Result(t ValType) BlockType { return BlockType{t: t, has: true} }

// Sig is the function's signature.
func (f *Func) Sig() FuncType { return f.sig }

// Local adds a local of type t and returns its index.
func (f *Func) Local(t ValType) uint32 {
	f.locals = append(f.locals, t)
	return uint32(len(f.sig.Params) + len(f.locals) - 1)
}

// LocalType is the type of local i (parameters included).
func (f *Func) LocalType(i uint32) ValType {
	if int(i) < len(f.sig.Params) {
		return f.sig.Params[i]
	}
	return f.locals[int(i)-len(f.sig.Params)]
}

// Len is the size of the body written so far.
func (f *Func) Len() int { return f.code.Len() }

func (f *Func) op(b byte) *Func { f.code.WriteByte(b); return f }

func (f *Func) u32(op byte, x uint32) *Func {
	f.code.WriteByte(op)
	writeU32(&f.code, x)
	return f
}

func (f *Func) blockStart(op byte, bt BlockType) *Label {
	f.code.WriteByte(op)
	if bt.has {
		f.code.WriteByte(byte(bt.t))
	} else {
		f.code.WriteByte(0x40)
	}
	f.depth++
	return &Label{depth: f.depth}
}

// Block opens a block; a branch to it goes to its end.
func (f *Func) Block(bt BlockType) *Label { return f.blockStart(0x02, bt) }

// Loop opens a loop; a branch to it goes to its start.
func (f *Func) Loop(bt BlockType) *Label { return f.blockStart(0x03, bt) }

// If opens an if on the i32 on the stack.
func (f *Func) If(bt BlockType) *Label { return f.blockStart(0x04, bt) }

// Else starts an if's else branch.
func (f *Func) Else() *Func { return f.op(0x05) }

// End closes the innermost block, loop or if.
func (f *Func) End() *Func {
	f.depth--
	return f.op(opEnd)
}

func (f *Func) rel(l *Label) uint32 {
	if l.depth > f.depth || l.depth <= 0 {
		panic("wasm: branch to a block that isn't open")
	}
	return uint32(f.depth - l.depth)
}

// Br branches to l.
func (f *Func) Br(l *Label) *Func { return f.u32(0x0c, f.rel(l)) }

// BrIf branches to l when the i32 on the stack isn't 0.
func (f *Func) BrIf(l *Label) *Func { return f.u32(0x0d, f.rel(l)) }

// BrDepth and BrIfDepth branch by relative depth (the text format's numbers).
func (f *Func) BrDepth(d uint32) *Func   { return f.u32(0x0c, d) }
func (f *Func) BrIfDepth(d uint32) *Func { return f.u32(0x0d, d) }

func (f *Func) Return() *Func      { return f.op(0x0f) }
func (f *Func) Unreachable() *Func { return f.op(0x00) }
func (f *Func) Nop() *Func         { return f.op(0x01) }
func (f *Func) Drop() *Func        { return f.op(0x1a) }
func (f *Func) Select() *Func      { return f.op(0x1b) }

// Call calls function idx.
func (f *Func) Call(idx uint32) *Func { return f.u32(0x10, idx) }

// CallIndirect calls the table entry whose index is on the stack, with signature typ.
func (f *Func) CallIndirect(typ uint32) *Func {
	f.u32(0x11, typ)
	f.code.WriteByte(0x00)
	return f
}

func (f *Func) LocalGet(i uint32) *Func  { return f.u32(0x20, i) }
func (f *Func) LocalSet(i uint32) *Func  { return f.u32(0x21, i) }
func (f *Func) LocalTee(i uint32) *Func  { return f.u32(0x22, i) }
func (f *Func) GlobalGet(i uint32) *Func { return f.u32(0x23, i) }
func (f *Func) GlobalSet(i uint32) *Func { return f.u32(0x24, i) }

func (f *Func) I32Const(v int32) *Func {
	f.code.WriteByte(opI32Const)
	writeS64(&f.code, int64(v))
	return f
}

func (f *Func) I64Const(v int64) *Func {
	f.code.WriteByte(opI64Const)
	writeS64(&f.code, v)
	return f
}

func (f *Func) F64Const(v float64) *Func {
	f.code.WriteByte(opF64Const)
	writeF64(&f.code, v)
	return f
}

// Mem writes a load or store with its natural alignment and an offset.
func (f *Func) Mem(op byte, offset uint32) *Func {
	f.code.WriteByte(op)
	writeU32(&f.code, memAlign[op])
	writeU32(&f.code, offset)
	return f
}

func (f *Func) I32Load(off uint32) *Func   { return f.Mem(0x28, off) }
func (f *Func) I64Load(off uint32) *Func   { return f.Mem(0x29, off) }
func (f *Func) F64Load(off uint32) *Func   { return f.Mem(0x2b, off) }
func (f *Func) I32Load8U(off uint32) *Func { return f.Mem(0x2d, off) }
func (f *Func) I32Store(off uint32) *Func  { return f.Mem(0x36, off) }
func (f *Func) I64Store(off uint32) *Func  { return f.Mem(0x37, off) }
func (f *Func) F64Store(off uint32) *Func  { return f.Mem(0x39, off) }
func (f *Func) I32Store8(off uint32) *Func { return f.Mem(0x3a, off) }
func (f *Func) MemorySize() *Func          { f.code.Write([]byte{0x3f, 0x00}); return f }
func (f *Func) MemoryGrow() *Func          { f.code.Write([]byte{0x40, 0x00}); return f }
func (f *Func) MemoryCopy() *Func          { f.code.Write([]byte{0xfc, 0x0a, 0x00, 0x00}); return f }
func (f *Func) MemoryFill() *Func          { f.code.Write([]byte{0xfc, 0x0b, 0x00}); return f }
func (f *Func) Misc(sub uint32) *Func      { f.code.WriteByte(0xfc); writeU32(&f.code, sub); return f }
func (f *Func) Op(op byte) *Func           { return f.op(op) }
func (f *Func) Raw(b ...byte) *Func        { f.code.Write(b); return f }
func (f *Func) I32TruncSatF64S() *Func     { return f.Misc(2) }
func (f *Func) I64TruncSatF64S() *Func     { return f.Misc(6) }
func (f *Func) OpByName(name string) bool {
	op, ok := simpleOps[name]
	if ok {
		f.op(op)
	}
	return ok
}
func (f *Func) MiscByName(name string) bool {
	sub, ok := miscOps[name]
	if ok {
		f.Misc(sub)
	}
	return ok
}

// Numeric instructions the code generator uses by name. The full set the assembler knows is in simpleOps.
const (
	opEnd      = 0x0b
	opI32Const = 0x41
	opI64Const = 0x42
	opF64Const = 0x44

	OpI32Eqz  = 0x45
	OpI32Eq   = 0x46
	OpI32Ne   = 0x47
	OpI32LtS  = 0x48
	OpI32LtU  = 0x49
	OpI32GtS  = 0x4a
	OpI32GtU  = 0x4b
	OpI32LeS  = 0x4c
	OpI32LeU  = 0x4d
	OpI32GeS  = 0x4e
	OpI32GeU  = 0x4f
	OpI64Eqz  = 0x50
	OpI64Eq   = 0x51
	OpF64Eq   = 0x61
	OpF64Ne   = 0x62
	OpF64Lt   = 0x63
	OpF64Gt   = 0x64
	OpF64Le   = 0x65
	OpF64Ge   = 0x66
	OpI32Add  = 0x6a
	OpI32Sub  = 0x6b
	OpI32Mul  = 0x6c
	OpI32DivU = 0x6e
	OpI32And  = 0x71
	OpI32Or   = 0x72
	OpI32Xor  = 0x73
	OpI32Shl  = 0x74
	OpI32ShrU = 0x76
	OpI64RemS = 0x81
	OpF64Abs  = 0x99
	OpF64Neg  = 0x9a
	OpF64Ceil = 0x9b
	OpF64Flr  = 0x9c
	OpF64Trnc = 0x9d
	OpF64Add  = 0xa0
	OpF64Sub  = 0xa1
	OpF64Mul  = 0xa2
	OpF64Div  = 0xa3
	OpF64Min  = 0xa4
	OpF64Max  = 0xa5
	OpF64Copy = 0xa6

	OpI64TruncF64S   = 0xb0
	OpF64ConvertI32S = 0xb7
	OpF64ConvertI32U = 0xb8
	OpF64ConvertI64S = 0xb9
)

// simpleOps are the instructions without immediates, by their text names.
var simpleOps = map[string]byte{
	"unreachable": 0x00, "nop": 0x01, "return": 0x0f, "drop": 0x1a, "select": 0x1b,
	"i32.eqz": 0x45, "i32.eq": 0x46, "i32.ne": 0x47, "i32.lt_s": 0x48, "i32.lt_u": 0x49, "i32.gt_s": 0x4a,
	"i32.gt_u": 0x4b, "i32.le_s": 0x4c, "i32.le_u": 0x4d, "i32.ge_s": 0x4e, "i32.ge_u": 0x4f,
	"i64.eqz": 0x50, "i64.eq": 0x51, "i64.ne": 0x52, "i64.lt_s": 0x53, "i64.lt_u": 0x54, "i64.gt_s": 0x55,
	"i64.gt_u": 0x56, "i64.le_s": 0x57, "i64.le_u": 0x58, "i64.ge_s": 0x59, "i64.ge_u": 0x5a,
	"f64.eq": 0x61, "f64.ne": 0x62, "f64.lt": 0x63, "f64.gt": 0x64, "f64.le": 0x65, "f64.ge": 0x66,
	"i32.clz": 0x67, "i32.ctz": 0x68, "i32.popcnt": 0x69, "i32.add": 0x6a, "i32.sub": 0x6b, "i32.mul": 0x6c,
	"i32.div_s": 0x6d, "i32.div_u": 0x6e, "i32.rem_s": 0x6f, "i32.rem_u": 0x70, "i32.and": 0x71, "i32.or": 0x72,
	"i32.xor": 0x73, "i32.shl": 0x74, "i32.shr_s": 0x75, "i32.shr_u": 0x76, "i32.rotl": 0x77, "i32.rotr": 0x78,
	"i64.clz": 0x79, "i64.ctz": 0x7a, "i64.popcnt": 0x7b, "i64.add": 0x7c, "i64.sub": 0x7d, "i64.mul": 0x7e,
	"i64.div_s": 0x7f, "i64.div_u": 0x80, "i64.rem_s": 0x81, "i64.rem_u": 0x82, "i64.and": 0x83, "i64.or": 0x84,
	"i64.xor": 0x85, "i64.shl": 0x86, "i64.shr_s": 0x87, "i64.shr_u": 0x88, "i64.rotl": 0x89, "i64.rotr": 0x8a,
	"f64.abs": 0x99, "f64.neg": 0x9a, "f64.ceil": 0x9b, "f64.floor": 0x9c, "f64.trunc": 0x9d, "f64.nearest": 0x9e,
	"f64.sqrt": 0x9f, "f64.add": 0xa0, "f64.sub": 0xa1, "f64.mul": 0xa2, "f64.div": 0xa3, "f64.min": 0xa4,
	"f64.max": 0xa5, "f64.copysign": 0xa6,
	"i32.wrap_i64": 0xa7, "i32.trunc_f64_s": 0xaa, "i32.trunc_f64_u": 0xab, "i64.extend_i32_s": 0xac,
	"i64.extend_i32_u": 0xad, "i64.trunc_f64_s": 0xb0, "i64.trunc_f64_u": 0xb1, "f64.convert_i32_s": 0xb7,
	"f64.convert_i32_u": 0xb8, "f64.convert_i64_s": 0xb9, "f64.convert_i64_u": 0xba,
	"i64.reinterpret_f64": 0xbd, "f64.reinterpret_i64": 0xbf,
	"i32.extend8_s": 0xc0, "i32.extend16_s": 0xc1,
}

// miscOps are the 0xfc-prefixed instructions without immediates.
var miscOps = map[string]uint32{
	"i32.trunc_sat_f64_s": 2, "i32.trunc_sat_f64_u": 3, "i64.trunc_sat_f64_s": 6, "i64.trunc_sat_f64_u": 7,
}

// memOps are the loads and stores.
var memOps = map[string]byte{
	"i32.load": 0x28, "i64.load": 0x29, "f64.load": 0x2b, "i32.load8_s": 0x2c, "i32.load8_u": 0x2d,
	"i32.load16_s": 0x2e, "i32.load16_u": 0x2f, "i64.load8_u": 0x31, "i64.load32_u": 0x35,
	"i32.store": 0x36, "i64.store": 0x37, "f64.store": 0x39, "i32.store8": 0x3a, "i32.store16": 0x3b,
	"i64.store8": 0x3c, "i64.store32": 0x3e,
}

// memAlign is each load's and store's natural alignment, as a power of two.
var memAlign = map[byte]uint32{
	0x28: 2, 0x29: 3, 0x2b: 3, 0x2c: 0, 0x2d: 0, 0x2e: 1, 0x2f: 1, 0x31: 0, 0x35: 2,
	0x36: 2, 0x37: 3, 0x39: 3, 0x3a: 0, 0x3b: 1, 0x3c: 0, 0x3e: 2,
}

func writeU32(b *bytes.Buffer, x uint32) {
	for {
		c := byte(x & 0x7f)
		x >>= 7
		if x != 0 {
			b.WriteByte(c | 0x80)
			continue
		}
		b.WriteByte(c)
		return
	}
}

func writeS64(b *bytes.Buffer, x int64) {
	for {
		c := byte(x & 0x7f)
		x >>= 7
		if (x == 0 && c&0x40 == 0) || (x == -1 && c&0x40 != 0) {
			b.WriteByte(c)
			return
		}
		b.WriteByte(c | 0x80)
	}
}

func writeF64(b *bytes.Buffer, f float64) {
	bits := math.Float64bits(f)
	for i := 0; i < 8; i++ {
		b.WriteByte(byte(bits >> (8 * i)))
	}
}
