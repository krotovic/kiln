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
	tPromise // a Promise<T> (elem), or Promise<T, E> (key: the error class)
)

// Type is a script type. Primitive and null types are shared values; arrays and maps are compared by structure,
// classes and interfaces by declaration.
type Type struct {
	kind  typeKind
	elem  *Type // arrays' elements, maps' values, promises' values
	key   *Type // maps' keys, promises' errors (nil: it can't fail)
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

func arrayOf(t *Type) *Type      { return &Type{kind: tArray, elem: t} }
func mapOf(k, v *Type) *Type     { return &Type{kind: tMap, key: k, elem: v} }
func promiseOf(v, e *Type) *Type { return &Type{kind: tPromise, elem: v, key: e} }
func (t *Type) isRef() bool      { return t.kind >= tArray }
func (t *Type) primitive() bool  { return t.kind == tNumber || t.kind == tString || t.kind == tBool }

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
	case tPromise:
		if t.key != nil {
			return "Promise<" + t.elem.String() + ", " + t.key.String() + ">"
		}
		return "Promise<" + t.elem.String() + ">"
	}
	return "?"
}

// isErrorClass reports whether t is an error class, which promises fail with.
func (t *Type) isErrorClass() bool { return t.kind == tClass && t.class.isError }

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
	case tClass, tIface, tPromise:
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
	case tPromise:
		if (a.key == nil) != (b.key == nil) {
			return false
		}
		return identical(a.elem, b.elem) && (a.key == nil || identical(a.key, b.key))
	}
	return true
}

// assignable reports whether a value of type from may go where a to is expected: the same type, null into a
// reference, a class into its base classes, a class or interface into an interface it satisfies, and a promise
// that can't fail where one of the same value may fail.
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
	case tPromise:
		if from.kind == tPromise && from.key == nil && to.key != nil {
			return identical(from.elem, to.elem)
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
	isError     bool // an error class: what a Promise<T, E> fails with
	hidden      bool // one the compiler makes: a promise, or an async function's frame
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
	init     irExpr
	getter   NativeFunc  // a native class's field
	mod      *moduleInfo // where it is declared
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
	result   *Type   // typVoid for none
	locals   []*Type // its variables' types by slot, its parameters first
	body     *irBlock
	native   NativeFunc
	owner    *classInfo // methods and constructors
	private  bool
	optional bool // an interface's optional method
	async    bool // returns a promise its body settles, and may await
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
	bases   []*ifaceInfo // the interfaces it extends
}

// extends reports whether i is base or extends it, directly or not.
func (i *ifaceInfo) extends(base *ifaceInfo) bool {
	if i == base {
		return true
	}
	for _, b := range i.bases {
		if b.extends(base) {
			return true
		}
	}
	return false
}

// member is what an interface's member is checked against: a class's field or method, by its signature or type
// as messages write it and as types compare (canon).
type member struct {
	method, private, readonly bool
	text, canon               string
}

// satisfies says why c doesn't satisfy i, or "" when it does (satisfiesBy).
func (c *classInfo) satisfies(i *ifaceInfo) string {
	return satisfiesBy(i, func(name string) (member, bool) {
		if m := c.methods[name]; m != nil {
			return member{method: true, private: m.private, text: m.signature(), canon: canonSig(m)}, true
		}
		if f := c.fieldByName[name]; f != nil {
			return member{private: f.private, readonly: f.readonly, text: f.typ.String(), canon: canon(f.typ)}, true
		}
		return member{}, false
	})
}

// satisfiesBy says why a class whose members get finds doesn't satisfy i, or "" when it does: it needs every
// field of i with the same type (a field i may change must not be readonly in it) and every method that isn't
// optional with the same signature, all public. An optional method it has must match too. Compiled scripts are
// checked by the same rules (Class.Satisfies).
func satisfiesBy(i *ifaceInfo, get func(name string) (member, bool)) string {
	for _, name := range i.order {
		cm, has := get(name)
		if m, ok := i.methods[name]; ok {
			if !has || !cm.method {
				if has {
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
			if cm.canon != canonSig(m) && !(m.optional && asyncForVoid(cm.canon, canonSig(m))) {
				return name + " is " + cm.text + ", not " + m.signature()
			}
			continue
		}
		f := i.fields[name]
		if !has || cm.method {
			return "it lacks the field " + name
		}
		if cm.private {
			return name + " is private"
		}
		if cm.canon != canon(f.typ) {
			return name + " is " + cm.text + ", not " + f.typ.String()
		}
		if cm.readonly && !f.readonly {
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

// asyncForVoid reports whether a method of signature have (canonSig) may stand for an optional method of
// signature want that returns nothing: the same parameters, returning a Promise<void> (which may fail), as an
// async method does. The host that calls it ignores the promise.
func asyncForVoid(have, want string) bool {
	params, ok := strings.CutSuffix(want, ": void")
	if !ok {
		return false
	}
	rest, ok := strings.CutPrefix(have, params+": Promise<void")
	return ok && (rest == ">" || strings.HasPrefix(rest, ", "))
}
