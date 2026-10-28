# The language

The game has one language (`server/internal/engine/language`), a statically typed subset of TypeScript. It is used in
two forms:

- **Scripts** are whole programs: modules with classes, interfaces, functions and statements, which import each other.
  Scripts compile to WebAssembly: one module per script, which the servers run with wazero and the browser with its own
  engine, so a script gives the same results everywhere.
- **Expressions** are single expressions, the part of the language that computes a value, embedded where content needs
  a small calculation.

Its design goals: **safe** (code reaches only what its host gives it, under budgets of steps, nested calls and
memory), **deterministic** (randomness only from the host's seeded generator), **strictly typed** (no implicit
conversions but joining strings, no `undefined`: every value starts at its type's zero value), **checked early**
(compiled when content is saved), **fast, the same everywhere** (one WebAssembly module on the servers and in the
browser) and **reusable** (nothing in it knows about the game; hosts embed it with their own names, functions and
modules).

## Documents

| Document | What it covers |
| --- | --- |
| [The language](features/language-reference.md) | The reference: lexical structure, types and zero values, expressions and operators, conversions, statements, functions, classes, interfaces, modules, built-ins, budgets, errors; the script test cases |
| [Expressions](features/expressions.md) | Expressions on their own: what they may use, limits, folding; embedding them in Go; the shared expression cases |
| [Script runtime](features/script-runtime.md) | Embedding scripts in Go and in TypeScript, host modules, and compiling to WebAssembly: memory, the collector, calls, budgets, imports and exports, metadata |
| [Code editor and language server](features/code-editor.md) | The editor's Monaco code editor, the language server, and the language's tooling (analysis, completion, signature help, formatting, rename) |

## In the game

| Where | Form | Host module | Document |
| --- | --- | --- | --- |
| Prefabs' computed props and fields | Expressions | — | [Prefabs](features/prefabs.md#prefab-expressions) |
| Script components on objects | Scripts, a `Behaviour` | `"game"` (`game.d.als`) | [Scripted objects](features/scripting.md) |
| Spawners' spawn scripts | Scripts, a `SpawnScript` | `"game"` | [Spawners](features/spawning.md#spawn-scripts) |
| UIs | Scripts, a `UIController`, run in the browser | `"ui"` (`ui.d.als`) | [UIs](features/uis.md#ui-scripts) |
| Animations played by scripts | `play`, `playFor` in scripts | `"game"` | [Animations](features/animations.md#playing-from-scripts) |
| Effects played by scripts | `playEffect`, `playEffectFor` in scripts | `"game"` | [Effects](features/effects.md#playing-effects-from-scripts) |

Scripts are assets (`script/<slug>`), compiled by the asset service on upload (see
[Assets](features/assets.md#scripts)).
