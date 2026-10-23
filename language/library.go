package language

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Func is a function expressions can call. Call gets as many arguments as MinArgs..MaxArgs allow (MaxArgs -1
// means any number) and must be pure and deterministic: the same arguments always give the same result, with no
// side effects, so programs can be folded, cached and shared.
type Func struct {
	Name    string
	MinArgs int
	MaxArgs int
	Call    func(args []Value) (Value, error)
}

// Library is an immutable set of functions.
type Library struct {
	funcs map[string]Func
}

// NewLibrary makes a library of funcs.
func NewLibrary(funcs ...Func) Library {
	return Library{}.With(funcs...)
}

// With returns a copy of the library with more functions, replacing any of the same name; the receiver is
// unchanged, so hosts extend Std() without touching it.
func (l Library) With(funcs ...Func) Library {
	next := make(map[string]Func, len(l.funcs)+len(funcs))
	for n, f := range l.funcs {
		next[n] = f
	}
	for _, f := range funcs {
		next[f.Name] = f
	}
	return Library{funcs: next}
}

// Lookup finds a function by name.
func (l Library) Lookup(name string) (Func, bool) {
	f, ok := l.funcs[name]
	return f, ok
}

// Names lists the library's functions, sorted.
func (l Library) Names() []string {
	out := make([]string, 0, len(l.funcs))
	for n := range l.funcs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var std = NewLibrary(
	numbersFunc("min", 1, -1, func(xs []float64) float64 {
		m := xs[0]
		for _, x := range xs[1:] {
			m = math.Min(m, x)
		}
		return m
	}),
	numbersFunc("max", 1, -1, func(xs []float64) float64 {
		m := xs[0]
		for _, x := range xs[1:] {
			m = math.Max(m, x)
		}
		return m
	}),
	numbersFunc("floor", 1, 1, func(xs []float64) float64 { return math.Floor(xs[0]) }),
	numbersFunc("ceil", 1, 1, func(xs []float64) float64 { return math.Ceil(xs[0]) }),
	numbersFunc("round", 1, 1, func(xs []float64) float64 { return math.Round(xs[0]) }),
	numbersFunc("abs", 1, 1, func(xs []float64) float64 { return math.Abs(xs[0]) }),
	numbersFunc("clamp", 3, 3, func(xs []float64) float64 { return math.Max(xs[1], math.Min(xs[2], xs[0])) }),
	conversionFunc("num", ToNumber),
	conversionFunc("int", ToInteger),
	conversionFunc("str", ToString),
	conversionFunc("bool", ToBool),
	Func{Name: "len", MinArgs: 1, MaxArgs: 1, Call: func(args []Value) (Value, error) {
		s, ok := args[0].AsString()
		if !ok {
			return Value{}, fmt.Errorf("len wants a string, got a %s", args[0].Kind())
		}
		return Num(float64(utf8.RuneCountInString(s))), nil
	}},
	Func{Name: "concat", MinArgs: 1, MaxArgs: -1, Call: func(args []Value) (Value, error) {
		var b strings.Builder
		for _, a := range args {
			b.WriteString(a.String())
		}
		return Str(b.String()), nil
	}},
)

// Std is the standard library: min, max, floor, ceil, round, abs, clamp, num, int, str, bool, len and concat.
func Std() Library { return std }

// numbersFunc makes a function of number arguments that returns a number.
func numbersFunc(name string, minArgs, maxArgs int, fn func([]float64) float64) Func {
	return Func{Name: name, MinArgs: minArgs, MaxArgs: maxArgs, Call: func(args []Value) (Value, error) {
		xs := make([]float64, len(args))
		for i, a := range args {
			n, ok := a.AsNumber()
			if !ok {
				return Value{}, fmt.Errorf("%s wants numbers, got a %s", name, a.Kind())
			}
			xs[i] = n
		}
		return Num(fn(xs)), nil
	}}
}

func conversionFunc(name string, c Conversion) Func {
	return Func{Name: name, MinArgs: 1, MaxArgs: 1, Call: func(args []Value) (Value, error) {
		return Convert(args[0], c)
	}}
}
