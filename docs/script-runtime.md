# Script runtime

Scripts compile to WebAssembly: one module per script, every module it imports and the checked budgets included,
which Go servers run with wazero, Rust servers with wasmtime and the browser with its own engine. A host compiles once, when a script is saved or
uploaded; nothing compiles at run time.

## User perspective

Players get scripts that behave the same on servers and in their browser, at the speed of compiled code, and that
can't stall or exhaust a server: a script runs in a sandbox of its own and stops when it uses up its budget.

## Authoring perspective

Content creators never see the runtime: they write scripts (see the [language reference](language-reference.md)) and
their host compiles them. Hosts (a game's servers, the browser) offer scripts their API as host modules: definition
files in the language, such as a game's `"game"` and `"ui"`. New APIs (IO, networking…) arrive as more definition
files.

## Technical details

### Embedding (Go)

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
  interfaces, types, and `native` classes and functions, with no bodies or constants; their code is Go (`Natives`, by
  `"f"`, `"C.method"` and `"C.field"`). Native fields are readonly; scripts can't make or extend a native class. A host
  module is checked once, imports nothing, and must give every native it declares. Tools treat it like a module: its
  declarations are where go to definition leads, and `Analyze(name, …)` reads it on its own (hover and the outline in
  it).
- **Interfaces for other runtimes.** `HostModule.Interfaces()` describes every interface a host module exports
  (`Interface.Meta()`: its members in order, each method's or field's type as `Satisfies` compares it and as messages
  write it), as JSON for the runtimes without the checker: the Rust one checks a class against it by the same rules.
- **`CompileScript`** loads the entry module and everything it imports from a **`Source`** (`MapSource` for tests),
  and returns the script or its `Diagnostic`s (`{Module, Line, Col, Msg}`, which carry what an editor's language server
  needs). `ModuleImports(src)` lists a module's imports without checking it, so a host can find what depends on what.
- **`*Script`** is immutable and safe to share between goroutines. `Script.Modules()` lists the modules it was compiled
  from; `Script.Call` calls an exported function.
- **`*Instance`** is an object the host keeps and calls into, under fresh budgets each call, with a module instance of
  its own; it belongs to one goroutine at a time, and `Close` frees it. `FieldKind` and `Set` set its public number,
  string and bool fields; `HasMethod` tells whether it has a method (an optional one, say). Run-time errors are
  `*language.ScriptError`, which matches `language.ErrEval`.
- **Values across the boundary.** Numbers, strings and bools cross as themselves, arrays and maps as copies, and
  objects by reference: the host's native objects (`HostModule.Object`), which keep their identity in the script, and
  the script's objects, which the host holds by handle. A script's object the host got (a result, a native's argument)
  stays valid until its next call into the instance, unless it keeps it (`Instance.Keep`); `CallOn` calls one's
  methods.
- **Promises natives make.** A native declared to return a `Promise<T>` or `Promise<T, E>` makes one with
  `c.NewPromise()`, returns it, and keeps its `*language.Resolver` to settle it later: `Resolve(v)` with a value of
  `T`, or `Reject(e)` with an object of the error class `E` (a native class of its host module, `HostModule.Object`).
  Settling runs what waited for it (the scripts' code after their `await`s) as one call from the host, under fresh
  budgets, and returns its error like `Call`; so it must come from the goroutine that uses the instance, between its
  calls, never from inside a native. A promise settles once; settling it again, or after the instance closed, does
  nothing. An instance holds at most `MaxPendingPromises` (64) promises its natives made and the host hasn't settled:
  `NewPromise` fails past that. An async method the host calls returns a promise the host doesn't see (`Call`
  answers nothing for it).
- **Compiled modules.** `Script.Module()` is the script compiled to WebAssembly, its metadata included; the host
  stores it. `LoadScript(module, opts)` loads one against the host modules it was compiled with and makes its
  machine code, and refuses a module of another runtime version, or compiled against another definition file of a host
  module (it records each one's hash), or whose natives the host doesn't give. `CompileScript` makes no machine code (it
  checks the constants in wazero's interpreter); a script makes it when first instantiated. `Script.Close` frees it once
  the script's last instance is closed.

### Embedding (TypeScript)

The browser runs compiled modules with `@krotovic/kiln` (`js/src`), the same way:

```ts
import { hostObject, loadScript, NativeObject, type HostModule } from "@krotovic/kiln";

const ui: HostModule = {                // a host module of a game's UIs
  name: "ui",
  source: uiDefinitionFile,             // optional: checked against what the script was compiled with
  natives: {
    log: (call, _self, [line]) => console.log(line),
    "Node.text": (_call, self) => (self as NativeObject).payload as string,
  },
};
const script = await loadScript(await fetch(moduleUrl), { hosts: [ui] });
const inst = script.new("default", data);   // the default class, data for natives (call.data)
inst.set("speed", 2);
inst.call("onUpdate", hostObject(ui, "Context", ctx), 0.1);
```

- `loadScript` compiles the module once (`WebAssembly.compileStreaming` for a fetch `Response`) and checks its metadata
  against the host modules, as `LoadScript` does. `Script.new` and `Script.call` instantiate it.
- `Instance` has the Go instance's methods: `call`, `callOn`, `hasMethod`, `hasMethodOn`, `fieldKind`, `set`, `get`,
  `keep`, `selfObject`. Values are `number`, `string`, `boolean`, `null`, arrays, `Map`s, `ScriptObject`s,
  `NativeObject`s (`hostObject`) and `ScriptPromise`s.
- A native that returns a promise makes it with `call.newPromise()`, which gives the `ScriptPromise` to return and a
  `Resolver` (`resolve(v)`, `reject(nativeObject)`, `pending`), settled later from outside the script's calls (a
  network answer's callback, never the native itself), as in Go; `MAX_PENDING_PROMISES` is 64. A native that throws fails the script's call with `name: message`; errors are
  `ScriptError {module, line, col, msg}`.
- What the host does for a script it does as Go does (`go.ts`): numbers written in their shortest form (Go's
  `'f', -1`), read as `strconv.ParseFloat` reads them, `%` of fractions, and strings quoted as `%q` quotes them in
  messages. Everything else (Unicode, maps, the collector) is in the module itself.

### Embedding (Rust)

Rust servers run compiled modules with the crate `kiln` (`rust/`, from this repository as a Git dependency), on
wasmtime, the same way. Rust has no compiler: modules come from the Go one (the asset service, say).

```rust
use kiln::{HostModule, Interface, Value, load_script};

let game = HostModule::new("game", GAME_DEFINITION_FILE)     // the definition file the script was compiled against
    .native("log", |call, _this, args| { … ; Ok(Value::default()) })
    .native("Context.zone", |_call, this, _args| Ok(Value::str(this.payload::<Runner>().unwrap().zone.as_str())));
let script = load_script(&module_bytes, &[&game])?;          // refuses a module of another runtime or definition file
let class = script.default_class().unwrap();
let behaviour = &Interface::parse_all(GAME_INTERFACES_JSON)?["Behaviour"]; // HostModule.Interfaces() from Go
if let Err(why) = class.satisfies(behaviour) { … }         // why it isn't a Behaviour
let mut inst = script.new_instance(&class, runner)?;        // runner is what natives get (call.data::<T>())
inst.set("speed", Value::num(2.0))?;
let ctx = game.object("Context", runner_ref);                // a native class's object, with the host's payload
inst.call("onUpdate", &[ctx, Value::num(0.1)])?;
```

- `load_script` checks the metadata as `LoadScript` does (the runtime version, each host module's hash, every
  native given) and compiles the module once, with one wasmtime engine for the process; `Script` is cheap to clone
  and shared between threads. Each `Instance` is a wasmtime store of its own, and moves between threads (`Send`).
- `Instance` has the Go instance's methods: `call`, `call_on`, `has_method`, `has_method_on`, `field_kind`, `set`,
  `get`, `keep`, `self_object`, and `call_function` on an instance of no object (`Script::instantiate`), which keeps
  the instance for the promises its functions wait on (`Script::call` makes one for one call). `data` and `data_mut`
  reach what the host gave it.
- `Value` is opaque, like Go's: `Value::num`, `str`, `bool`, `null`, `array`, `map`, `literal`, and `as_number`,
  `as_str`, `as_bool`, `as_array`, `as_map`, `payload::<T>()` (a host object's), `kind`. Host objects keep their
  identity (clones are the same object); script objects are held by handle, as in Go.
- Natives are closures `Fn(&mut Call, &Value, &[Value]) -> NativeResult`, which may fail with any error: a
  `ScriptError` passes as it is, anything else fails the call as `member: error`. `call.new_promise()` gives the
  promise to return and a `Resolver`, settled later with `resolver.resolve(&mut inst, v)` or
  `reject(&mut inst, e)` between the instance's calls; at most `MAX_PENDING_PROMISES` (64) wait at a time.
- Errors are `kiln::Error`: `Script(ScriptError {module, line, col, msg})` for the script's, `Host(msg)` for the
  host's. What the host does for a script it does as Go does (`kiln::gofmt`): numbers in their shortest form (ties
  broken to even, as Go's `'f', -1` does), read as `strconv.ParseFloat` reads them, strings quoted as `%q` quotes
  them.

### Compiling to WebAssembly

`CompileScript` checks a script, turns each function into a typed IR (`ir.go`: names already resolved to variables'
slots, fields, constants and the functions calls run) and compiles the whole script, every module it imports included,
into one WebAssembly module (`gen.go`, `gen_func.go`, `gen_builtins.go`), with only the runtime functions it uses
(`runtime.wat`, assembled by `wasm`, which writes modules in the binary format and assembles a subset of
the text format). The same sources always compile to the same bytes. `abi.go` is the interface between a module and
its hosts (`host.go` in Go, `js/src/script.ts` in the browser); a change to it is a new `abiVersion`.

- **Values.** Numbers are `f64`, bools `i32`, and strings, arrays, maps and objects `i32` addresses in the module's
  memory, 0 for null.
- **Memory.** Address 0 is null; 64 scratch slots of 8 bytes carry arguments and results between the script and its
  host; static data follows (string literals, the classes' tables, Go's Unicode case table when the script changes
  case), then the heap. Every block on the heap has a 16-byte header: its tag (the collector's mark in the high bit),
  its size, a forwarding address, and a word of its own (a string's length in bytes, an array's length, a map's
  count). Strings are UTF-8; arrays keep their elements in a store of 8-byte slots; maps keep their entries in
  insertion order with an open-addressing index (FNV-1a over strings, the number's bits, `-0` as `0`); an object's
  fields are 8-byte slots; a native object holds the host's handle.
- **The collector** is a sliding mark-compact, run only between two calls from the host, so its roots are the handles
  the host holds (the instance's object, what it kept, what the last call handed out) and the module's constants, never
  the stack. It runs once the calls since the last collection allocated half of `MaxHeap`, and its live bytes are what
  "keeps more than `MaxHeap`" measures. Native objects are held weakly: one the script let go of is dropped, and the
  host told (`release`). Memory never shrinks, and grows to at most a little over three and a half times `MaxHeap`.
- **Async functions** (`gen_async.go`) compile to state machines. Calling one allocates its frame, an object of a
  hidden class that holds its variables (and, for a `for … of` that awaits, the loop's array, store and index), and its
  promise, another hidden class's object (its state, its value, its error, the first frame waiting on it); it runs the
  function's resume function, which restores the variables from the frame and runs the body. An `await` whose promise
  is pending saves the variables in the frame, notes which await it is (they are numbered in order), puts the frame
  on the promise's waiters and returns. Settling a promise queues its waiters, in the order they came; the host's
  `__promise_resolve_*` and `__promise_reject` settle a promise it made and resume the queued frames until none is
  left (`$promise_drain`, through the table's `resume` row: a frame's class names its resume function). A resumed
  frame goes through the body again, skipping every statement while it looks for its await: a statement holding no
  await runs only when the function isn't resuming, one holding awaits (their numbers make a range) is entered only
  when it holds the one resumed at, an `if` takes the branch holding it without its condition, and a loop skips its
  header and condition to reach it. Because awaits are whole statements, nothing waits in the middle of an
  expression. The collector traces frames and promises like any object; what a pending promise waits for is reachable
  from the host's handle of the promise it made, so nothing waiting is collected.
- **Calls.** Functions are WebAssembly functions, methods take the object first. A method call on a class that no class
  derived from it overrides is a direct call; the others, and calls and field reads through interfaces, go through one
  function table, a row per name and signature with an entry per class.
- **Budgets** are compiled in, from `ScriptLimits`: every statement and loop turn counts a step where it starts, every
  call one more and a level of nesting, built-ins their length; allocations count against the call's bytes, lengths
  and string sizes are checked where they grow. The host resets them per call (`__begin`, `__end`).
- **Errors.** A run-time error writes its arguments into the scratch slots and calls the import `fail` with its code
  and position, then traps. The module's metadata has the messages by code and the positions (module, line, column), so
  both hosts word errors alike.
- **Imports** (module `alo`): `fail`, `native` (a native by number, with `this` and its arguments in the scratch slots
  and its result in slot 0), `fmt_num` (numbers that aren't whole or are past 2^53; the rest are written in the
  module), `parse_num`, `fmod` (`%` of fractions; whole numbers are worked out in the module) and `release`. Hosts put
  strings into the module through the export `__str_new`. Every script imports the same functions, so one wazero
  runtime serves them all.
- **Exports:** `memory`, `__init` (the module's constants, computed when it is instantiated; `CompileScript` runs it
  once, so a constant that fails is a compile error), `__begin`, `__end`, `__gc`, the runtime's helpers for hosts
  (`__str_new`, `__arr_new`, `__map_new`, `__map_put`, the handles' `__handle_new`, `__handle_get`, `__handle_drop`,
  `__native_new`, `__native_get`, and in a script that makes promises `__promise_new`, `__promise_resolve_f`,
  `__promise_resolve_i` and `__promise_reject`), and for the script: `n<class>` (a new object), `c<class>` (its constructor),
  `m<class>.<method>` and `f.<function>`.
- **Metadata,** in the custom section `alo.script` (JSON): the runtime version, the limits, the modules, the messages
  and positions, every class (its fields with their offsets, its methods with their exports and signatures), the
  natives it calls, the host modules with a hash of their definition files, the entry's exported functions and
  classes.

### Tests

- `language`: the script cases of `testdata/scripts` (see
  [the language's tests](language-reference.md#tests)), every one compiled to WebAssembly and run by wazero; instances
  (the default class, `Satisfies`, `New`, fields set by kind, private fields not settable, `HasMethod`, calls with typed
  arguments); an instance's kept memory bounded; one script shared by many goroutines; diagnostics collected across
  functions; `ModuleImports`; a host module missing a native (`script_test.go`). The compiled runtime
  (`compile_test.go`): the collector freeing garbage and cycles over many calls while what the object keeps survives;
  kept objects surviving collections, objects not kept refused after the next call; natives taking and giving host
  objects (the same object staying the same), arrays and maps; host objects the script let go of released; arrays and
  maps as method arguments and results; numbers and strings as Go has them (`-0`, `%` of fractions and of whole
  numbers, numbers past 2^53 and below 1, `num` with `_` and hex, `int`, `round`, case mapping with Go's rules, Unicode
  spaces, runes in `len`, `slice` and `indexOf`, `split` on nothing); host strings with invalid UTF-8 read as Go reads
  them; dispatch through interfaces and overrides, fields through interfaces; a constant that fails being a compile
  error; a helper reporting its own trap, not an earlier call's error; a closed script making no instances while its
  instances keep working, and its machine code freed with the last; `LoadScript` running a module, refusing one without
  its host module, against a changed host module, of another runtime version or not a module; the same sources
  compiling to the same bytes; errors naming their module, line and column. Promises (`script_test.go`): frames waiting
  across many collections keeping their strings and loops' arrays; resolvers refusing a value of the wrong type, a
  rejection of a promise that can't fail and one with another class, settling once, and doing nothing after the
  instance closed. `HostModule.Interfaces` describing interfaces as the other runtimes read them. `bench_test.go`
  benchmarks a loop, method calls and strings.
- `wasm`: LEB128; a module written with the builder and the assembler (loops, labels, ifs with results,
  globals, loads and stores, the table, data, a custom section) validated and run by wazero, a host function calling
  back into the module; assembler errors (unknown locals, labels, instructions, functions and types, a function defined
  twice).
- **The browser's and the Rust runtime run the same cases:** `TestWriteRuntimeFixtures` (only with `KILN_FIXTURES`
  set; `npm test` sets it through `js/scripts/fixtures.mjs`, `cargo test` from `rust/tests/runtime.rs`) compiles every
  case of `testdata/scripts` that runs, and a few scripts for instances and for `Satisfies`, into the directory given
  (`js/src/fixtures/`, or Cargo's test directory; not committed), with how Go formats, parses and quotes a corpus of
  numbers and strings, the test host modules' interfaces (`HostModule.Interfaces`) and what `Satisfies` says of each
  class. So `npm test` and `cargo test` need Go.
- `js/src/script.test.ts`: the browser's runtime against the Go compiler's modules: every shared case
  of `language/testdata/scripts` that runs (results, logs, errors); numbers written, read and
  strings quoted exactly as Go does, over a corpus Go wrote; an instance with props, kinds, typed calls and its host's
  natives; a module refused against a changed host module or without one; the collector keeping what is held and
  refusing what wasn't kept, and "keeps more than" past the heap budget; natives with objects, arrays and maps,
  released host objects; a timing bound. The async cases run with natives whose promises the test settles after
  `main()`, in order, as Go's harness does.
- `rust/tests/runtime.rs`: the Rust runtime against the same fixtures and checks as the browser's, and: classes
  checked against interfaces with what Go's `Satisfies` said of each (missing, private, field for method, other
  signatures, async for an optional void method, readonly and field types); resolvers refusing a value of the wrong
  type, a rejection of a promise that can't fail and one with another class, settling once, and another instance's
  resolver; errors naming their module, line and column; an instance moved to another thread. `src/gofmt.rs` has
  unit tests of its own (exact ties, Go's number syntax).
