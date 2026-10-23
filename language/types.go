package language

import (
	"strings"
)

type typeKind uint8

const (
	tInvalid typeKind = iota // a part that already failed to check; it matches anything, so one mistake is reported once
	tNumber
	tString
	tBool
	tVoid
	tNull // the type of null, which every reference type accepts
	tArray
	tMap
	tClass
	tIface
)

// Type is a script type. Primitive and null types are shared values; arrays and maps are compared by structure,
// classes and interfaces by declaration.
type Type struct {
	kind  typeKind
	elem  *Type // arrays' elements, maps' values
	key   *Type // maps' keys
	class *classInfo
	iface *ifaceInfo
}

var (
	typInvalid = &Type{kind: tInvalid}
	typNumber  = &Type{kind: tNumber}
	typString  = &Type{kind: tString}
	typBool    = &Type{kind: tBool}
	typVoid    = &Type{kind: tVoid}
	typNull    = &Type{kind: tNull}
)

func arrayOf(t *Type) *Type     { return &Type{kind: tArray, elem: t} }
func mapOf(k, v *Type) *Type    { return &Type{kind: tMap, key: k, elem: v} }
func (t *Type) isRef() bool     { return t.kind >= tArray }
func (t *Type) primitive() bool { return t.kind == tNumber || t.kind == tString || t.kind == tBool }

func (t *Type) String() string {
	switch t.kind {
	case tNumber:
		return "number"
	case tString:
		return "string"
	case tBool:
		return "bool"
	case tVoid:
		return "void"
	case tNull:
		return "null"
	case tArray:
		return t.elem.String() + "[]"
	case tMap:
		return "Map<" + t.key.String() + ", " + t.elem.String() + ">"
	case tClass:
		return t.class.name
	case tIface:
		return t.iface.name
	}
	return "?"
}

// kind is the Value kind a value of the type has (null aside).
func (t *Type) valueKind() Kind {
	switch t.kind {
	case tNumber:
		return Number
	case tString:
		return String
	case tBool:
		return Bool
	case tArray:
		return Array
	case tMap:
		return Map
	case tClass, tIface:
		return Object
	}
	return 0
}

// zeroOf is the value a variable, field or slot of type t starts with: 0, false, "" or null.
func zeroOf(t *Type) Value {
	switch t.kind {
	case tNumber:
		return Num(0)
	case tString:
		return Str("")
	case tBool:
		return Boolean(false)
	}
	return null
}

func identical(a, b *Type) bool {
	if a == b {
		return true
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case tArray:
		return identical(a.elem, b.elem)
	case tMap:
		return identical(a.key, b.key) && identical(a.elem, b.elem)
	case tClass:
		return a.class == b.class
	case tIface:
		return a.iface == b.iface
	}
	return true
}

// assignable reports whether a value of type from may go where a to is expected: the same type, null into a
// reference, a class into its base classes, and a class or interface into an interface it satisfies.
func assignable(from, to *Type) bool {
	if from.kind == tInvalid || to.kind == tInvalid || identical(from, to) {
		return true
	}
	if from.kind == tNull {
		return to.isRef()
	}
	switch to.kind {
	case tClass:
		if from.kind == tClass {
			return from.class.isA(to.class)
		}
	case tIface:
		if from.kind == tClass {
			return from.class.satisfies(to.iface) == ""
		}
		if from.kind == tIface {
			return ifaceSatisfies(from.iface, to.iface) == ""
		}
	}
	return false
}

// classInfo is a class: its fields (inherited ones first, so a subclass keeps its base's slots), its methods by
// name (inherited ones included, overrides replacing them) and its constructor.
type classInfo struct {
	name        string
	mod         *moduleInfo
	pos         int
	decl        *classDecl
	native      bool
	super       *classInfo
	fields      []*fieldInfo
	fieldByName map[string]*fieldInfo
	methods     map[string]*funcInfo
	ctor        *funcInfo
	implements  []*ifaceInfo
	typ         *Type
	state       uint8 // 0 unresolved, 1 resolving, 2 resolved
}

type fieldInfo struct {
	name     string
	pos      int
	typ      *Type
	slot     int
	owner    *classInfo
	private  bool
	readonly bool
	decl     *fieldDecl
	init     evalFn
	getter   NativeFunc // a native class's field
}

// isA reports whether c is base or derives from it.
func (c *classInfo) isA(base *classInfo) bool {
	for k := c; k != nil; k = k.super {
		if k == base {
			return true
		}
	}
	return false
}

// funcInfo is a function, method or constructor.
type funcInfo struct {
	name     string
	mod      *moduleInfo
	pos      int
	decl     *funcDecl
	params   []*Type
	names    []string
	result   *Type // typVoid for none
	nlocals  int
	body     execFn
	native   NativeFunc
	owner    *classInfo // methods and constructors
	private  bool
	optional bool // an interface's optional method
}

func (f *funcInfo) sameSignature(g *funcInfo) bool {
	if len(f.params) != len(g.params) || !identical(f.result, g.result) {
		return false
	}
	for i := range f.params {
		if !identical(f.params[i], g.params[i]) {
			return false
		}
	}
	return true
}

func (f *funcInfo) signature() string {
	parts := make([]string, len(f.params))
	for i, p := range f.params {
		parts[i] = f.names[i] + ": " + p.String()
	}
	return "(" + strings.Join(parts, ", ") + "): " + f.result.String()
}

// ifaceInfo is an interface: its fields and methods, its base interfaces' included.
type ifaceInfo struct {
	name    string
	mod     *moduleInfo
	pos     int
	decl    *interfaceDecl
	fields  map[string]*fieldInfo
	methods map[string]*funcInfo
	order   []string // member names in declaration order, for messages
	typ     *Type
	state   uint8
}

// satisfies says why c doesn't satisfy i, or "" when it does: c needs every field of i with the same type (a
// field i may change must not be readonly in c) and every method that isn't optional with the same signature, all
// public. An optional method c has must match too.
func (c *classInfo) satisfies(i *ifaceInfo) string {
	for _, name := range i.order {
		if m, ok := i.methods[name]; ok {
			cm := c.methods[name]
			if cm == nil {
				if _, isField := c.fieldByName[name]; isField {
					return name + " is a field, not a method"
				}
				if m.optional {
					continue
				}
				return "it lacks the method " + name
			}
			if cm.private {
				return name + " is private"
			}
			if !cm.sameSignature(m) {
				return name + " is " + cm.signature() + ", not " + m.signature()
			}
			continue
		}
		f := i.fields[name]
		cf := c.fieldByName[name]
		if cf == nil {
			return "it lacks the field " + name
		}
		if cf.private {
			return name + " is private"
		}
		if !identical(cf.typ, f.typ) {
			return name + " is " + cf.typ.String() + ", not " + f.typ.String()
		}
		if cf.readonly && !f.readonly {
			return name + " is readonly"
		}
	}
	return ""
}

// ifaceSatisfies says why an interface a doesn't satisfy b, or "" when it does, by the same rules.
func ifaceSatisfies(a, b *ifaceInfo) string {
	for _, name := range b.order {
		if m, ok := b.methods[name]; ok {
			am := a.methods[name]
			if am == nil {
				if m.optional {
					continue
				}
				return "it lacks the method " + name
			}
			if am.optional && !m.optional {
				return name + " is optional"
			}
			if !am.sameSignature(m) {
				return name + " is " + am.signature() + ", not " + m.signature()
			}
			continue
		}
		f := b.fields[name]
		af := a.fields[name]
		if af == nil {
			return "it lacks the field " + name
		}
		if !identical(af.typ, f.typ) {
			return name + " is " + af.typ.String() + ", not " + f.typ.String()
		}
		if af.readonly && !f.readonly {
			return name + " is readonly"
		}
	}
	return ""
}
