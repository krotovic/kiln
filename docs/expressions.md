# Expressions

An expression is the expression part of the [language](language-reference.md), compiled on its own and embedded
where content needs a small calculation: today the props and fields of [prefabs](prefabs.md#prefab-expressions).

## User perspective

Players see what expressions compute: a herb that glows at night, a sprite as big as its prop says. An expression has
no loops, so it finishes at once, and a failing one leaves its field as the content has it rather than breaking
anything.

## Authoring perspective

```text
"image/slime-" + str(count % 3)              → "image/slime-1" when count is 4
hour >= 20 || hour < 6 ? "night" : "day"     → "night" at 22:30
clamp(1 + count * 0.25, 1, 2)                → 1.5 when count is 2
```

- **What it may use:** the literals `true`, `false`, numbers (with `_` separators) and double-quoted strings (escapes
  `\"` and `\\` only), the operators of levels 1 to 8 (see
  [the language's operators](language-reference.md#expressions)), parentheses, the names its host declares, and calls
  of the functions in its host's library (the [built-ins](language-reference.md#built-ins)). Script syntax (`.`, `[`,
  `{`, `=`, `;`, single quotes) is refused with "not allowed in an expression"; words that are keywords in scripts
  (`new`, `class`…) are names in expressions.
- **Types:** numbers, strings and bools. Names have no declared types, so operators check their operands when the
  expression runs.
- **Limits** (`Limits`; `DefaultLimits` allows 1024 characters, 32 levels of nesting and 256 parts): since nothing
  loops, evaluation visits each part at most once. `MaxString` (4096 by default) bounds every string it makes or reads.
  Hosts may set smaller ones: prefabs allow 200 characters, 16 levels of nesting, 64 parts, and strings of at most 256
  characters.
- **Folding:** parts without names are computed once at compile time: `2 * 3 + str(1)` compiles to `"61"`. A constant
  part that fails, such as `1 / 0`, is kept and fails when evaluated.
- **Errors:** a compile error carries its 1-based column; besides the language's run-time errors, an operator or
  function given the wrong kind, and a name the host declared but had no value for, fail when evaluated.

## Technical details

### Embedding (Go)

```go
lib := language.Std().With(language.Func{
	Name: "lerp", MinArgs: 3, MaxArgs: 3,
	Call: func(args []language.Value) (language.Value, error) {
		a, ok1 := args[0].AsNumber()
		b, ok2 := args[1].AsNumber()
		t, ok3 := args[2].AsNumber()
		if !ok1 || !ok2 || !ok3 {
			return language.Value{}, fmt.Errorf("lerp wants numbers")
		}
		return language.Num(a + (b-a)*t), nil
	},
})
scope := language.NewScope("level", "progress")

prog, err := language.Compile(`lerp(10, 50, progress) * level`, lib, scope, language.DefaultLimits)
if err != nil {
	return err // *language.Error: refuse the content
}
v, err := prog.Eval(language.MapEnv{"level": language.Num(3), "progress": language.Num(0.5)})
```

- **`Library`** is an immutable set of functions. `With` returns a copy, so a host extends `Std()` without changing it
  for others. A host function must be pure and deterministic: no randomness, no clock, no I/O, and nothing that depends
  on state outside its arguments. Compile-time folding and shared programs rely on that.
- **`Scope`** declares the names a host offers, checked at compile time. `With` adds names, for example the params
  defined so far.
- **`Env`** gives the values at run time. `MapEnv` is a map; a host can implement `Env` to look values up lazily.
  `Program.Names()` lists the names an expression uses, so a host can compute only those.
- **Programs** are immutable and safe to evaluate from many goroutines at once. Compile once (when content loads or
  changes) and cache the program.
- **Errors:** a compile error is a `*language.Error`; an evaluation error (`*language.EvalError`) matches
  `language.ErrEval`.
- **`Literal`** reads plain text as a value (see
  [Text form and conversions](language-reference.md#text-form-and-conversions)). **`Convert`** and `ParseConversion`
  apply a type a content creator chose (`"number"`, `"integer"`, `"string"`, `"bool"`, or `""` for as is).

### Tests

`server/internal/engine/language/testdata/cases.json` holds the expression cases, shared by every implementation (a
TypeScript port on the client would run the same file, as `movement.json` is shared). Each case has:

- `src`: the expression, compiled with the standard library and a scope of `env`'s keys plus `scope`.
- `env`: the names' values, as JSON numbers, strings and bools.
- Either `want` (the expected value, which must match in kind too) or `error`: `"compile"` or `"eval"`.

To add a case, append it to `cases`; `TestSharedCases` runs them all. The cases cover numbers with `_` separators and
misplaced ones refused, precedence, string concatenation, the ternary, short-circuiting, every function and conversion,
evaluation and compile errors. Behaviour outside a single expression is tested in `expr_test.go`: compile errors
carrying their column; length, depth and size limits; a host library extended without changing `Std()`; the names an
expression uses; constant parts folded at compile time (a failing one kept); one program evaluated from many
goroutines; strings stopping at the limit; conversions and literals (list numbers keeping their text, compared by
value); script syntax refused in expressions, whose keywords stay names.
