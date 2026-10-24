package language

// The checker turns each function, method, constructor, field value and module constant into this typed IR, which
// the code generator (gen_*.go) compiles to WebAssembly. Names are resolved already: variables to their slots,
// fields to their fieldInfo, calls to the funcInfo they call (or, through an interface or an overridden method,
// to the name the object's class answers). Each node keeps the source position its run-time errors name.

type irExpr interface{ typeOf() *Type }

type irBase struct{ t *Type }

func (e irBase) typeOf() *Type { return e.t }

type (
	irNum  struct{ v float64 }
	irStr  struct{ s string }
	irBool struct{ v bool }
	// irNull is null of a reference type t.
	irNull struct{ irBase }
	irBad  struct{}

	irLocal struct {
		irBase
		slot int
	}
	irThis  struct{ irBase }
	irConst struct {
		irBase
		k *constInfo
	}
	irNot struct{ x irExpr }
	irNeg struct{ x irExpr }
	// irArith is + - * / % on numbers.
	irArith struct {
		op   byte
		l, r irExpr
		pos  int
	}
	// irConcat is + with a string on either side, joining both operands' text forms.
	irConcat struct {
		l, r irExpr
		pos  int
	}
	// irCompare is < <= > >= on numbers or strings.
	irCompare struct {
		op   string
		l, r irExpr
	}
	// irEqual is == or != on two values of comparable types.
	irEqual struct {
		neq  bool
		l, r irExpr
	}
	irLogic struct {
		and  bool
		l, r irExpr
	}
	irCond struct {
		irBase
		c, a, b irExpr
	}
	// irCall calls a module's function (or a host module's native one).
	irCall struct {
		irBase
		fn   *funcInfo
		args []irExpr
		pos  int
	}
	// irStd calls the standard library.
	irStd struct {
		irBase
		name string
		args []irExpr
		pos  int
	}
	// irLength is a string's length, an array's length or a map's size.
	irLength struct{ x irExpr }
	// irField reads a field of an object of a known class (a native class's through the host).
	irField struct {
		irBase
		x   irExpr
		f   *fieldInfo
		pos int
	}
	// irIfaceField reads the field name of whatever object an interface holds.
	irIfaceField struct {
		irBase
		x    irExpr
		name string
		pos  int
	}
	irIndex struct {
		irBase
		x, i irExpr
		pos  int
	}
	// irMethod calls a method. fn is the method the receiver's static type has; the object's class decides what
	// runs, unless no class overrides it.
	irMethod struct {
		irBase
		recv  irExpr
		fn    *funcInfo
		name  string
		iface bool // the receiver is an interface
		args  []irExpr
		pos   int
	}
	// irSuper calls the base class's method fn on this.
	irSuper struct {
		irBase
		fn   *funcInfo
		args []irExpr
		pos  int
	}
	irNew struct {
		irBase
		class *classInfo
		args  []irExpr
		pos   int
	}
	irNewMap struct {
		irBase
		pos int
	}
	irArray struct {
		irBase
		items []irExpr
		pos   int
	}
	// irBuiltin calls a method of an array, string or map.
	irBuiltin struct {
		irBase
		name  string
		recv  irExpr
		recvT *Type
		args  []irExpr
		pos   int
	}
)

func (e *irNum) typeOf() *Type   { return typNumber }
func (e *irStr) typeOf() *Type   { return typString }
func (e *irBool) typeOf() *Type  { return typBool }
func (e *irBad) typeOf() *Type   { return typInvalid }
func (e *irNot) typeOf() *Type   { return typBool }
func (e *irNeg) typeOf() *Type   { return typNumber }
func (e *irArith) typeOf() *Type { return typNumber }

func (e *irConcat) typeOf() *Type  { return typString }
func (e *irCompare) typeOf() *Type { return typBool }
func (e *irEqual) typeOf() *Type   { return typBool }
func (e *irLogic) typeOf() *Type   { return typBool }
func (e *irLength) typeOf() *Type  { return typNumber }

var bad irExpr = &irBad{}

type irStmt interface{ irStmt() }

type (
	irBlock struct{ stmts []irStmt }
	// irLet sets a new variable's slot: to init, or to its type's zero value.
	irLet struct {
		slot int
		t    *Type
		init irExpr
		pos  int
	}
	// irAssign changes a variable, field or element: op is '=', the arithmetic operator of a compound assignment
	// (+ joins strings when the target is one), or 'i' and 'd' for ++ and -- (value nil).
	irAssign struct {
		target *irTarget
		op     byte
		value  irExpr
		pos    int
	}
	irExprStmt struct {
		x   irExpr
		pos int
	}
	irIf struct {
		cond      irExpr
		then, els irStmt
		pos       int
	}
	// irLoop runs init, then body while cond holds (always without one), with step after each turn.
	irLoop struct {
		init       irStmt
		cond       irExpr
		step, body irStmt
		pos        int
	}
	irForOf struct {
		slot int
		elem *Type
		iter irExpr
		body irStmt
		pos  int
	}
	irBreak    struct{}
	irContinue struct{}
	irReturn   struct {
		value irExpr
		pos   int
	}
	// irSuperCall runs the base class's constructor on this.
	irSuperCall struct {
		ctor *funcInfo
		args []irExpr
		pos  int
	}
	// irFieldInits sets the class's own fields that have values, in order.
	irFieldInits struct{ class *classInfo }
	irNop        struct{}
)

func (*irBlock) irStmt()      {}
func (*irLet) irStmt()        {}
func (*irAssign) irStmt()     {}
func (*irExprStmt) irStmt()   {}
func (*irIf) irStmt()         {}
func (*irLoop) irStmt()       {}
func (*irForOf) irStmt()      {}
func (*irBreak) irStmt()      {}
func (*irContinue) irStmt()   {}
func (*irReturn) irStmt()     {}
func (*irSuperCall) irStmt()  {}
func (*irFieldInits) irStmt() {}
func (*irNop) irStmt()        {}

// Target kinds of an assignment.
const (
	targetLocal = iota + 1
	targetField
	targetIfaceField
	targetIndex
)

// irTarget is what an assignment changes. The object or array and the index are evaluated once.
type irTarget struct {
	kind int
	t    *Type
	slot int        // targetLocal
	x    irExpr     // the object or array
	f    *fieldInfo // targetField
	name string     // targetIfaceField
	i    irExpr     // targetIndex
	pos  int
}
