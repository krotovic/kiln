package wasm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The assembler reads a subset of the text format, enough to write runtime functions by hand:
//
//	(func $name (param $a i32) (param $b f64) (result i32) (local $i i32)
//	  (local.set $i (i32.add (local.get $a) (i32.const 1)))
//	  (if (i32.eqz (local.get $i)) (then (return (i32.const 0))))
//	  (block $done (loop $next ... (br_if $done ...) (br $next)))
//	  (local.get $i))
//
// Instructions are written folded: an instruction's operands come after its immediates, inside its parentheses,
// and are emitted first. An instruction without immediates or operands may also be written bare (return, drop).
// Names of functions, globals and constants are the Resolver's, so a host can generate functions on demand and
// give the assembler its symbols; (i32.const $name) reads a constant.

// Resolver gives the names a function's body uses besides its own locals and labels.
type Resolver interface {
	Func(name string) (uint32, bool)
	Global(name string) (uint32, bool)
	Const(name string) (int64, bool)
}

// WatFunc is a function read from the text format, not yet emitted.
type WatFunc struct {
	Name    string
	Params  []ValType
	Results []ValType
	locals  []ValType
	names   map[string]uint32
	body    []*sexp
}

// Type is the function's signature.
func (w *WatFunc) Type() FuncType { return FuncType{Params: w.Params, Results: w.Results} }

// Calls lists the functions the body calls, so a host can generate only what is used.
func (w *WatFunc) Calls() []string {
	var out []string
	var walk func(s *sexp)
	walk = func(s *sexp) {
		if s.list == nil {
			return
		}
		if len(s.list) >= 2 && s.list[0].atom == "call" && strings.HasPrefix(s.list[1].atom, "$") {
			out = append(out, s.list[1].atom[1:])
		}
		for _, c := range s.list {
			walk(c)
		}
	}
	for _, s := range w.body {
		walk(s)
	}
	return out
}

type sexp struct {
	atom  string
	str   bool
	list  []*sexp
	line  int
	isLst bool
}

// ParseWat reads functions in the text format, by name (without the $).
func ParseWat(src string) (map[string]*WatFunc, error) {
	forms, err := parseSexps(src)
	if err != nil {
		return nil, err
	}
	out := map[string]*WatFunc{}
	for _, f := range forms {
		if !f.isLst || len(f.list) < 2 || f.list[0].atom != "func" || !strings.HasPrefix(f.list[1].atom, "$") {
			return nil, fmt.Errorf("line %d: want (func $name …)", f.line)
		}
		w := &WatFunc{Name: f.list[1].atom[1:], names: map[string]uint32{}}
		var localNames []string
		rest := f.list[2:]
		for len(rest) > 0 && rest[0].isLst && len(rest[0].list) > 0 {
			head := rest[0].list[0].atom
			if head != "param" && head != "result" && head != "local" {
				break
			}
			items := rest[0].list[1:]
			name := ""
			if len(items) > 0 && strings.HasPrefix(items[0].atom, "$") {
				name, items = items[0].atom, items[1:]
			}
			for _, it := range items {
				t, ok := valTypes[it.atom]
				if !ok {
					return nil, fmt.Errorf("line %d: unknown type %q", it.line, it.atom)
				}
				switch head {
				case "param":
					if name != "" {
						w.names[name] = uint32(len(w.Params))
					}
					w.Params = append(w.Params, t)
				case "result":
					w.Results = append(w.Results, t)
				case "local":
					w.locals = append(w.locals, t)
					localNames = append(localNames, name)
				}
			}
			rest = rest[1:]
		}
		for i, n := range localNames {
			if n != "" {
				w.names[n] = uint32(len(w.Params) + i)
			}
		}
		w.body = rest
		if _, dup := out[w.Name]; dup {
			return nil, fmt.Errorf("line %d: $%s is defined twice", f.line, w.Name)
		}
		out[w.Name] = w
	}
	return out, nil
}

var valTypes = map[string]ValType{"i32": I32, "i64": I64, "f32": F32, "f64": F64}

// Emit writes the function's locals and body into f, which must have the function's signature.
func (w *WatFunc) Emit(f *Func, r Resolver) error {
	for _, t := range w.locals {
		f.Local(t)
	}
	a := &assembler{w: w, f: f, r: r}
	for _, s := range w.body {
		if err := a.instr(s); err != nil {
			return fmt.Errorf("$%s: %w", w.Name, err)
		}
	}
	return nil
}

type assembler struct {
	w      *WatFunc
	f      *Func
	r      Resolver
	labels []string // innermost last
}

func (a *assembler) errorf(s *sexp, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", s.line, fmt.Sprintf(format, args...))
}

func (a *assembler) blockType(items []*sexp) (BlockType, string, []*sexp) {
	label := ""
	if len(items) > 0 && strings.HasPrefix(items[0].atom, "$") {
		label, items = items[0].atom, items[1:]
	}
	bt := Void
	if len(items) > 0 && items[0].isLst && len(items[0].list) == 2 && items[0].list[0].atom == "result" {
		if t, ok := valTypes[items[0].list[1].atom]; ok {
			bt = Result(t)
			items = items[1:]
		}
	}
	return bt, label, items
}

func (a *assembler) instr(s *sexp) error {
	if !s.isLst {
		if op, ok := simpleOps[s.atom]; ok {
			a.f.op(op)
			return nil
		}
		return a.errorf(s, "unknown instruction %q (write instructions with immediates folded)", s.atom)
	}
	if len(s.list) == 0 {
		return a.errorf(s, "empty instruction")
	}
	head := s.list[0].atom
	switch head {
	case "block", "loop":
		bt, label, body := a.blockType(s.list[1:])
		if head == "block" {
			a.f.Block(bt)
		} else {
			a.f.Loop(bt)
		}
		a.labels = append(a.labels, label)
		for _, b := range body {
			if err := a.instr(b); err != nil {
				return err
			}
		}
		a.labels = a.labels[:len(a.labels)-1]
		a.f.End()
		return nil
	case "if":
		bt, label, rest := a.blockType(s.list[1:])
		var then, els *sexp
		var cond []*sexp
		for _, r := range rest {
			switch {
			case r.isLst && len(r.list) > 0 && r.list[0].atom == "then":
				then = r
			case r.isLst && len(r.list) > 0 && r.list[0].atom == "else":
				els = r
			default:
				cond = append(cond, r)
			}
		}
		if then == nil {
			return a.errorf(s, "if without (then …)")
		}
		for _, c := range cond {
			if err := a.instr(c); err != nil {
				return err
			}
		}
		a.f.If(bt)
		a.labels = append(a.labels, label)
		for _, b := range then.list[1:] {
			if err := a.instr(b); err != nil {
				return err
			}
		}
		if els != nil {
			a.f.Else()
			for _, b := range els.list[1:] {
				if err := a.instr(b); err != nil {
					return err
				}
			}
		}
		a.labels = a.labels[:len(a.labels)-1]
		a.f.End()
		return nil
	}
	// a plain instruction: immediates (atoms), then operands (lists)
	var imms []*sexp
	i := 1
	for ; i < len(s.list) && !s.list[i].isLst; i++ {
		imms = append(imms, s.list[i])
	}
	for _, op := range s.list[i:] {
		if !op.isLst {
			return a.errorf(op, "operand %q after operands", op.atom)
		}
		if err := a.instr(op); err != nil {
			return err
		}
	}
	return a.plain(s, head, imms)
}

func (a *assembler) plain(s *sexp, head string, imms []*sexp) error {
	need := func(n int) error {
		if len(imms) != n {
			return a.errorf(s, "%s takes %d immediates, not %d", head, n, len(imms))
		}
		return nil
	}
	switch head {
	case "local.get", "local.set", "local.tee":
		if err := need(1); err != nil {
			return err
		}
		idx, ok := a.w.names[imms[0].atom]
		if !ok {
			n, err := strconv.ParseUint(imms[0].atom, 10, 32)
			if err != nil {
				return a.errorf(s, "unknown local %s", imms[0].atom)
			}
			idx = uint32(n)
		}
		switch head {
		case "local.get":
			a.f.LocalGet(idx)
		case "local.set":
			a.f.LocalSet(idx)
		default:
			a.f.LocalTee(idx)
		}
		return nil
	case "global.get", "global.set":
		if err := need(1); err != nil {
			return err
		}
		idx, ok := a.r.Global(strings.TrimPrefix(imms[0].atom, "$"))
		if !ok {
			return a.errorf(s, "unknown global %s", imms[0].atom)
		}
		if head == "global.get" {
			a.f.GlobalGet(idx)
		} else {
			a.f.GlobalSet(idx)
		}
		return nil
	case "call":
		if err := need(1); err != nil {
			return err
		}
		idx, ok := a.r.Func(strings.TrimPrefix(imms[0].atom, "$"))
		if !ok {
			return a.errorf(s, "unknown function %s", imms[0].atom)
		}
		a.f.Call(idx)
		return nil
	case "br", "br_if":
		if err := need(1); err != nil {
			return err
		}
		d, err := a.depth(imms[0])
		if err != nil {
			return err
		}
		if head == "br" {
			a.f.BrDepth(d)
		} else {
			a.f.BrIfDepth(d)
		}
		return nil
	case "i32.const", "i64.const":
		if err := need(1); err != nil {
			return err
		}
		v, err := a.intImm(imms[0])
		if err != nil {
			return err
		}
		if head == "i32.const" {
			a.f.I32Const(int32(v))
		} else {
			a.f.I64Const(v)
		}
		return nil
	case "f64.const":
		if err := need(1); err != nil {
			return err
		}
		v, err := floatImm(imms[0].atom)
		if err != nil {
			return a.errorf(s, "%v", err)
		}
		a.f.F64Const(v)
		return nil
	case "memory.size":
		a.f.MemorySize()
		return need(0)
	case "memory.grow":
		a.f.MemoryGrow()
		return need(0)
	case "memory.copy":
		a.f.MemoryCopy()
		return need(0)
	case "memory.fill":
		a.f.MemoryFill()
		return need(0)
	}
	if op, ok := memOps[head]; ok {
		var off uint32
		for _, im := range imms {
			switch {
			case strings.HasPrefix(im.atom, "offset="):
				n, err := strconv.ParseUint(im.atom[len("offset="):], 0, 32)
				if err != nil {
					return a.errorf(s, "bad offset %s", im.atom)
				}
				off = uint32(n)
			case strings.HasPrefix(im.atom, "align="):
			default:
				return a.errorf(s, "unknown immediate %s", im.atom)
			}
		}
		a.f.Mem(op, off)
		return nil
	}
	if op, ok := simpleOps[head]; ok {
		a.f.op(op)
		return need(0)
	}
	if sub, ok := miscOps[head]; ok {
		a.f.Misc(sub)
		return need(0)
	}
	return a.errorf(s, "unknown instruction %s", head)
}

func (a *assembler) depth(s *sexp) (uint32, error) {
	if strings.HasPrefix(s.atom, "$") {
		for i := len(a.labels) - 1; i >= 0; i-- {
			if a.labels[i] == s.atom {
				return uint32(len(a.labels) - 1 - i), nil
			}
		}
		return 0, a.errorf(s, "unknown label %s", s.atom)
	}
	n, err := strconv.ParseUint(s.atom, 10, 32)
	if err != nil {
		return 0, a.errorf(s, "bad label %s", s.atom)
	}
	return uint32(n), nil
}

func (a *assembler) intImm(s *sexp) (int64, error) {
	if strings.HasPrefix(s.atom, "$") {
		v, ok := a.r.Const(s.atom[1:])
		if !ok {
			return 0, a.errorf(s, "unknown constant %s", s.atom)
		}
		return v, nil
	}
	text := strings.ReplaceAll(s.atom, "_", "")
	neg := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(strings.TrimPrefix(text, "-"), "+")
	u, err := strconv.ParseUint(text, 0, 64)
	if err != nil {
		return 0, a.errorf(s, "bad integer %s", s.atom)
	}
	if neg {
		return -int64(u), nil
	}
	return int64(u), nil
}

func floatImm(text string) (float64, error) {
	switch text {
	case "inf", "+inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
}

func parseSexps(src string) ([]*sexp, error) {
	p := &sexpParser{src: src, line: 1}
	var out []*sexp
	for {
		p.skip()
		if p.i >= len(p.src) {
			return out, nil
		}
		s, err := p.parse()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
}

type sexpParser struct {
	src  string
	i    int
	line int
}

func (p *sexpParser) skip() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '\n':
			p.line++
			p.i++
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case strings.HasPrefix(p.src[p.i:], ";;"):
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
		case strings.HasPrefix(p.src[p.i:], "(;"):
			end := strings.Index(p.src[p.i:], ";)")
			if end < 0 {
				p.i = len(p.src)
				return
			}
			p.line += strings.Count(p.src[p.i:p.i+end], "\n")
			p.i += end + 2
		default:
			return
		}
	}
}

func (p *sexpParser) parse() (*sexp, error) {
	p.skip()
	if p.i >= len(p.src) {
		return nil, fmt.Errorf("line %d: unexpected end", p.line)
	}
	line := p.line
	switch c := p.src[p.i]; c {
	case '(':
		p.i++
		s := &sexp{isLst: true, line: line}
		for {
			p.skip()
			if p.i >= len(p.src) {
				return nil, fmt.Errorf("line %d: unclosed (", line)
			}
			if p.src[p.i] == ')' {
				p.i++
				return s, nil
			}
			child, err := p.parse()
			if err != nil {
				return nil, err
			}
			s.list = append(s.list, child)
		}
	case ')':
		return nil, fmt.Errorf("line %d: unexpected )", line)
	case '"':
		end := strings.IndexByte(p.src[p.i+1:], '"')
		if end < 0 {
			return nil, fmt.Errorf("line %d: unclosed string", line)
		}
		s := &sexp{atom: p.src[p.i+1 : p.i+1+end], str: true, line: line}
		p.i += end + 2
		return s, nil
	}
	start := p.i
	for p.i < len(p.src) && !strings.ContainsRune(" \t\r\n()\";", rune(p.src[p.i])) {
		p.i++
	}
	return &sexp{atom: p.src[start:p.i], line: line}, nil
}
