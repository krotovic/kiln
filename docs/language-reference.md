# The language

Kiln (`language/`) is a statically typed subset of TypeScript for embedding in hosts. It is used in two forms:

- **Scripts** are whole programs: modules with classes, interfaces, functions and statements, which import each other.
- **Expressions** are single expressions, the part of the language that computes a value, embedded where content needs
  a small calculation (see [Expressions](expressions.md)).

Scripts compile to WebAssembly: one module per script, which servers run with wazero (Go) or wasmtime (Rust) and the browser with its own
engine, so a script gives the same results everywhere (see [Script runtime](script-runtime.md)).

## User perspective

The people a host serves (the players of a game, say) never write the language; they meet what it does. What they
can count on comes from its design goals:

- **Safe.** Code reaches only what its host gives it: no I/O, no clock, nothing global. An expression has no loops, so
  it finishes at once; a script loops and recurses, but every call from the host stops after a budget of steps, nested
  calls and memory (see [Budgets](#budgets)). A broken script stops on its own and the host goes on.
- **Deterministic.** The same inputs always give the same result. The language has no random function: randomness
  comes in only from the host, drawn from a seeded generator the host offers, such as a `rand()`. Maps iterate
  in insertion order.
- **Fast, the same everywhere.** A script compiles to one WebAssembly module, checked budgets included, which runs on
  servers and in the browser alike, at the speed of compiled code.

## Authoring perspective

Content creators write scripts and expressions against what their host offers. More of the design goals:

- **Strictly typed.** Operators accept only the types they're meant for, and the only implicit conversion is joining
  strings with `+`, so mistakes fail loudly instead of producing `NaN` or `"undefined"`. There is no `undefined` at
  all: every value starts at its type's [zero value](#zero-values).
- **Checked early.** Code is compiled when content is saved or uploaded, against the names, functions and modules its
  host offers. A typo is refused then, with where it is, not found when it runs.
- **Reusable.** Nothing in the language knows about its hosts. Hosts embed it with their own names, functions and
  modules: in a game, say, object behaviours, spawn rules, UI scripts and computed fields of templates.

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

- **Numbers:** `3`, `0.25`, `.5`, `1.5e2`. An `_` between two digits groups them for reading and is otherwise ignored:
  `1_000_000`, `0.000_5`, `1e1_0`. It can't start or end a number, double up, or touch the `.` or `e` (`1_`, `1__0`,
  `1_.5` are refused).
- **Strings** in double or single quotes: `"night"`, `'it\'s'`. Escapes: `\"`, `\'`, `\\`, `\n`, `\t`, `\r`. A string
  ends on its line.
- **Names** start with a letter or `_`, followed by letters, digits and `_`.
- **Keywords** can't be names: `let`, `const`, `function`, `class`, `interface`, `type`, `extends`, `implements`,
  `new`, `this`, `super`, `return`, `if`, `else`, `while`, `for`, `break`, `continue`, `import`, `export`, `null`,
  `true`, `false`, `void`, `undefined`, `await`. Some words count only where the grammar expects them: `constructor`,
  `of`, `from`, `as`, `default`, `private`, `readonly`, `native`, `async` (before `function` or a method's name) and
  `error` (before `class`).
- **Comments:** `// to the end of the line` and `/* … */`.
- **Semicolons** are optional, as in TypeScript: a statement ends at a `;`, a line break, a `}` or the end. A `[` that
  starts a line starts a statement (`[v, err] = await p`), never an index into the line before.

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
| `Promise<T>`, `Promise<T, E>` | what an async function or the host settles later (see [Async functions](#async-functions-and-promises)) | `null` |

- `void` is only a function's result: it returns nothing.
- `type Name = …` names a type: `type Names = string[]`.
- Classes are **nominal**: a `B` is no `A` unless it extends it. Interfaces are **structural**: whatever has the members
  fits (see [Interfaces](#interfaces)).

#### Zero values

There is no `undefined`. Every variable, field and array slot has a value from the start, its type's zero value when
nothing gave it one, as in Go:

- Any reference may be `null`; compare with `== null`. `null` alone has no type: `let a = null` is refused, while
  `let a: Item = null` is fine. `number`, `string` and `bool` are never null.
- A null array or map reads as empty, as Go's nil slices and maps do: `length` and `size` are 0, `for … of` runs no
  times, `get` gives the zero value, `indexOf` gives -1. Writing to one (`push`, `set`, `xs[0] = …`), or using a member
  of a null object, fails with where it happened.
- `m.get(k)` of a missing key gives the value type's zero value; `m.has(k)` tells whether it is there. An array index
  out of range, or not a whole number, fails.
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

Binary operators are left-associative (`10 - 4 - 3` is `3`). The ternary is right-associative (`a ? 1 : b ? 2 : 3`).
Operand types are checked at compile time: `1 - "x"` is refused before anything runs. Division and `%` by zero, and
results too large to be finite, fail when they run.

#### Text form and conversions

Joining with `+` writes each operand in its text form: numbers in the shortest form that reads back the same (`3`,
`0.25`, `150`), bools as `true` and `false`. So `"herb-" + 2` is `"herb-2"`.

The conversion functions, which take a number, string or bool:

| From \ to | `num` (number) | `int` (integer) | `str` (string) | `bool` |
| --- | --- | --- | --- | --- |
| number | itself | truncated toward zero: `int(-2.7)` is `-2` | text form | `false` for 0, otherwise `true` |
| string | parsed, spaces around it allowed: `num(" 2.5 ")` is `2.5`, `num("1_000")` is `1000`; fails on `"abc"` | parsed, then truncated | itself | `"true"` and `"false"` only; fails on anything else |
| bool | 1 or 0 | 1 or 0 | `"true"` or `"false"` | itself |

Hosts read plain text from content (a value in a list, a prop) as a **literal**: text that is a number (`1_000` too)
becomes a number, `true` and `false` become bools, and everything else stays a string. A literal number keeps the text
it was written as: `"01"` counts as 1 (`v == 1`, `v + 1` is 2) but is still written `01` by `str` and by `+`.
Arithmetic and `num` give a plain number, written in its shortest form.

### Statements

- **Variables:** `let x = 1`, `let x: number`, `const x = …`. A variable without a type takes its value's; one without
  a value needs a type and starts at its zero value. A `const` needs a value and can't change. A name declared twice in
  one block is refused; an inner block may hide an outer one.
- **Assignment:** `x = v`, `x += v` (and `-=`, `*=`, `/=`, `%=`), `x++`, `x--`, `++x`, `--x`, on variables, fields and
  array elements. Assignment is a statement, never an expression. `+=` on a string joins.
- **Control:** `if (c) { … } else if (d) { … } else { … }`, `while (c) { … }`, `for (let i = 0; i < n; i++) { … }`,
  `for (const x of xs) { … }` (over an array; over a map's `keys()` or `values()`), `break`, `continue`. Conditions are
  bools, and bodies are always blocks.
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

### Async functions and promises

What the host has to fetch (an inventory the World keeps, say) comes back later. A function that waits for it is
`async`, and returns a promise its body settles:

```ts
import { Behaviour, Context, ActionEvent, InventoryError, trade, log } from "game"

error class ShopError {
  code: string
  constructor(code: string) { this.code = code }
}

// What the player can afford: a promise that fails with a ShopError
async function afford(ctx: Context, event: ActionEvent, price: number): Promise<number, ShopError> {
  const [gold, err] = await event.player.inventory("").amount("gold")
  if (err != null) { return new ShopError(err.code) }        // an error rejects the promise
  if (gold < price) { return new ShopError("poor") }
  return gold                                                // a value fulfils it
}

export default class Shop implements Behaviour {
  async onAction(ctx: Context, event: ActionEvent): Promise<void> {
    const [gold, err] = await afford(ctx, event, 10)
    if (err != null) { log("can't buy: " + err.code); return }
    const [done, failed] = await ctx.object.exchange(event.player, trade().pay("gold", 10).receive("iron-sword", 1))
    log(failed == null ? "sold" : "refused: " + failed.code)
  }
}
```

- **`Promise<T>`** can't fail; **`Promise<T, E>`** fails with an `E`, which must be an **error class**. `T` may be
  `void`, and can't be an error class, so what a `return` settles is always clear.
- **Error classes** are declared `error class Name { … }` (`export error class …`; a host module's
  `export native error class …`). They are classes in every other way. An error class extends only error classes, and
  a plain class can't extend one; a `Promise<T, Base>` fails with a `Base` or any of its subclasses. Interfaces and
  plain classes can't be a promise's error type, and there is no built-in error class.
- **An `async` function or method** returns a `Promise<T>` or `Promise<T, E>`. In its body, `return x` (a `T`)
  fulfils the promise, `return e` (an `E`) rejects it, and reaching the end (or `return` alone) fulfils it with the zero
  value. Constructors aren't async. Calling an async function runs it at once, up to its first `await` that has to
  wait, and gives its promise; without an `await` on it, it goes on by itself.
- **`await p`** is a whole statement, only in an async function, and only in these forms:

  | Form | For | Gives |
  | --- | --- | --- |
  | `const x = await p`, `let x = await p`, `x = await p` | a `Promise<T>` | the value |
  | `const [v, err] = await p`, `let [v, err] = await p`, `[v, err] = await p` | a `Promise<T, E>` | the value and the error: on success `err` is `null`, on failure `v` is `T`'s zero value |
  | `await p` | either | nothing: it only waits |
  | `return await p` | a promise whose value fits the function's, and whose error does | settles the function's promise as `p` settled |

  `_` in place of a name drops that part (`const [_, err] = await p`). `await` can't be part of a larger expression
  (`f(await p)`), nor in a `for` loop's header: put what it gives in a variable first. An assignment's target
  (`this.box.n`) is found once the promise settled. The `[a, b]` form takes apart only what an await gives; there are
  no tuples otherwise.
- **Errors are values.** There is no `try`/`catch` and no exception: a failure the code can handle is a promise's
  error. A bug (a budget used up, a null used, a failed call) still fails the call it happens in, as everywhere, and is
  never turned into an `E`.
- **Types.** A `Promise<T>` goes where a `Promise<T, E>` of the same `T` is wanted (a promise that can't fail fits
  where one may), not the other way. Promises can be kept in variables and fields and awaited more than once; one that
  settled already gives what it settled with at once.
- **The host's callbacks.** An async method may stand for an optional method of an interface that returns nothing
  (`async onAction(…): Promise<void>` for `Behaviour`'s `onAction?(…): void`): the host calls it and goes on; what
  comes after each `await` runs when the promise settles, under a fresh [budget](#budgets). A host that ignores a
  callback's promise ignores its error too, so such a method handles its own errors.
- There is no `.then`, `Promise.all` or `Promise.race` yet: the language has no function values.

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
  `implements` or not. Fields need the same type (a readonly interface field also takes a field that isn't readonly),
  methods the same signature, and private members don't count.
- An interface may extend others; it has their members too.
- An **optional method** (`hello?()`) may be left out. Code can't call one through the interface; only the host does,
  after checking it is there.

### Modules

A script is a set of modules, each one source file, named by an ID (the host chooses them, such as `script/main`).

- **Imports** come first: `import { A, B as C } from "script/util"` imports the names another module exports,
  `import D from "script/door"` its default export, and `import D, { e } from …` both. Besides other modules, code
  imports the host's modules by their names (the examples here use a game's `"game"`).
- **Exports:** `export` goes before any declaration. `export default class` makes a class the default export.
- **Declarations** at the top of a module: classes, interfaces, types, functions and constants, in any order (they may
  refer to each other). A module has no variables: state lives in objects. Its constants are numbers, strings or bools,
  computed once when the script compiles, so everything that uses the script shares them safely.
- A module imported by several others is loaded once. Import cycles are refused, naming the cycle
  (`script/a → script/b → script/a`).

### Built-ins

The standard library, callable everywhere (in expressions it takes any number, string or bool where it wants one and
fails when it runs; in scripts the types are checked when they compile):

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
| `MaxHeap` | 4 MB | bytes one call allocates, and the bytes an object keeps reachable between calls (in the module's memory: blocks with their headers) |
| `MaxSource` | 64 KB | one module's source |
| `MaxModules` | 64 | modules of a script |
| `MaxNesting` | 64 | nesting of blocks, expressions and types |

What an object keeps is measured once the calls since the last measure have allocated half of `MaxHeap`: the
collector runs then, between two calls, and what is left is what the object (and what the host keeps) holds, so the
check costs little. Expressions have their own, smaller limits (see [Expressions](expressions.md)).

### Errors

- **Compile errors** say where the code is wrong: bad syntax, an unknown name, a type that doesn't fit, a function that
  doesn't return on every path, an import cycle. A script reports every problem it finds, up to 50, each as
  `module:line:col: message` (`script/door:3:14: unknown name opne`); lines and columns are 1-based, columns in bytes.
  Parsing stops at a module's first syntax error; checking goes on past type errors, and a part that failed doesn't
  report again where it is used.
- **Run-time errors** stop the call that hit them, with the module, line and column: division by zero, a null used, an
  index out of range, a failed conversion, a budget used up (`script/spin:5:12: ran more than 100000 steps`). The host
  decides what happens next.

## Technical details

How the language is embedded in Go and TypeScript, and how scripts compile to WebAssembly, is in
[Script runtime](script-runtime.md); and expressions on their own in [Expressions](expressions.md).

### Tests

Scripts have their cases in `language/testdata/scripts/*.txt`, which `TestScriptCases` runs. A
case starts with `=== name`, has a section per module (`-- script/main --`, `-- script/util --`…), and says what to
expect:

- `-- want --`: what `main()`, exported by `script/main`, returns, written as `"text"`, `3`, `true`, `[1, 2]`,
  `{"key": 1}` or a class name;
- `-- logs --`: the lines the test host module's `log` got (cases import `log`, `probe`, `Probe`, `Greeter`, `Hooks`,
  the error class `ProbeError`, and `later`, `laterText` and `failLater`, whose promises the harness settles after
  `main()` returns, in the order they were made);
- `-- compile error --`: part of a diagnostic, usually `module:line:col: message`;
- `-- eval error --`: part of the error `main()` fails with.

The cases cover an array element set in the array as the value being assigned left it; statements and operators typed
at compile time, optional semicolons, comments and escapes, recursion, the standard library, module constants; classes
with constructors, field values, inheritance, `super`, overrides keeping their signature, private and readonly members,
nominal classes; structural interfaces, `implements`, interface inheritance, optional methods only the host calls;
aliases; zero values of every type, null arrays and maps reading as empty and refusing writes, missing keys, no
`undefined`; arrays, maps (insertion order) and strings with their methods; numbers and strings as Go has them (`-0`,
`%` of fractions, numbers past 2^53 and below 1, `num` with `_` and hex, case mapping with Go's rules, runes); imports, aliases, default imports, a module
imported twice, classes extending another module's, unknown exports and modules, import cycles, a module's errors
naming it, host modules and native classes, natives refused outside them; the step, depth, length and string budgets;
async functions (`async.txt`): promises settled at once and later, in order, errors passed up as values through script
and host error classes and their subclasses, loops and branches keeping their variables across awaits (`for … of` with
`continue` and `break`, `while`), async methods keeping `this`, `return await`, assignments to fields from an await, a
promise awaited twice and after it settled, `Promise<T>` going where `Promise<T, E>` is wanted, an async method standing
for an optional host callback, failures after an await, `await` of null, the 64 promises a host may hold, and every
misuse refused when it compiles (await outside async functions, inside an expression or a `for` header, a failing
promise not taken apart and one that can't fail taken apart, error types that aren't error classes and values that
are, error classes extending plain classes and the reverse, returns of the wrong value or error, async methods for a
callback that isn't optional).
Every one is compiled to WebAssembly and run by wazero, and the browser's and the Rust runtime run them too (see
[Script runtime](script-runtime.md#tests)). The language keeps its cases in `testdata/` so every implementation is
tested against them.
