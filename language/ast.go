package language

// The syntax of scripts. Expressions share the expression engine's nodes (literalNode, nameNode, unaryNode,
// binaryNode, logicalNode, condNode, callNode) and add the nodes below, which only the script checker reads.

// scriptExpr is the part every script-only expression node shares. Expressions never parse one, so eval is
// never reached.
type scriptExpr struct{ pos int }

func (n scriptExpr) position() int { return n.pos }
func (n scriptExpr) eval(*evalCtx) (Value, error) {
	return Value{}, evalErr(n.pos, "not allowed in an expression")
}

type nullNode struct{ scriptExpr }

type thisNode struct{ scriptExpr }

// memberNode is x.name; pos is the name's.
type memberNode struct {
	scriptExpr
	x    node
	name string
}

// indexNode is x[i]; pos is the bracket's.
type indexNode struct {
	scriptExpr
	x, i node
}

// methodCallNode is recv.name(args); pos is the name's.
type methodCallNode struct {
	scriptExpr
	recv node
	name string
	args []node
}

// superCallNode is super(args), in a constructor.
type superCallNode struct {
	scriptExpr
	args []node
}

// superMethodNode is super.name(args); pos is the name's.
type superMethodNode struct {
	scriptExpr
	name string
	args []node
}

// newNode is new T(args).
type newNode struct {
	scriptExpr
	typ  *typeExpr
	args []node
}

// arrayNode is [items].
type arrayNode struct {
	scriptExpr
	items []node
}

// typeExpr is a type as written: a name (number, string, bool, void, a class, interface or alias), Map with
// arguments, and any number of [] after it.
type typeExpr struct {
	pos  int
	name string
	args []*typeExpr
	dims int
}

type stmt interface{ stmtPos() int }

type stmtBase struct{ pos int }

func (s stmtBase) stmtPos() int { return s.pos }

// letStmt is let or const; pos is the name's.
type letStmt struct {
	stmtBase
	name     string
	constant bool
	typ      *typeExpr
	init     node
}

// assignStmt is target op value, with op one of = += -= *= /= %=, or target++ and target-- (value nil).
type assignStmt struct {
	stmtBase
	target node
	op     string
	value  node
}

type exprStmt struct {
	stmtBase
	x node
}

type ifStmt struct {
	stmtBase
	cond node
	then *blockStmt
	els  stmt // *blockStmt, *ifStmt or nil
}

type whileStmt struct {
	stmtBase
	cond node
	body *blockStmt
}

type forStmt struct {
	stmtBase
	init stmt // *letStmt, *assignStmt, *exprStmt or nil
	cond node
	step stmt // *assignStmt, *exprStmt or nil
	body *blockStmt
}

// forOfStmt is for (let|const name of iter); pos is the name's.
type forOfStmt struct {
	stmtBase
	name     string
	constant bool
	iter     node
	body     *blockStmt
}

type breakStmt struct{ stmtBase }

type continueStmt struct{ stmtBase }

type returnStmt struct {
	stmtBase
	value node
}

type blockStmt struct {
	stmtBase
	stmts []stmt
}

type paramDecl struct {
	pos  int
	name string
	typ  *typeExpr
}

// funcDecl is a function, method or constructor. body is nil for a native one, or an interface's method.
type funcDecl struct {
	pos      int
	name     string
	params   []paramDecl
	result   *typeExpr // nil for void
	body     *blockStmt
	private  bool
	optional bool // an interface's optional method
}

type fieldDecl struct {
	pos      int
	name     string
	typ      *typeExpr
	init     node
	private  bool
	readonly bool
}

type classDecl struct {
	pos        int
	name       string
	extends    *typeExpr
	implements []*typeExpr
	fields     []*fieldDecl
	methods    []*funcDecl
	ctor       *funcDecl
	native     bool
}

type interfaceDecl struct {
	pos     int
	name    string
	extends []*typeExpr
	fields  []*fieldDecl
	methods []*funcDecl
}

type aliasDecl struct {
	pos  int
	name string
	typ  *typeExpr
}

type constDecl struct {
	pos  int
	name string
	typ  *typeExpr
	init node
}

type importName struct {
	pos   int
	name  string // the exported name
	alias string // the local name
}

type importDecl struct {
	pos   int
	from  string
	names []importName
}

// decl is one top-level declaration: exactly one of its fields is set.
type decl struct {
	exported  bool
	isDefault bool
	class     *classDecl
	iface     *interfaceDecl
	alias     *aliasDecl
	fn        *funcDecl
	native    bool // a native function
	konst     *constDecl
	sym       *symbol // what declare made of it; nil when it failed
}

func (d *decl) name() (string, int) {
	switch {
	case d.class != nil:
		return d.class.name, d.class.pos
	case d.iface != nil:
		return d.iface.name, d.iface.pos
	case d.alias != nil:
		return d.alias.name, d.alias.pos
	case d.fn != nil:
		return d.fn.name, d.fn.pos
	}
	return d.konst.name, d.konst.pos
}

type moduleAST struct {
	imports []*importDecl
	decls   []*decl
}
