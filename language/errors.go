package language

import (
	"errors"
	"fmt"
)

// ErrEval is what every evaluation error matches with errors.Is.
var ErrEval = errors.New("evaluation failed")

// Error is a compile error: the expression can't be read, or uses a name or function it may not. Pos is the
// 1-based column it starts at.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("column %d: %s", e.Pos, e.Msg) }

// EvalError is an error while evaluating: division by zero, a name missing from the Env, a value of the wrong
// kind, or a function that failed. It matches ErrEval.
type EvalError struct {
	Pos int
	Msg string
}

func (e *EvalError) Error() string        { return fmt.Sprintf("column %d: %s", e.Pos, e.Msg) }
func (e *EvalError) Is(target error) bool { return target == ErrEval }

func compileErr(pos int, format string, args ...any) *Error {
	return &Error{Pos: pos + 1, Msg: fmt.Sprintf(format, args...)}
}

func evalErr(pos int, format string, args ...any) *EvalError {
	return &EvalError{Pos: pos + 1, Msg: fmt.Sprintf(format, args...)}
}
