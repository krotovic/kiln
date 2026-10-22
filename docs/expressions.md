# Expressions

The expression engine (`server/internal/engine/expr`) is a small, safe language for computing a value from named
inputs. Content creators write expressions in the world files. The servers compile them when the content is saved and
evaluate them while the game runs.

## Purpose

Content needs small calculations that are too specific to hard-code: a sprite that depends on the hour, a size that
grows with how many have spawned. The engine gives content these calculations without giving it a programming
language.

Design goals:

- **Safe.** There is no assignment, looping, recursion, I/O or access to anything the host doesn't pass in. Every
  expression finishes, and how long it can take is bounded by its size.
- **Deterministic.** The same inputs always give the same result. There is no random function: randomness comes in
  only as inputs the host draws from a seeded generator (`RANDOM_SEED`).
- **Strictly typed.** A value is a number, a string or a bool, and operators accept only the kinds they're meant for.
  The only implicit conversion is string concatenation with `+`, so mistakes fail loudly instead of quietly producing
  `NaN` or `"undefined"`.
- **Checked early.** An expression is compiled against the names and functions its host offers. A typo is refused when
  the content is saved, with the column it's at.
- **Reusable.** Nothing in the package knows about the game. Spawn params are the first host (see
  [Hosts](#hosts)). Quest conditions, world-event rules, loot tables and UI formulas can embed it the same way.

## Language

```text
"image/slime-" + str(count % 3)              → "image/slime-1" when count is 4
hour >= 20 || hour < 6 ? "night" : "day"     → "night" at 22:30
clamp(1 + count * 0.25, 1, 2)                → 1.5 when count is 2
```

- **Literals.**
  - Numbers: `3`, `0.25`, `.5`, `1.5e2`.
  - Strings in double quotes: `"night"`. The only escapes are `\"` and `\\`.
  - `true` and `false`.
- **Names** start with a letter or `_`, followed by letters, digits and `_`. Which names exist depends on the host;
  `true` and `false` are keywords.
- **Calls:** `name(arg, …)`, for functions in the host's library.
- **Parentheses** group.
- **Comments:** none.
- **Whitespace** is ignored.

Operators, from loosest to tightest binding:

| Precedence | Operators | Operands | Result |
| --- | --- | --- | --- |
| 1 | `c ? a : b` | `c` a bool; `a`, `b` anything | `a` if `c`, else `b` (only that side is evaluated) |
| 2 | `\|\|` | bools | bool; the right side is skipped when the left is `true` |
| 3 | `&&` | bools | bool; the right side is skipped when the left is `false` |
| 4 | `==` `!=` | two values of the same kind | bool |
| 5 | `<` `<=` `>` `>=` | two numbers, or two strings (compared byte by byte) | bool |
| 6 | `+` `-` | numbers; `+` with a string on either side concatenates | number or string |
| 7 | `*` `/` `%` | numbers | number (`%` keeps the sign of the left operand) |
| 8 | unary `-` `!` | `-` a number, `!` a bool | number or bool |

Binary operators are left-associative (`10 - 4 - 3` is `3`). The ternary is right-associative
(`a ? 1 : b ? 2 : 3`).

## Types and conversions

There are three kinds: **number** (64-bit floating point, always finite), **string** and **bool**.

String concatenation writes each operand in its text form:

- Numbers come out in the shortest form that reads back the same: `3`, `0.25`, `150`.
- Bools come out as `true` and `false`.

So `"herb-" + 2` is `"herb-2"`.

The conversion functions, which are also the types a host may convert a result to (`number`, `integer`, `string`,
`bool`):

| From \ to | `num` (number) | `int` (integer) | `str` (string) | `bool` |
| --- | --- | --- | --- | --- |
| number | itself | truncated toward zero: `int(-2.7)` is `-2` | text form | `false` for 0, otherwise `true` |
| string | parsed, spaces around it allowed: `num(" 2.5 ")` is `2.5`; fails on `"abc"` | parsed, then truncated | itself | `"true"` and `"false"` only; fails on anything else |
| bool | 1 or 0 | 1 or 0 | `"true"` or `"false"` | itself |

When a host reads plain text from content, such as a value in a list, it uses `Literal`. Text that is a number becomes
a number, `true` and `false` become bools, and everything else becomes a string. A literal number keeps the text it
was written as: `"01"` counts as 1 (`v == 1`, `v + 1` is 2) but is still written `01` by `str` and string
concatenation. Arithmetic and `num` give a plain number, written in its shortest form.

## Standard library

`expr.Std()`:

| Function | Arguments | Result | Errors |
| --- | --- | --- | --- |
| `min(a, …)`, `max(a, …)` | one or more numbers | the smallest or largest | a non-number |
| `floor(x)`, `ceil(x)`, `round(x)` | a number | rounded down, up, or to the nearest (halves away from zero) | a non-number |
| `abs(x)` | a number | its magnitude | a non-number |
| `clamp(x, lo, hi)` | three numbers | `x` kept within `lo..hi` | a non-number |
| `num(v)`, `int(v)`, `str(v)`, `bool(v)` | any value | converted (see above) | a value that doesn't convert |
| `len(s)` | a string | its length in characters | a non-string |
| `concat(v, …)` | one or more values | their text forms joined | none |

## Limits and errors

Compiling enforces `Limits`. `DefaultLimits` allows 1024 characters, 32 levels of nesting and 256 parts (literals,
names, operators and calls). Hosts can set tighter limits, as spawn params do. Because nothing loops, evaluation visits
each part at most once. `MaxString` (4096 by default) bounds every string an evaluation makes or reads, so
expressions that join strings, perhaps of other values that joined strings, can't grow them without bound; a longer
one is an evaluation error.

There are two kinds of errors:

- **Compile errors** (`*expr.Error`, with a 1-based column). Causes: bad syntax, an unknown name or function, a
  function called with the wrong number of arguments, or an expression over the limits. The host refuses the content
  that holds it; the World answers `400` with the message, which the editor shows.
- **Evaluation errors** (`*expr.EvalError`, which matches `expr.ErrEval`). Causes: division or `%` by zero, an operator
  or function given the wrong kind, a failed conversion, a name the host declared but had no value for, a string
  over `MaxString`, or a number too large to be finite. The host decides what to do; spawn params skip the param (see below).

Parts without names are computed once at compile time. `2 * 3 + str(1)` compiles to the string `"61"`. A constant
part that fails, such as `1 / 0`, is kept and fails when evaluated.

## Embedding (Go)

```go
lib := expr.Std().With(expr.Func{
	Name: "lerp", MinArgs: 3, MaxArgs: 3,
	Call: func(args []expr.Value) (expr.Value, error) {
		a, ok1 := args[0].AsNumber()
		b, ok2 := args[1].AsNumber()
		t, ok3 := args[2].AsNumber()
		if !ok1 || !ok2 || !ok3 {
			return expr.Value{}, fmt.Errorf("lerp wants numbers")
		}
		return expr.Num(a + (b-a)*t), nil
	},
})
scope := expr.NewScope("level", "progress")

prog, err := expr.Compile(`lerp(10, 50, progress) * level`, lib, scope, expr.DefaultLimits)
if err != nil {
	return err // *expr.Error: refuse the content
}
v, err := prog.Eval(expr.MapEnv{"level": expr.Num(3), "progress": expr.Num(0.5)})
```

- **`Library`** is an immutable set of functions. `With` returns a copy, so a host extends `Std()` without changing
  it for others. A host function must be pure and deterministic: no randomness, no clock, no I/O, and nothing that
  depends on state outside its arguments. Compile-time folding and shared programs rely on that.
- **`Scope`** declares the names a host offers. It is checked at compile time. `With` adds names, for example the
  params defined so far.
- **`Env`** gives the values at run time. `MapEnv` is a map. A host can implement `Env` to look values up lazily.
  `Program.Names()` lists the names an expression uses, so a host can compute only those.
- **Programs** are immutable and safe to evaluate from many goroutines at once. Compile once (when content loads or
  changes) and cache the program; spawning caches them per spawner.
- **`Convert`** and `ParseConversion` apply a type a content creator chose (`"number"`, `"integer"`, `"string"`,
  `"bool"`, or `""` for as is).

## Hosts

### Spawn params

A spawner's template can have params that set fields of its components when an entity spawns (see
[Spawning](architecture.md#spawning)). A param with the `expression` source evaluates an expression and converts the
result with its optional type.

- **Names.** Every expression sees these built-ins:

  | Name | Value |
  | --- | --- |
  | `hour` | the game hour, `0` to `24` (a game day lasts 2 hours) |
  | `count` | the spawner's live entities before this spawn |
  | `max` | the spawner's max count |
  | `seq` | this spawn's number (0, 1, 2, … for the spawner) |
  | `x`, `y` | where the entity spawns, in tiles |

  It also sees every param named before it: time of day, random and spawn count params, and other expressions. Param
  names follow the same rules as names, up to 32 characters, and can't reuse a built-in.
- **Order.** Params resolve in the order they're listed, so an expression can use only the params above it. Compiling
  checks this: a name defined below is unknown.
- **Values from lists.** A time of day, random or count step value goes through `Literal`, so `"2"` is the number 2,
  and `"01"` is the number 1 that is still written `01`.
- **Into a field.** A param with a field writes its value into that field of the template's component: `asset_id`,
  `scale` (models), and `asset_id`, `animation`, `facing`, `width` and `height` (sprites).
  - Every value suits a string field, in its text form (a list value as it was written).
  - A value of the wrong kind, or one the component refuses (an unknown facing, a size out of range), leaves the field
    as the template has it.
- **Failures.** A param whose expression fails at run time stays undefined. An expression that uses it fails in turn,
  and its field keeps the template's value; the rest of the params still apply. The server logs the first problem of
  each spawner once.
- **Limits.** 200 characters, 16 levels of nesting, 64 parts, and strings of at most 256 characters.

Example: a herb that glows at night and grows as more spawn around it.

| # | Name | Sets | Source |
| --- | --- | --- | --- |
| 1 | `phase` | (variable only) | time of day: 20 to 6 → `night`, 6 to 20 → `day` |
| 2 | `variant` | (variable only) | random: `1` (weight 3), `2` (weight 1) |
| 3 | | sprite `asset_id` | expression `"image/herb-" + phase + "-" + str(variant)` |
| 4 | | sprite `width` | expression `clamp(1 + count * 0.25, 1, 2)` |

## Tests

`server/internal/engine/expr/testdata/cases.json` holds the language's cases, shared by every implementation. A
TypeScript port on the client would run the same file, as `movement.json` is shared. Each case has:

- `src`: the expression, compiled with the standard library and a scope of `env`'s keys plus `scope`.
- `env`: the names' values, as JSON numbers, strings and bools.
- Either `want` (the expected value, which must match in kind too) or `error`: `"compile"` or `"eval"`.

To add a case, append it to `cases`; `TestSharedCases` runs them all. Behaviour outside a single expression (limits,
library extension, concurrent evaluation, compile-time folding) is tested in `expr_test.go`.
