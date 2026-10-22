package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

type exprCase struct {
	Src   string         `json:"src"`
	Env   map[string]any `json:"env"`
	Scope []string       `json:"scope"`
	Want  any            `json:"want"`
	Error string         `json:"error"`
}

func valueOf(t *testing.T, v any) Value {
	t.Helper()
	switch v := v.(type) {
	case float64:
		return Num(v)
	case string:
		return Str(v)
	case bool:
		return Boolean(v)
	}
	t.Fatalf("unsupported JSON value %v", v)
	return Value{}
}

func TestSharedCases(t *testing.T) {
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []exprCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, c := range file.Cases {
		t.Run(c.Src, func(t *testing.T) {
			env := MapEnv{}
			scope := NewScope(c.Scope...)
			for k, v := range c.Env {
				env[k] = valueOf(t, v)
				scope = scope.With(k)
			}
			prog, err := Compile(c.Src, Std(), scope, DefaultLimits)
			if c.Error == "compile" {
				var ce *Error
				if !errors.As(err, &ce) {
					t.Fatalf("Compile = %v, want a compile error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got, err := prog.Eval(env)
			if c.Error == "eval" {
				if !errors.Is(err, ErrEval) {
					t.Fatalf("Eval = %v, %v; want an evaluation error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if want := valueOf(t, c.Want); got != want {
				t.Fatalf("Eval = %v (%s), want %v (%s)", got, got.Kind(), want, want.Kind())
			}
		})
	}
}

func TestCompileErrorsCarryTheirColumn(t *testing.T) {
	_, err := Compile("count + nope", Std(), NewScope("count"), DefaultLimits)
	var ce *Error
	if !errors.As(err, &ce) || ce.Pos != 9 {
		t.Fatalf("err = %v, want a compile error at column 9", err)
	}
	if !strings.Contains(err.Error(), "column 9") {
		t.Fatalf("message %q doesn't name the column", err)
	}
}

func TestLimits(t *testing.T) {
	limits := Limits{MaxLength: 40, MaxDepth: 4, MaxNodes: 8}
	cases := map[string]string{
		"length": strings.Repeat("1+", 30) + "1",
		"depth":  "((((((1))))))",
		"nodes":  "1+1+1+1+1+1",
	}
	for name, src := range cases {
		if _, err := Compile(src, Std(), Scope{}, limits); err == nil {
			t.Errorf("%s: %q compiled, want a limit error", name, src)
		}
	}
	if _, err := Compile("1+1+1", Std(), Scope{}, limits); err != nil {
		t.Fatalf("a small expression failed: %v", err)
	}
}

func TestStringsStopAtTheLimit(t *testing.T) {
	limits := DefaultLimits
	limits.MaxString = 8
	for _, src := range []string{`s + s + s`, `concat(s, s, s)`, `str(s) + "!!!!!"`, `long`} {
		prog, err := Compile(src, Std(), NewScope("s", "long"), limits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prog.Eval(MapEnv{"s": Str("abcd"), "long": Str("123456789")}); !errors.Is(err, ErrEval) {
			t.Errorf("%s: err = %v, want a string over the limit refused", src, err)
		}
	}
	prog, _ := Compile(`s + s`, Std(), NewScope("s"), limits)
	if got, err := prog.Eval(MapEnv{"s": Str("abcd")}); err != nil || got != Str("abcdabcd") {
		t.Fatalf("a string at the limit: %v, %v", got, err)
	}
}

func TestHostsExtendTheLibraryWithoutChangingStd(t *testing.T) {
	double := Func{Name: "double", MinArgs: 1, MaxArgs: 1, Call: func(args []Value) (Value, error) {
		n, ok := args[0].AsNumber()
		if !ok {
			return Value{}, fmt.Errorf("double wants a number")
		}
		return Num(n * 2), nil
	}}
	lib := Std().With(double)
	prog, err := Compile("double(x) + 1", lib, NewScope("x"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := prog.Eval(MapEnv{"x": Num(4)}); got != Num(9) {
		t.Fatalf("Eval = %v, want 9", got)
	}
	if _, ok := Std().Lookup("double"); ok {
		t.Fatal("With changed Std()")
	}
	if _, err := Compile("double(1)", Std(), Scope{}, DefaultLimits); err == nil {
		t.Fatal("Std() compiled a host function")
	}
}

func TestNamesListsWhatTheExpressionUses(t *testing.T) {
	prog, err := Compile("b + a * a", Std(), NewScope("a", "b", "c"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(prog.Names(), ","); got != "a,b" {
		t.Fatalf("Names = %s, want a,b", got)
	}
}

func TestConstantPartsFoldAtCompileTime(t *testing.T) {
	prog, err := Compile("2 * 3 + str(1)", Std(), Scope{}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	lit, ok := prog.root.(*literalNode)
	if !ok || lit.v != Str("61") {
		t.Fatalf("root = %#v, want the literal \"61\"", prog.root)
	}
	// A failing constant part stays, and fails when evaluated.
	prog, err = Compile("1 / 0", Std(), Scope{}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prog.Eval(nil); !errors.Is(err, ErrEval) {
		t.Fatalf("Eval = %v, want an evaluation error", err)
	}
}

func TestProgramsAreSafeToShare(t *testing.T) {
	prog, err := Compile("x * 2 + len(s)", Std(), NewScope("x", "s"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				x := float64(i*100 + j)
				got, err := prog.Eval(MapEnv{"x": Num(x), "s": Str("abc")})
				if err != nil || got != Num(x*2+3) {
					t.Errorf("Eval = %v, %v; want %v", got, err, x*2+3)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestConvert(t *testing.T) {
	cases := []struct {
		in   Value
		conv string
		want Value
		fail bool
	}{
		{Str("2.5"), "number", Num(2.5), false},
		{Str("2.5"), "integer", Num(2), false},
		{Num(-2.5), "integer", Num(-2), false},
		{Num(3), "string", Str("3"), false},
		{Boolean(true), "string", Str("true"), false},
		{Num(0), "bool", Boolean(false), false},
		{Str("false"), "bool", Boolean(false), false},
		{Str("x"), "", Str("x"), false},
		{Str("x"), "number", Value{}, true},
		{Str("x"), "bool", Value{}, true},
	}
	for _, c := range cases {
		conv, err := ParseConversion(c.conv)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Convert(c.in, conv)
		if (err != nil) != c.fail || (!c.fail && got != c.want) {
			t.Errorf("Convert(%v, %q) = %v, %v; want %v (fail %v)", c.in, c.conv, got, err, c.want, c.fail)
		}
	}
	if _, err := ParseConversion("float"); err == nil {
		t.Error("ParseConversion accepted an unknown type")
	}
}

func TestLiteral(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		text string
	}{
		{"3", Number, "3"},
		{"01", Number, "01"},
		{"1.50", Number, "1.50"},
		{"true", Bool, "true"},
		{"image/a", String, "image/a"},
		{"", String, ""},
	}
	for _, c := range cases {
		if got := Literal(c.in); got.Kind() != c.kind || got.String() != c.text {
			t.Errorf("Literal(%q) = %v (%s), want %q (%s)", c.in, got, got.Kind(), c.text, c.kind)
		}
	}
	// A literal number counts as its value, and loses its form only through arithmetic or num()
	prog, err := Compile(`v == 1 && v + 1 == 2 && "x" + v == "x01" && str(num(v)) == "1" && str(v) == "01"`, Std(), NewScope("v"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := prog.Eval(MapEnv{"v": Literal("01")}); err != nil || got != Boolean(true) {
		t.Fatalf("Eval = %v, %v; want true", got, err)
	}
}
