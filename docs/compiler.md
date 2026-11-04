# The compiler command

`kiln compile` is the compiler for hosts that don't embed the Go package, such as a Rust server that runs scripts on
the `kiln` crate and needs them compiled when content is saved. It reads one request as JSON on its standard input
and writes the response as JSON on its standard output; a request it can't run exits with status 1 and the reason on
standard error.

```sh
go install github.com/krotovic/kiln/cmd/kiln@latest
kiln compile < request.json > response.json
```

## The request

```json
{
  "hosts": [{ "name": "game", "source": "export interface Behaviour { … }" }],
  "scripts": { "script/door": "import { Behaviour } from \"game\"; …", "script/util": "…" },
  "imports": ["script/door"],
  "jobs": [
    { "entry": "script/door", "hosts": ["game"], "satisfies": ["game.Behaviour"] },
    { "entry": "script/menu", "hosts": ["ui"], "refuse": { "game": "\"game\" is the servers' module" } }
  ]
}
```

- **`hosts`** are the host modules, each by its name (as scripts import it) and its definition file. They are given
  as declarations only (`HostModule.Declarations`): the natives' code is the host's, wherever it runs the scripts.
  A module compiled here records the same hash of each definition file as one compiled in the host's own process, so
  the host loads it against its own host modules. A definition file that doesn't check refuses the request.
- **`scripts`** are the sources of every script a job may import, by ID.
- **`imports`** lists scripts whose imports the response gives (`ModuleImports`), so a host can find what depends on
  what without compiling.
- **`jobs`** compile a script each: `entry` against the host modules `hosts` names (all of them when it names none).
  A module in `refuse` (and not among the job's host modules) can't be imported, with its message as the reason.
  `satisfies` names host modules' interfaces (`host.Interface`) to check the default class against. A job naming a
  host module or an interface there isn't refuses the request.

## The response

```json
{
  "imports": { "script/door": ["game", "script/util"] },
  "results": [
    {
      "entry": "script/door",
      "module": "AGFzbQ…",
      "modules": ["script/door", "script/util"],
      "default_class": { "name": "Door", "constructor_params": 0 }
    },
    {
      "entry": "script/menu",
      "diagnostics": [{ "module": "script/menu", "line": 1, "col": 1, "message": "…" }]
    }
  ]
}
```

- **`imports`**: each script's imports in the order it writes them (none for a script that doesn't parse).
- **`results`**, one per job in order: the WebAssembly `module` (base64), the IDs of the script's `modules` (the
  entry and the scripts it imports, host modules aside), and the `default_class` the entry exports, if any: its name,
  how many arguments its constructor takes, and, in `unsatisfied`, why it doesn't satisfy each interface of
  `satisfies` it doesn't; or the `diagnostics` (`{module, line, col, message}`) of a script that doesn't compile.

## Tests

`cmd/kiln/compile_test.go`: a module the command compiles is byte for byte the one `CompileScript` makes against the
host with its natives' code, and loads against it; the modules, the default class and the imports; diagnostics of a
script that doesn't type-check, a refused module, a class that doesn't satisfy an interface and takes arguments, a
missing script; requests refused (a host module that doesn't check or is given twice, an unknown host module or
interface, an interface named without its host); JSON in and out, unknown fields refused.
