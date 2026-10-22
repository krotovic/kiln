// Package expr is a small, safe expression language that hosts embed to compute values from named inputs.
//
// An expression is compiled once against a Library of functions and a Scope of the names it may use, then
// evaluated any number of times against an Env. Values are numbers, strings or bools, with strict typing: the only
// implicit conversion is string concatenation with +. There is no assignment, looping, recursion, I/O or
// randomness, so a compiled Program is deterministic and safe to share between goroutines. See docs/expressions.md.
package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is the type of a Value.
type Kind uint8

const (
	Number Kind = iota + 1
	String
	Bool
)

func (k Kind) String() string {
	switch k {
	case Number:
		return "number"
	case String:
		return "string"
	case Bool:
		return "bool"
	}
	return "nothing"
}

// Value is a number, a string or a bool. The zero Value is none of them and only appears as "no value".
type Value struct {
	kind Kind
	num  float64
	str  string // a string's text, or the text a Literal number was written as
	b    bool
}

// Num, Str and Boolean make values.
func Num(f float64) Value   { return Value{kind: Number, num: f} }
func Str(s string) Value    { return Value{kind: String, str: s} }
func Boolean(b bool) Value  { return Value{kind: Bool, b: b} }
func (v Value) Kind() Kind  { return v.kind }
func (v Value) Valid() bool { return v.kind != 0 }

// AsNumber, AsString and AsBool return the value and whether it has that kind.
func (v Value) AsNumber() (float64, bool) { return v.num, v.kind == Number }
func (v Value) AsString() (string, bool)  { return v.str, v.kind == String }
func (v Value) AsBool() (bool, bool)      { return v.b, v.kind == Bool }

// String is the value's text form, as str() and string concatenation write it: numbers in the shortest form that
// reads back the same ("3", "0.25") or, read by Literal, as they were written ("01"); bools as "true" or "false".
func (v Value) String() string {
	switch v.kind {
	case Number:
		if v.str != "" {
			return v.str
		}
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	case String:
		return v.str
	case Bool:
		return strconv.FormatBool(v.b)
	}
	return ""
}

// Interface returns the value as a Go float64, string or bool (nil for no value).
func (v Value) Interface() any {
	switch v.kind {
	case Number:
		return v.num
	case String:
		return v.str
	case Bool:
		return v.b
	}
	return nil
}

// Literal reads a value written as plain text, as value lists in content do: a number when the text is one,
// "true" and "false" as bools, and a string otherwise. A number keeps its text, so "01" counts as 1 but is still
// written "01" (into a string field, or joined to a string).
func Literal(s string) Value {
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return Value{kind: Number, num: f, str: s}
	}
	switch s {
	case "true":
		return Boolean(true)
	case "false":
		return Boolean(false)
	}
	return Str(s)
}

// Conversion turns a value into another kind, as num(), int(), str() and bool() do.
type Conversion uint8

const (
	AsIs Conversion = iota
	ToNumber
	ToInteger
	ToString
	ToBool
)

// ParseConversion reads a conversion's name: "" (as is), "number", "integer", "string" or "bool".
func ParseConversion(name string) (Conversion, error) {
	switch name {
	case "":
		return AsIs, nil
	case "number":
		return ToNumber, nil
	case "integer":
		return ToInteger, nil
	case "string":
		return ToString, nil
	case "bool":
		return ToBool, nil
	}
	return AsIs, fmt.Errorf("unknown type %q (want number, integer, string or bool)", name)
}

// Convert applies c to v. Numbers read from strings with surrounding spaces allowed; integers truncate toward
// zero; numbers are true unless 0; only "true" and "false" read as bools.
func Convert(v Value, c Conversion) (Value, error) {
	switch c {
	case AsIs:
		return v, nil
	case ToNumber, ToInteger:
		var f float64
		switch v.kind {
		case Number:
			f = v.num // a number made from it is written in its own form
		case Bool:
			if v.b {
				f = 1
			}
		case String:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v.str), 64)
			if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
				return Value{}, fmt.Errorf("%q is not a number", v.str)
			}
			f = parsed
		default:
			return Value{}, fmt.Errorf("no value to convert")
		}
		if c == ToInteger {
			f = math.Trunc(f)
		}
		return Num(f), nil
	case ToString:
		if !v.Valid() {
			return Value{}, fmt.Errorf("no value to convert")
		}
		return Str(v.String()), nil
	case ToBool:
		switch v.kind {
		case Bool:
			return v, nil
		case Number:
			return Boolean(v.num != 0), nil
		case String:
			switch v.str {
			case "true":
				return Boolean(true), nil
			case "false":
				return Boolean(false), nil
			}
			return Value{}, fmt.Errorf("%q is not a bool", v.str)
		}
		return Value{}, fmt.Errorf("no value to convert")
	}
	return Value{}, fmt.Errorf("unknown conversion")
}
