# The language

The game has one language (`server/internal/engine/language`), a statically typed subset of TypeScript. It is used
in two forms:

- **Scripts** are whole programs: modules with classes, interfaces, functions and statements, which import each
  other.
- **Expressions** are single expressions, the part of the language that computes a value, embedded where content
  needs a small calculation.

This document describes the language first ([Language reference](#language-reference)), then how Go code embeds
it ([Embedding](#embedding-go)), then what the game does with it ([In the game](#in-the-game)): spawn params use
expressions, and `Script` components run scripts on zone objects.

## Design goals

- **Safe.** Code reaches only what its host gives it: no I/O, no clock, nothing global. An expression has no loops,
  so it finishes at once; a script loops and recurses, but every call from the host stops after a budget of steps,
  nested calls and memory (see [Budgets](#budgets)).
- **Deterministic.** The same inputs always give the same result. There is no random function: randomness comes in
  only as inputs the host draws from a seeded generator (`RANDOM_SEED`). Maps iterate in insertion order.
- **Strictly typed.** Operators accept only the types they're meant for, and the only implicit conversion is
  joining strings with `+`, so mistakes fail loudly instead of producing `NaN` or `"undefined"`. There is no
  `undefined` at all: every value starts at its type's [zero value](#zero-values).
- **Checked early.** Code is compiled when content is saved or uploaded, against the names, functions and modules
  its host offers. A typo is refused then, with where it is, not found when it runs.
- **Reusable.** Nothing in the language knows about the game. Hosts embed it with their own names, functions and
  modules: spawn params and script components today; quest conditions, world-event rules, loot tables or UI
  formulas later.

## Language reference

A first look, two modules of a script:

```ts
// script/items
export class Item {
  readonly id: string
  constructor(id: string) { this.id = id }
}
```

```ts
// script/bag
import { Item } from "script/items"

const MAX_STACK = 99

export interface Named {
  readonly name: string
}

// A bag of items, counted by their IDs.
export class Bag implements Named {
  readonly name: string
  private counts: Map<string, number> = new Map<string, number>()

  constructor(name: string) {
    this.name = name
  }

  add(item: Item, n: number): number {
    const now = min(this.counts.get(item.id) + n, MAX_STACK)  // a missing key reads as 0
    this.counts.set(item.id, now)
    return now
  }

  list(): string[] {
    let out: string[] = []
    for (const id of this.counts.keys()) {
      out.push(id + " ×" + str(this.counts.get(id)))
    }
    return out
  }
}
```

### Lexical structure

- **Numbers:** `3`, `0.25`, `.5`, `1.5e2`. An `_` between two digits groups them for reading and is otherwise
  ignored: `1_000_000`, `0.000_5`, `1e1_0`. It can't start or end a number, double up, or touch the `.` or `e`
  (`1_`, `1__0`, `1_.5` are refused).
- **Strings** in double or single quotes: `"night"`, `'it\'s'`. Escapes: `\"`, `\'`, `\\`, `\n`, `\t`, `\r`. A
  string ends on its line.
- **Names** start with a letter or `_`, followed by letters, digits and `_`.
- **Keywords** can't be names: `let`, `const`, `function`, `class`, `interface`, `type`, `extends`, `implements`,
  `new`, `this`, `super`, `return`, `if`, `else`, `while`, `for`, `break`, `continue`, `import`, `export`, `null`,
  `true`, `false`, `void`, `undefined`. Some words count only where the grammar expects them: `constructor`, `of`,
  `from`, `as`, `default`, `private`, `readonly`, `native`.
- **Comments:** `// to the end of the line` and `/* … */`.
- **Semicolons** are optional, as in TypeScript: a statement ends at a `;`, a line break, a `}` or the end.

### Types

| Type | Values | Zero value |
| --- | --- | --- |
| `number` | 64-bit floating point, always finite | `0` |
| `string` | text | `""` |
| `bool` (or `boolean`) | `true`, `false` | `false` |
| `T[]` (or `Array<T>`) | arrays of `T` | `null` |
| `Map<K, V>` | maps with `number` or `string` keys | `null` |
| a class | its objects and its subclasses' | `null` |
| an interface | objects of any class with its members | `null` |

- `void` is only a function's result: it returns nothing.
- `type Name = …` names a type: `type Names = string[]`.
- Classes are **nominal**: a `B` is no `A` unless it extends it. Interfaces are **structural**: whatever has the
  members fits (see [Interfaces](#interfaces)).

#### Zero values

There is no `undefined`. Every variable, field and array slot has a value from the start, its type's zero value
when nothing gave it one, as in Go:

- Any reference may be `null`; compare with `== null`. `null` alone has no type: `let a = null` is refused, while
  `let a: Item = null` is fine. `number`, `string` and `bool` are never null.
- A null array or map reads as empty, as Go's nil slices and maps do: `length` and `size` are 0, `for … of` runs
  no times, `get` gives the zero value, `indexOf` gives -1. Writing to one (`push`, `set`, `xs[0] = …`), or using a
  member of a null object, fails with where it happened.
- `m.get(k)` of a missing key gives the value type's zero value; `m.has(k)` tells whether it is there. An array
  index out of range, or not a whole number, fails.
- There are no optional fields or parameters, no `T | null` and no `!`.

### Expressions

```text
"image/slime-" + str(count % 3)              → "image/slime-1" when count is 4
hour >= 20 || hour < 6 ? "night" : "day"     → "night" at 22:30
clamp(1 + count * 0.25, 1, 2)                → 1.5 when count is 2
```

- **Literals:** numbers, strings, `true`, `false`, `null`, and arrays `[1, 2, 3]` (an empty one takes its type from
  where it goes: `let xs: number[] = []`).
- **Names:** variables, parameters and module constants.
- **Calls:** `f(a, b)` (a function of the module, an imported one or the standard library's), `x.m(a)` (a method),
  `super.m(a)` (the base class's method).
- **Members and elements:** `x.field`, `xs[i]`.
- **Objects:** `new Bag("pack")`, `new Map<string, number>()`.
- **`this`** in a class's code. Parentheses group.

Operators, from loosest to tightest binding:

| Precedence | Operators | Operands | Result |
| --- | --- | --- | --- |
| 1 | `c ? a : b` | `c` a bool; `a`, `b` of one type (or `null` and a reference) | `a` if `c`, else `b` (only that side is evaluated) |
| 2 | `\|\|` | bools | bool; the right side is skipped when the left is `true` |
| 3 | `&&` | bools | bool; the right side is skipped when the left is `false` |
| 4 | `==` `!=` (`===` `!==` alike) | two values of one type, or a reference and `null` | bool; objects, arrays and maps compare by identity |
| 5 | `<` `<=` `>` `>=` | two numbers, or two strings (compared byte by byte) | bool |
| 6 | `+` `-` | numbers; `+` with a string on either side and a number, string or bool on the other joins them | number or string |
| 7 | `*` `/` `%` | numbers | number (`%` keeps the sign of the left operand) |
| 8 | unary `-` `!` | `-` a number, `!` a bool | number or bool |
| 9 | `x.f`, `x.m(…)`, `x[i]`, `f(…)` | | |

Binary operators are left-associative (`10 - 4 - 3` is `3`). The ternary is right-associative
(`a ? 1 : b ? 2 : 3`). Operand types are checked at compile time: `1 - "x"` is refused before anything runs.
Division and `%` by zero, and results too large to be finite, fail when they run.

#### Text form and conversions

Joining with `+` writes each operand in its text form: numbers in the shortest form that reads back the same (`3`,
`0.25`, `150`), bools as `true` and `false`. So `"herb-" + 2` is `"herb-2"`.

The conversion functions, which take a number, string or bool:

| From \ to | `num` (number) | `int` (integer) | `str` (string) | `bool` |
| --- | --- | --- | --- | --- |
| number | itself | truncated toward zero: `int(-2.7)` is `-2` | text form | `false` for 0, otherwise `true` |
| string | parsed, spaces around it allowed: `num(" 2.5 ")` is `2.5`, `num("1_000")` is `1000`; fails on `"abc"` | parsed, then truncated | itself | `"true"` and `"false"` only; fails on anything else |
| bool | 1 or 0 | 1 or 0 | `"true"` or `"false"` | itself |

Hosts read plain text from content (a value in a list, a prop) as a **literal**: text that is a number (`1_000`
too) becomes a number, `true` and `false` become bools, and everything else stays a string. A literal number keeps the text it was
written as: `"01"` counts as 1 (`v == 1`, `v + 1` is 2) but is still written `01` by `str` and by `+`. Arithmetic
and `num` give a plain number, written in its shortest form.

### Statements

- **Variables:** `let x = 1`, `let x: number`, `const x = …`. A variable without a type takes its value's; one
  without a value needs a type and starts at its zero value. A `const` needs a value and can't change. A name
  declared twice in one block is refused; an inner block may hide an outer one.
- **Assignment:** `x = v`, `x += v` (and `-=`, `*=`, `/=`, `%=`), `x++`, `x--`, `++x`, `--x`, on variables,
  fields and array elements. Assignment is a statement, never an expression. `+=` on a string joins.
- **Control:** `if (c) { … } else if (d) { … } else { … }`, `while (c) { … }`, `for (let i = 0; i < n; i++) { … }`,
  `for (const x of xs) { … }` (over an array; over a map's `keys()` or `values()`), `break`, `continue`. Conditions
  are bools, and bodies are always blocks.
- **`return v`**, or `return` alone in a function that returns nothing. A `return` followed by a line break returns
  nothing.
- **Calls** stand as statements. Any other expression alone (`x == 1`) is refused as doing nothing.

### Functions

```ts
function grade(score: number): string {
  if (score >= 90) { return "a" }
  return score >= 50 ? "b" : "c"
}
```

- Parameters need types. Returning a value needs a result type (without one the function returns nothing), and a
  function with one must return on every path.
- Functions may call each other and recurse, within the [budgets](#budgets).
- There are no function values or closures yet.

### Classes

```ts
class Animal {
  name: string
  legs = 4                        // a number, string or bool value says its type
  private secret = 0              // only Animal's own code sees it
  readonly born: number           // set only in Animal's constructor
  constructor(name: string) { this.name = name; this.born = 1 }
  describe(): string { return this.name + " has " + str(this.legs) + " legs" }
}
class Bird extends Animal {
  constructor(name: string) { super(name); this.legs = 2 }   // super(…) comes first
  describe(): string { return "a bird: " + super.describe() }
}
const b: Animal = new Bird("Tweety")
b.describe()                      // "a bird: Tweety has 2 legs"
```

- **Fields** have a type, or a number, string or bool value that says it. A field without a value starts at its zero
  value. Fields' values are set when the constructor starts, right after `super(…)`.
- **`private`** members are seen only by their class's own code (on any object of the class). **`readonly`** fields
  are set only by their class's constructor.
- **Constructors:** at most one. A class without one takes its base's (or none). A subclass's constructor calls
  `super(…)` first.
- **Inheritance:** a class `extends` one class. Methods are virtual: `b.describe()` runs `Bird`'s. An override keeps
  its base's signature, and private methods can't be overridden.
- **`implements I, J`** checks the class against interfaces where it is declared.

### Interfaces

```ts
interface Named extends Other {
  readonly name: string
  greet(who: string): string
  hello?(): void
}
```

- Interfaces are structural: any class, or interface, with the members goes where the interface is wanted, with
  `implements` or not. Fields need the same type (a readonly interface field also takes a field that isn't
  readonly), methods the same signature, and private members don't count.
- An interface may extend others; it has their members too.
- An **optional method** (`hello?()`) may be left out. Code can't call one through the interface; only the host does,
  after checking it is there.

### Modules

A script is a set of modules, each one source file, named by an ID (in the game, a script asset's:
`script/<slug>`).

- **Imports** come first: `import { A, B as C } from "script/util"` imports the names another module exports,
  `import D from "script/door"` its default export, and `import D, { e } from …` both. Besides other modules, code
  imports the host's modules by their names (the game's is `"game"`).
- **Exports:** `export` goes before any declaration. `export default class` makes a class the default export.
- **Declarations** at the top of a module: classes, interfaces, types, functions and constants, in any order (they
  may refer to each other). A module has no variables: state lives in objects. Its constants are numbers, strings
  or bools, computed once when the script compiles, so everything that uses the script shares them safely.
- A module imported by several others is loaded once. Import cycles are refused, naming the cycle
  (`script/a → script/b → script/a`).

### Built-ins

The standard library, callable everywhere (in expressions it takes any number, string or bool where it wants one
and fails when it runs; in scripts the types are checked when they compile):

| Function | Arguments | Result |
| --- | --- | --- |
| `min(a, …)`, `max(a, …)` | one or more numbers | the smallest or largest |
| `floor(x)`, `ceil(x)`, `round(x)` | a number | rounded down, up, or to the nearest (halves away from zero) |
| `abs(x)` | a number | its magnitude |
| `clamp(x, lo, hi)` | three numbers | `x` kept within `lo..hi` |
| `num(v)`, `int(v)`, `str(v)`, `bool(v)` | a number, string or bool | converted (see [Text form and conversions](#text-form-and-conversions)); fails on a value that doesn't convert |
| `len(s)` | a string | its length in characters |
| `concat(v, …)` | one or more numbers, strings or bools | their text forms joined |

These names can't be declared again. Arrays, maps and strings have members:

| On | Members |
| --- | --- |
| `T[]` | `length`; `push(x, …)`, `pop()` (fails when empty), `slice(start?, end?)`, `indexOf(x)`, `includes(x)`, `join(sep)` (of numbers, strings or bools) |
| `Map<K, V>` | `size`; `get(k)`, `set(k, v)`, `has(k)`, `delete(k)`, `keys()`, `values()`; entries keep their insertion order |
| `string` | `length` (in characters); `slice(start?, end?)`, `indexOf(s)`, `includes(s)`, `startsWith(s)`, `endsWith(s)`, `toUpperCase()`, `toLowerCase()`, `trim()`, `split(sep)` |

`slice` takes negative positions from the end, as in TypeScript. `keys()`, `values()` and `slice` return new arrays.

### Budgets

Every call into a script (a constructor, a method the host calls) runs under limits, so code can't stall or exhaust
its server (`ScriptLimits`, defaults in `DefaultScriptLimits`):

| Limit | Default | Bounds |
| --- | --- | --- |
| `MaxSteps` | 100 000 | statements, loop turns and calls in one call (long built-ins count their length) |
| `MaxDepth` | 64 | calls nested in one another |
| `MaxLength` | 10 000 | elements of an array, entries of a map |
| `MaxString` | 4096 | bytes of a string |
| `MaxHeap` | 4 MB | bytes one call allocates, and the bytes an object keeps reachable between calls |
| `MaxSource` | 64 KB | one module's source |
| `MaxModules` | 64 | modules of a script |
| `MaxNesting` | 64 | nesting of blocks, expressions and types |

What an object keeps is measured once the calls since the last measure have allocated half of `MaxHeap`, so the
check costs little. Expressions have their own, smaller limits (see [Expressions on their own](#expressions-on-their-own)).

### Errors

- **Compile errors** say where the code is wrong: bad syntax, an unknown name, a type that doesn't fit, a function
  that doesn't return on every path, an import cycle. A script reports every problem it finds, up to 50, each as
  `module:line:col: message` (`script/door:3:14: unknown name opne`); lines and columns are 1-based, columns in
  bytes. Parsing stops at a module's first syntax error; checking goes on past type errors, and a part that failed
  doesn't report again where it is used.
- **Run-time errors** stop the call that hit them, with the module, line and column: division by zero, a null used,
  an index out of range, a failed conversion, a budget used up (`script/spin:5:12: ran more than 100000 steps`). The
  host decides what happens next.

### Expressions on their own

An expression is the expression part of the language, compiled on its own:

- **What it may use:** the literals `true`, `false`, numbers (with `_` separators) and double-quoted strings (escapes `\"` and `\\` only),
  the operators of levels 1 to 8, parentheses, the names its host declares, and calls of the functions in its host's
  library. Script syntax (`.`, `[`, `{`, `=`, `;`, single quotes) is refused with "not allowed in an expression";
  words that are keywords in scripts (`new`, `class`…) are names in expressions.
- **Types:** numbers, strings and bools. Names have no declared types, so operators check their operands when the
  expression runs.
- **Limits** (`Limits`; `DefaultLimits` allows 1024 characters, 32 levels of nesting and 256 parts): since nothing
  loops, evaluation visits each part at most once. `MaxString` (4096 by default) bounds every string it makes or
  reads.
- **Folding:** parts without names are computed once at compile time: `2 * 3 + str(1)` compiles to `"61"`. A
  constant part that fails, such as `1 / 0`, is kept and fails when evaluated.
- **Errors:** a compile error (`*language.Error`) carries its 1-based column; an evaluation error
  (`*language.EvalError`) matches `language.ErrEval`. Besides the run-time errors above, an operator or function
  given the wrong kind, and a name the host declared but had no value for, fail when evaluated.

## Embedding (Go)

### Expressions

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

- **`Library`** is an immutable set of functions. `With` returns a copy, so a host extends `Std()` without changing
  it for others. A host function must be pure and deterministic: no randomness, no clock, no I/O, and nothing that
  depends on state outside its arguments. Compile-time folding and shared programs rely on that.
- **`Scope`** declares the names a host offers, checked at compile time. `With` adds names, for example the params
  defined so far.
- **`Env`** gives the values at run time. `MapEnv` is a map; a host can implement `Env` to look values up lazily.
  `Program.Names()` lists the names an expression uses, so a host can compute only those.
- **Programs** are immutable and safe to evaluate from many goroutines at once. Compile once (when content loads or
  changes) and cache the program.
- **`Literal`** reads plain text as a value (see [Text form and conversions](#text-form-and-conversions)).
  **`Convert`** and `ParseConversion` apply a type a content creator chose (`"number"`, `"integer"`, `"string"`,
  `"bool"`, or `""` for as is).

### Scripts

```go
host := &language.HostModule{
	Name: "game",
	Source: `
export interface Behaviour { onUpdate?(ctx: Context, dt: number): void }
export native class Context { readonly zone: string; hour(): number }
export native function log(message: string): void`,
	Natives: map[string]language.NativeFunc{
		"log":          func(c *language.Call, _ language.Value, args []language.Value) (language.Value, error) { … },
		"Context.zone": func(c *language.Call, this language.Value, _ []language.Value) (language.Value, error) {
			return language.Str(this.Payload().(*myRunner).zone), nil
		},
		"Context.hour": …,
	},
}
script, diags := language.CompileScript("script/door", source, language.ScriptOptions{Hosts: []*language.HostModule{host}})
class, _ := script.DefaultClass()
behaviour, _ := host.Interface("Behaviour")
if why := class.Satisfies(behaviour); why != "" { … }
inst, err := script.New(class, runner)          // runner is what natives get as Call.Data()
_ = inst.Set("speed", language.Num(2))          // a public number, string or bool field
ctx, _ := host.Object("Context", runner)        // a native class's object, with the host's payload
_, err = inst.Call("onUpdate", ctx, language.Num(0.1))
```

- **Host modules** are how a host offers scripts its API. Each is a **definition file** in the language: only
  interfaces, types, and `native` classes and functions, with no bodies or constants; their code is Go (`Natives`,
  by `"f"`, `"C.method"` and `"C.field"`). Native fields are readonly; scripts can't make or extend a native class.
  A host module is checked once, imports nothing, and must give every native it declares. Tools treat it like a
  module: its declarations are where go to definition leads, and `Analyze(name, …)` reads it on its own (hover and
  the outline in it). New engine APIs (IO, networking…) arrive as more definition files.
- **`CompileScript`** loads the entry module and everything it imports from a **`Source`** (`MapSource` for tests),
  and returns the script or its `Diagnostic`s (`{Module, Line, Col, Msg}`, which carry what an editor's language
  server will need). `ModuleImports(src)` lists a module's imports without checking it, so a host can find what
  depends on what.
- **`*Script`** is immutable and safe to share between goroutines. `Script.Modules()` lists the modules it was
  compiled from; `Script.Call` calls an exported function.
- **`*Instance`** is an object the host keeps and calls into, under fresh budgets each call; it belongs to one
  goroutine at a time. `FieldKind` and `Set` set its public number, string and bool fields; `HasMethod` tells
  whether it has a method (an optional one, say). Run-time errors are `*language.ScriptError`, which matches
  `language.ErrEval`.

## Tooling

The language's tools (`analysis.go`, `complete.go`, `format.go`) answer questions about source that is being
written, for the [language server](architecture.md#language-server) behind the editor's code editor. They share the
checker with `CompileScript`, so they see exactly what compiling sees.

- **`Analyze(entry, src, opts)`** loads and checks a script like `CompileScript` (or, with a host module's name as
  the entry, that module's definition file on its own), but goes on past errors (a module
  that doesn't parse is left out, and the names imported from it go unchecked rather than unknown), and records
  every name occurrence as a `Ref`: its range, kind (local, parameter, field, method, function, class, interface,
  type, constant, module, built-in), its type or signature as hover shows it, its doc comment, its `Signature`
  (for calls), and its definition `Location`: in a script, or in a host module's definition file (none for the
  standard library and the built-in members). `At(module,
  offset)` finds the name at a position, `References(def)` every occurrence of what is declared there, and
  `Symbols(module)` the outline: classes with their fields, methods and constructor, interfaces, functions, types
  and constants.
- **Doc comments** are the `//` lines or `/* */` block right above a declaration (no blank line between), each on a
  line of its own, without their markers: `/** A bell. */`. A comment at the end of a line of code belongs to that
  code.
- **Import aliases** count as declared where they are imported: renaming `B` in `import { A as B }` renames the
  module's uses of `B`, and renaming `A` renames `A` in the import.
- **`Complete(module, src, opts, offset)`** puts a probe name in place of the word at the cursor (and, if that
  doesn't parse, ends the statement right after it, for `ctx.| log(…)`) and checks the module; the checker, meeting
  the probe, records what could go there: after a dot, the members the code may use (private ones only inside
  their class, no optional interface methods; arrays', maps' and strings' built-ins); for a name, the variables in
  scope, the module's constants, functions and classes, the standard library and keywords; for a type, the types.
  In an import's `{ }` it offers the module's exports, and in its quotes the module names the `Source` lists
  (`Lister`) and the host modules. Source that still doesn't parse gets the keywords that start declarations and
  statements.
- **`SignatureAt(module, src, opts, offset)`** finds the innermost `(` still open at the cursor, the function,
  method or constructor before it, and the argument the cursor is in (counting commas); an unfinished call is
  completed with the probe (and a `)`) so the rest of the module checks.
- **`Format(src)`** rewrites a module in one style: two spaces of indentation per open bracket, the line breaks as
  written (at most one blank line in a row), spaces around binary operators and after commas, colons and keywords,
  none inside brackets, around dots, after unary operators (unless the next is one too: `- -x` never becomes `--x`)
  or in `Map<K, V>`. Comments stay where they are; the
  tokens never change; formatting formatted source changes nothing. Source that doesn't lex is refused.
- **`ValidName(name)`** says whether a rename may use a name, and **`RenameConflict(def)`** why a field or method
  can't be renamed on its own: it overrides a base class's member or a subclass overrides it, it implements a
  member of an interface its class names in `implements` (the host's `Behaviour` too, whose methods the host calls
  by name), or, for an interface's member, a class implementing the interface has it, or another interface extends
  it. Renaming one of them would break the others; classes that satisfy an interface without naming it aren't seen.

## In the game

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
- **Values from lists.** A time of day, random or count step value is read as a literal, so `"2"` is the number 2,
  and `"01"` is the number 1 that is still written `01`.
- **Into a field.** A param with a field writes its value into that field of the template's component: `asset_id`,
  `scale` (models), and `asset_id`, `animation`, `facing`, `width` and `height` (sprites).
  - Every value suits a string field, in its text form (a list value as it was written).
  - A value of the wrong kind, or one the component refuses (an unknown facing, a size out of range), leaves the field
    as the template has it.
- **Errors.** An expression that doesn't compile is refused when the spawner is saved; the World answers `400` with
  the message, which the editor shows. A param whose expression fails at run time stays without a value; an
  expression that uses it fails in turn, and its field keeps the template's value. The rest of the params still
  apply, and the server logs the first problem of each spawner once.
- **Limits.** 200 characters, 16 levels of nesting, 64 parts, and strings of at most 256 characters.

Example: a herb that glows at night and grows as more spawn around it.

| # | Name | Sets | Source |
| --- | --- | --- | --- |
| 1 | `phase` | (variable only) | time of day: 20 to 6 → `night`, 6 to 20 → `day` |
| 2 | `variant` | (variable only) | random: `1` (weight 3), `2` (weight 1) |
| 3 | | sprite `asset_id` | expression `"image/herb-" + phase + "-" + str(variant)` |
| 4 | | sprite `width` | expression `clamp(1 + count * 0.25, 1, 2)` |

### Script components

A `Script` component runs a script on a zone object (`internal/game/scripting`; how servers load and run them is in
[Scripting](architecture.md#scripting)):

```ts
import { Behaviour, Context, log } from "game"

// A bell that rings every `every` seconds, louder at night.
export default class Bell implements Behaviour {
  every = 10            // a prop sets it: props {every: "5"}
  private waited = 0

  onStart(ctx: Context): void {
    log("a bell hangs at " + str(ctx.object.x) + ", " + str(ctx.object.y))
  }

  onUpdate(ctx: Context, dt: number): void {
    this.waited += dt
    if (this.waited < this.every) { return }
    this.waited = 0
    const night = ctx.hour() >= 20 || ctx.hour() < 6
    log(night ? "DONG" : "ding")
  }
}
```

- **Scripts are assets** (`script/<slug>`, an `.als` file; see [Assets](architecture.md#assets)), and a module's ID
  is its asset's. The asset service compiles a script when it is uploaded, against the `"game"` module and the
  scripts it imports, and refuses one that doesn't compile, or whose change would break a script that imports it,
  answering its diagnostics. The editor lists them.
- **The component** names the script (`asset_id`), the server that runs it (`scope`: `instance`, each instance its
  own copy; `fragment`, the zone's fragment; or `world`, the World) and its `props`.
- **The class.** The script's default export must be a class with a constructor taking no arguments that satisfies
  `Behaviour`.
- **Props** (at most 32, named like spawn params, values up to 256 characters) set the class's public number,
  string and bool fields of the same names, after its constructor and before `onStart`. Text goes into a string
  field as written; a number or bool field reads it as a literal. A prop without such a field, or whose text isn't
  of the field's kind, is logged and leaves the field as the class has it.
- **The `"game"` module** is the definition file `internal/game/scripting/game.d.als` (embedded in the servers);
  the code editor opens it, read-only, as `game.d.als`:

  ```ts
  export interface Behaviour {
    onStart?(ctx: Context): void               // once, when the object is made and its props are set
    onUpdate?(ctx: Context, dt: number): void  // ten times a second; dt in seconds
    onStop?(ctx: Context): void                // when the object, component or script changes or goes, or the server stops
  }
  export native class Context {
    readonly object: GameObject
    readonly zone: string
    readonly scope: string                     // "instance", "fragment" or "world"
    hour(): number                             // the game hour, 0 to 24
    now(): number                              // seconds since 1970
  }
  export native class GameObject { readonly id: string; readonly name: string; readonly x: number;
                                   readonly y: number; readonly h: number; readonly rotation: number }
  export native function log(message: string): void   // the server's log, at most 20 lines a second per script
  ```

  Scripts read the world but don't change it yet.
- **Budgets.** Each call (the constructor, `onStart`, one `onUpdate`, `onStop`) runs under the default
  [budgets](#budgets).
- **Failures.** A script that doesn't compile, isn't a `Behaviour` or whose call fails is stopped and logged once,
  without `onStop`. It starts again when its object, its component or any of its modules changes.

## Tests

`server/internal/engine/language/testdata/cases.json` holds the expression cases, shared by every implementation (a
TypeScript port on the client would run the same file, as `movement.json` is shared). Each case has:

- `src`: the expression, compiled with the standard library and a scope of `env`'s keys plus `scope`.
- `env`: the names' values, as JSON numbers, strings and bools.
- Either `want` (the expected value, which must match in kind too) or `error`: `"compile"` or `"eval"`.

To add a case, append it to `cases`; `TestSharedCases` runs them all. Behaviour outside a single expression (limits,
library extension, concurrent evaluation, compile-time folding) is tested in `expr_test.go`.

Scripts have their cases in `testdata/scripts/*.txt`, which `TestScriptCases` runs. A case starts with
`=== name`, has a section per module (`-- script/main --`, `-- script/util --`…), and says what to expect:

- `-- want --`: what `main()`, exported by `script/main`, returns, written as `"text"`, `3`, `true`, `[1, 2]`,
  `{"key": 1}` or a class name;
- `-- logs --`: the lines the test host module's `log` got (cases import `log`, `probe`, `Probe` and `Greeter`
  from `"test"`);
- `-- compile error --`: part of a diagnostic, usually `module:line:col: message`;
- `-- eval error --`: part of the error `main()` fails with.

Instances, props, memory, sharing a script between goroutines and host modules are tested in `script_test.go`.

The tools have cases of their own in the same format: `testdata/tools/*.txt` (`TestToolCases`), where `‸` in a
module marks the cursor and the sections say what to expect there: `-- hover --` (the detail, then the doc),
`-- definition --` (`module:line:col` or `none`), `-- completions --` (lines `+ label` that must be offered and
`- label` that mustn't), `-- signature --` (`label @ active parameter` or `none`), `-- references --`,
`-- symbols --` (the outline) and `-- diagnostics --`. `testdata/format/*.txt` (`TestFormatCases`) has `-- in --`
and `-- want --` (or `error`); every case also checks that formatting again changes nothing and keeps the tokens.
