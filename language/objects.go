package language

import "fmt"

// object is a native class's object, which the host made with its payload (HostModule.Object).
type object struct {
	class   *classInfo
	payload any
}

// arrayVal and mapVal are a script's arrays and maps as the host sees them: copies of what the script held.
type arrayVal struct{ items []Value }

// mapVal keeps its entries in insertion order, as scripts' maps do.
type mapVal struct {
	keys, vals []Value
	index      map[mapKey]int
}

type mapKey struct {
	kind Kind
	num  float64
	str  string
}

func keyOf(v Value) mapKey {
	if v.kind == Number {
		n := v.num
		if n == 0 {
			n = 0 // -0 and 0 are one key
		}
		return mapKey{kind: Number, num: n}
	}
	return mapKey{kind: String, str: v.str}
}

// ScriptError is an error while a script runs: a failed operation, a null used, or a budget used up. It names
// the module, line and column, and matches ErrEval.
type ScriptError struct {
	Module string
	Line   int
	Col    int
	Msg    string
}

func (e *ScriptError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.Module, e.Line, e.Col, e.Msg)
}
func (e *ScriptError) Is(target error) bool { return target == ErrEval }
