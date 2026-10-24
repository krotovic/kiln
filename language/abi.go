package language

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The interface between a compiled script and its host (Go's in host.go, the browser's in
// game/src/engine/language): the memory layout, the imports and exports, the error codes with their messages,
// and the metadata the module carries. A change to any of it is a new abiVersion.

const abiVersion = 1

// metaSection is the custom section that holds a module's metadata.
const metaSection = "alo.script"

// Memory layout: address 0 is null; the scratch slots carry arguments and results between the script and the
// host; static data (string literals, tables) follows, then the heap.
const (
	scratchBase  = 16
	scratchSlots = 64
	staticBase   = scratchBase + scratchSlots*8
	headerSize   = 16
	upperLower   = 0x110000 // unicode.UpperLower
)

// Block tags.
const (
	tagString   = 1
	tagArrVals  = 2 // an array of numbers or bools
	tagArrRefs  = 3 // an array of strings, arrays, maps or objects
	tagStore    = 4
	tagMap      = 5
	tagHandles  = 6
	tagNatives  = 7
	tagClass    = 16 // + the class's index
	mapStrKeys  = 1
	mapRefVals  = 2
	pageSize    = 65536
	maxNatParam = scratchSlots - 1
)

// The imports, all from the module "alo".
const (
	impFail     = "fail"      // (code, pos): a run-time error, with its arguments in the scratch slots
	impNative   = "native"    // (id, pos) -> status: a native, with this and its arguments in the scratch slots, its result in slot 0
	impFmtNum   = "fmt_num"   // (f64) -> string: a number in its shortest form, for those the script doesn't write itself
	impParseNum = "parse_num" // (ptr, len) -> ok: ParseFloat's number in slot 0
	impFmod     = "fmod"      // (f64, f64) -> f64: math.Mod
	impRelease  = "release"   // (nh): the collector dropped the native object of the host's handle
)

// Error codes, with their messages. A message's {i} is its argument from scratch slot i, by kind: i an
// integer, n a number in its text form, s a string, q a string quoted as Go's %q does.
const (
	errInternal = iota
	errSteps
	errDepth
	errMemory
	errLength
	errString
	errDivZero
	errTooLarge
	errIndex
	errIndexSet
	errNull
	errNullWrite
	errPopEmpty
	errSliceWhole
	errNotNumber
	errNotBool
	errHostField
)

var errorMessages = []string{
	errInternal:   "{0s}",
	errSteps:      "ran more than {0i} steps",
	errDepth:      "calls nested deeper than {0i}",
	errMemory:     "used more than {0i} bytes of memory",
	errLength:     "an {0s} longer than {1i}",
	errString:     "a string longer than {0i} characters",
	errDivZero:    "division by zero",
	errTooLarge:   "{0s} gives a number too large",
	errIndex:      "index {0n} is out of range (length {1i})",
	errIndexSet:   "index {0i} is out of range (length {1i})",
	errNull:       "{0s} of null",
	errNullWrite:  "{0s} on a null {1s}",
	errPopEmpty:   "pop on an empty array",
	errSliceWhole: "slice wants whole numbers, not {0n}",
	errNotNumber:  "{0s}: {1q} is not a number",
	errNotBool:    "{0s}: {1q} is not a bool",
	errHostField:  "{0s} is the host's, and can't be set",
}

// scriptMeta is a module's metadata: what a host needs to call into it and to report its errors.
type scriptMeta struct {
	ABI     int          `json:"abi"`
	Entry   string       `json:"entry"`
	Modules []string     `json:"modules"`
	Limits  ScriptLimits `json:"limits"`
	// Messages are the error messages by code
	Messages []string `json:"messages"`
	// Positions are where errors can happen, three numbers each: the module's index, the line and the column
	Positions []int32      `json:"positions"`
	Classes   []classMeta  `json:"classes"`
	Natives   []nativeMeta `json:"natives"`
	Hosts     []hostMeta   `json:"hosts"`
	// Funcs are the entry's exported functions, by name
	Funcs map[string]*funcMeta `json:"funcs"`
	// Exports are the entry's exported classes ("default" for its default export) by index
	Exports map[string]int `json:"exports"`
}

type hostMeta struct {
	Name string `json:"name"`
	Hash string `json:"hash"` // of its source, so a host with another definition file is refused
}

// typeMeta is a type as the host sees it. K is n (number), s (string), b (bool), v (void), a (array of E),
// m (map of Key to E), c (an object of class C), or i (an object of an interface).
type typeMeta struct {
	K    string    `json:"k"`
	E    *typeMeta `json:"e,omitempty"`
	Key  *typeMeta `json:"key,omitempty"`
	C    int       `json:"c,omitempty"`
	Name string    `json:"name"` // as messages write it
}

type classMeta struct {
	Name   string `json:"name"`
	Module string `json:"module"`
	Pos    int32  `json:"pos"`
	Native bool   `json:"native,omitempty"`
	// Size is an object's size in bytes, header included
	Size    int                    `json:"size"`
	Fields  []fieldMeta            `json:"fields"`
	Methods map[string]*methodMeta `json:"methods"`
	// Ctor is the constructor's parameters
	Ctor []typeMeta `json:"ctor"`
}

type fieldMeta struct {
	Name     string   `json:"name"`
	Type     typeMeta `json:"type"`
	Canon    string   `json:"canon"`
	Offset   int      `json:"offset"`
	Private  bool     `json:"private,omitempty"`
	Readonly bool     `json:"readonly,omitempty"`
}

type methodMeta struct {
	Params  []typeMeta `json:"params"`
	Result  typeMeta   `json:"result"`
	Private bool       `json:"private,omitempty"`
	// Sig is the signature as messages write it, Canon as Satisfies compares it
	Sig   string `json:"sig"`
	Canon string `json:"canon"`
	// Export is the module export that runs it on an object of the class (none for a native class's)
	Export string `json:"export,omitempty"`
}

type funcMeta struct {
	Params []typeMeta `json:"params"`
	Result typeMeta   `json:"result"`
	Export string     `json:"export"`
}

// nativeMeta is a native the script calls: "f" for a function, "C.m" for a method and "C.f" for a field of a
// native class, of the host module Host.
type nativeMeta struct {
	Host   string     `json:"host"`
	Name   string     `json:"name"`
	This   bool       `json:"this,omitempty"`
	Params []typeMeta `json:"params"`
	Result typeMeta   `json:"result"`
}

// canon is a type as Satisfies compares it: classes and interfaces by the module that declares them.
func canon(t *Type) string {
	switch t.kind {
	case tArray:
		return canon(t.elem) + "[]"
	case tMap:
		return "Map<" + canon(t.key) + ", " + canon(t.elem) + ">"
	case tClass:
		return t.class.mod.id + ":" + t.class.name
	case tIface:
		return t.iface.mod.id + ":" + t.iface.name
	}
	return t.String()
}

func canonSig(f *funcInfo) string {
	parts := make([]string, len(f.params))
	for i, p := range f.params {
		parts[i] = canon(p)
	}
	return "(" + strings.Join(parts, ", ") + "): " + canon(f.result)
}

// formatMessage fills in a message's arguments.
func formatMessage(msg string, arg func(i int, kind byte) string) string {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		if msg[i] == '{' && i+3 < len(msg) && msg[i+3] == '}' {
			b.WriteString(arg(int(msg[i+1]-'0'), msg[i+2]))
			i += 3
			continue
		}
		b.WriteByte(msg[i])
	}
	return b.String()
}

// numberText is a number's text form, as str() and + write it.
func numberText(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func quoteText(s string) string { return strconv.Quote(s) }

// finite reports whether f is a number scripts can hold.
func finite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }

func (m *scriptMeta) position(pos int32) (string, int, int) {
	i := int(pos) * 3
	if pos < 0 || i+2 >= len(m.Positions) {
		return m.Entry, 1, 1
	}
	mod := ""
	if mi := int(m.Positions[i]); mi < len(m.Modules) {
		mod = m.Modules[mi]
	}
	return mod, int(m.Positions[i+1]), int(m.Positions[i+2])
}

func (m *scriptMeta) errorAt(pos int32, format string, args ...any) *ScriptError {
	mod, line, col := m.position(pos)
	return &ScriptError{Module: mod, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}
