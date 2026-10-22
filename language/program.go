package expr

import (
	"math"
	"sort"
)

// Program is a compiled expression. It is immutable and safe to evaluate from many goroutines at once.
type Program struct {
	src       string
	root      node
	names     []string
	maxString int
}

// evalCtx is what a node evaluates against: the names' values and the string length limit.
type evalCtx struct {
	env       Env
	maxString int
}

// checkString refuses a string result longer than the limit, so expressions that join strings (perhaps of other
// params, each joining the one before) can't grow them without bound.
func (c *evalCtx) checkString(pos int, v Value) (Value, error) {
	if s, ok := v.AsString(); ok && c.maxString > 0 && len(s) > c.maxString {
		return Value{}, evalErr(pos, "a string longer than %d characters", c.maxString)
	}
	return v, nil
}

// Compile reads src and checks it against lib and scope: every function must be in lib and called with an
// allowed number of arguments, and every name must be in scope. Parts that use no names are computed once here.
// Errors are *Error, with the column they start at.
func Compile(src string, lib Library, scope Scope, limits Limits) (*Program, error) {
	root, err := parse(src, limits)
	if err != nil {
		return nil, err
	}
	used := map[string]struct{}{}
	root, err = bind(root, lib, scope, used, limits.MaxString)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	return &Program{src: src, root: root, names: names, maxString: limits.MaxString}, nil
}

// Eval computes the expression's value with env's names. Errors are *EvalError and match ErrEval.
func (p *Program) Eval(env Env) (Value, error) {
	if env == nil {
		env = MapEnv(nil)
	}
	return p.root.eval(&evalCtx{env: env, maxString: p.maxString})
}

// Names lists the names the expression uses, sorted, so a host knows which values it needs.
func (p *Program) Names() []string { return append([]string(nil), p.names...) }

// Source is the text the program was compiled from.
func (p *Program) Source() string { return p.src }

// bind resolves names and functions, and folds parts without names into literals.
func bind(n node, lib Library, scope Scope, used map[string]struct{}, maxString int) (node, error) {
	var err error
	switch n := n.(type) {
	case *nameNode:
		if !scope.Has(n.name) {
			return nil, compileErr(n.pos, "unknown name %q", n.name)
		}
		used[n.name] = struct{}{}
		return n, nil
	case *unaryNode:
		if n.x, err = bind(n.x, lib, scope, used, maxString); err != nil {
			return nil, err
		}
	case *binaryNode:
		if n.l, err = bind(n.l, lib, scope, used, maxString); err != nil {
			return nil, err
		}
		if n.r, err = bind(n.r, lib, scope, used, maxString); err != nil {
			return nil, err
		}
	case *logicalNode:
		if n.l, err = bind(n.l, lib, scope, used, maxString); err != nil {
			return nil, err
		}
		if n.r, err = bind(n.r, lib, scope, used, maxString); err != nil {
			return nil, err
		}
	case *condNode:
		if n.c, err = bind(n.c, lib, scope, used, maxString); err != nil {
			return nil, err
		}
		if n.a, err = bind(n.a, lib, scope, used, maxString); err != nil {
			return nil, err
		}
		if n.b, err = bind(n.b, lib, scope, used, maxString); err != nil {
			return nil, err
		}
	case *callNode:
		fn, ok := lib.Lookup(n.name)
		if !ok {
			return nil, compileErr(n.pos, "unknown function %q", n.name)
		}
		if len(n.args) < fn.MinArgs || (fn.MaxArgs >= 0 && len(n.args) > fn.MaxArgs) {
			return nil, compileErr(n.pos, "%s takes %s", n.name, arity(fn))
		}
		n.fn = fn
		for i := range n.args {
			if n.args[i], err = bind(n.args[i], lib, scope, used, maxString); err != nil {
				return nil, err
			}
		}
	}
	return fold(n, maxString), nil
}

// fold replaces a node whose children are all literals with its value. A part that fails (say, 1 / 0) stays, so
// it fails when evaluated, like any other evaluation error.
func fold(n node, maxString int) node {
	if _, ok := n.(*literalNode); ok {
		return n
	}
	for _, c := range children(n) {
		if _, ok := c.(*literalNode); !ok {
			return n
		}
	}
	v, err := n.eval(&evalCtx{env: MapEnv(nil), maxString: maxString})
	if err != nil {
		return n
	}
	return &literalNode{v: v, pos: n.position()}
}

func children(n node) []node {
	switch n := n.(type) {
	case *unaryNode:
		return []node{n.x}
	case *binaryNode:
		return []node{n.l, n.r}
	case *logicalNode:
		return []node{n.l, n.r}
	case *condNode:
		return []node{n.c, n.a, n.b}
	case *callNode:
		return n.args
	case *nameNode:
		return []node{n} // never folds
	}
	return nil
}

func arity(fn Func) string {
	switch {
	case fn.MaxArgs < 0:
		return plural(fn.MinArgs) + " or more"
	case fn.MinArgs == fn.MaxArgs:
		return plural(fn.MinArgs)
	}
	return plural(fn.MinArgs) + " to " + plural(fn.MaxArgs)
}

func plural(n int) string {
	if n == 1 {
		return "1 argument"
	}
	return Num(float64(n)).String() + " arguments"
}

type node interface {
	eval(c *evalCtx) (Value, error)
	position() int
}

type literalNode struct {
	v   Value
	pos int
}

type nameNode struct {
	name string
	pos  int
}

type unaryNode struct {
	op  string
	x   node
	pos int
}

type binaryNode struct {
	op   string
	l, r node
	pos  int
}

type logicalNode struct {
	op   string
	l, r node
	pos  int
}

type condNode struct {
	c, a, b node
	pos     int
}

type callNode struct {
	name string
	fn   Func
	args []node
	pos  int
}

func (n *literalNode) position() int { return n.pos }
func (n *nameNode) position() int    { return n.pos }
func (n *unaryNode) position() int   { return n.pos }
func (n *binaryNode) position() int  { return n.pos }
func (n *logicalNode) position() int { return n.pos }
func (n *condNode) position() int    { return n.pos }
func (n *callNode) position() int    { return n.pos }

func (n *literalNode) eval(*evalCtx) (Value, error) { return n.v, nil }

func (n *nameNode) eval(c *evalCtx) (Value, error) {
	v, ok := c.env.Lookup(n.name)
	if !ok || !v.Valid() {
		return Value{}, evalErr(n.pos, "%s has no value", n.name)
	}
	return c.checkString(n.pos, v)
}

func (n *unaryNode) eval(c *evalCtx) (Value, error) {
	x, err := n.x.eval(c)
	if err != nil {
		return Value{}, err
	}
	if n.op == "!" {
		b, ok := x.AsBool()
		if !ok {
			return Value{}, evalErr(n.pos, "! wants a bool, got a %s", x.Kind())
		}
		return Boolean(!b), nil
	}
	f, ok := x.AsNumber()
	if !ok {
		return Value{}, evalErr(n.pos, "- wants a number, got a %s", x.Kind())
	}
	return Num(-f), nil
}

func (n *binaryNode) eval(c *evalCtx) (Value, error) {
	l, err := n.l.eval(c)
	if err != nil {
		return Value{}, err
	}
	r, err := n.r.eval(c)
	if err != nil {
		return Value{}, err
	}
	switch n.op {
	case "==", "!=":
		if l.Kind() != r.Kind() {
			return Value{}, evalErr(n.pos, "%s compares a %s with a %s", n.op, l.Kind(), r.Kind())
		}
		return Boolean(equal(l, r) == (n.op == "==")), nil
	case "<", "<=", ">", ">=":
		return n.compare(l, r)
	case "+":
		if l.Kind() == String || r.Kind() == String {
			if len(l.String())+len(r.String()) > c.maxString && c.maxString > 0 {
				return Value{}, evalErr(n.pos, "a string longer than %d characters", c.maxString)
			}
			return Str(l.String() + r.String()), nil
		}
	}
	a, aok := l.AsNumber()
	b, bok := r.AsNumber()
	if !aok || !bok {
		return Value{}, evalErr(n.pos, "%s wants numbers, got a %s and a %s", n.op, l.Kind(), r.Kind())
	}
	var f float64
	switch n.op {
	case "+":
		f = a + b
	case "-":
		f = a - b
	case "*":
		f = a * b
	case "/":
		if b == 0 {
			return Value{}, evalErr(n.pos, "division by zero")
		}
		f = a / b
	case "%":
		if b == 0 {
			return Value{}, evalErr(n.pos, "division by zero")
		}
		f = math.Mod(a, b)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return Value{}, evalErr(n.pos, "%s gives a number too large", n.op)
	}
	return Num(f), nil
}

func (n *binaryNode) compare(l, r Value) (Value, error) {
	var c int
	switch {
	case l.Kind() == Number && r.Kind() == Number:
		a, _ := l.AsNumber()
		b, _ := r.AsNumber()
		c = cmpOrdered(a, b)
	case l.Kind() == String && r.Kind() == String:
		a, _ := l.AsString()
		b, _ := r.AsString()
		c = cmpOrdered(a, b)
	default:
		return Value{}, evalErr(n.pos, "%s compares a %s with a %s", n.op, l.Kind(), r.Kind())
	}
	switch n.op {
	case "<":
		return Boolean(c < 0), nil
	case "<=":
		return Boolean(c <= 0), nil
	case ">":
		return Boolean(c > 0), nil
	}
	return Boolean(c >= 0), nil
}

// equal compares two values of the same kind by what they are, not how they were written.
func equal(l, r Value) bool {
	switch l.Kind() {
	case Number:
		return l.num == r.num
	case String:
		return l.str == r.str
	}
	return l.b == r.b
}

func cmpOrdered[T float64 | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (n *logicalNode) eval(c *evalCtx) (Value, error) {
	l, err := n.l.eval(c)
	if err != nil {
		return Value{}, err
	}
	a, ok := l.AsBool()
	if !ok {
		return Value{}, evalErr(n.pos, "%s wants bools, got a %s", n.op, l.Kind())
	}
	if (n.op == "&&" && !a) || (n.op == "||" && a) {
		return Boolean(a), nil
	}
	r, err := n.r.eval(c)
	if err != nil {
		return Value{}, err
	}
	b, ok := r.AsBool()
	if !ok {
		return Value{}, evalErr(n.pos, "%s wants bools, got a %s", n.op, r.Kind())
	}
	return Boolean(b), nil
}

func (n *condNode) eval(c *evalCtx) (Value, error) {
	cond, err := n.c.eval(c)
	if err != nil {
		return Value{}, err
	}
	b, ok := cond.AsBool()
	if !ok {
		return Value{}, evalErr(n.pos, "? wants a bool, got a %s", cond.Kind())
	}
	if b {
		return n.a.eval(c)
	}
	return n.b.eval(c)
}

func (n *callNode) eval(c *evalCtx) (Value, error) {
	args := make([]Value, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(c)
		if err != nil {
			return Value{}, err
		}
		args[i] = v
	}
	v, err := n.fn.Call(args)
	if err != nil {
		return Value{}, evalErr(n.pos, "%s: %v", n.name, err)
	}
	if !v.Valid() {
		return Value{}, evalErr(n.pos, "%s gave no value", n.name)
	}
	if f, ok := v.AsNumber(); ok && (math.IsInf(f, 0) || math.IsNaN(f)) {
		return Value{}, evalErr(n.pos, "%s gives a number too large", n.name)
	}
	return c.checkString(n.pos, v)
}
