package language

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// scriptCase is one case of testdata/scripts: modules by ID (script/main is the entry, whose exported main()
// runs), and what to expect: want (main's value, formatted by show), logs (what the test module's log got),
// compile (a diagnostic, as "module:line:col: part of its message") or eval (part of the error main fails with).
type scriptCase struct {
	name    string
	file    string
	modules map[string]string
	want    *string
	logs    *string
	compile *string
	eval    *string
}

// readScriptCases reads a cases file: "=== name" starts a case, and "-- section --" a section of it.
func readScriptCases(t *testing.T, path string) []*scriptCase {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []*scriptCase
	var cur *scriptCase
	var section string
	var body []string
	flush := func() {
		if cur == nil || section == "" {
			return
		}
		text := strings.TrimRight(strings.Join(body, "\n"), "\n")
		switch {
		case strings.HasPrefix(section, "script/"):
			cur.modules[section] = text
		case section == "want":
			cur.want = &text
		case section == "logs":
			cur.logs = &text
		case section == "compile error":
			cur.compile = &text
		case section == "eval error":
			cur.eval = &text
		default:
			t.Fatalf("%s: unknown section %q", path, section)
		}
		section, body = "", nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			flush()
			cur = &scriptCase{name: strings.TrimPrefix(line, "=== "), file: filepath.Base(path), modules: map[string]string{}}
			cases = append(cases, cur)
		case strings.HasPrefix(line, "-- ") && strings.HasSuffix(line, " --"):
			flush()
			section = strings.TrimSuffix(strings.TrimPrefix(line, "-- "), " --")
		case section != "":
			body = append(body, line)
		}
	}
	flush()
	return cases
}

// testHost is the host module the cases import as "test". Its log appends to the call's data.
func testHost() *HostModule {
	return &HostModule{
		Name: "test",
		Source: `
export native function log(message: string): void
export native function probe(name: string): Probe
export native class Probe {
  readonly name: string
  twice(n: number): number
}
export interface Greeter {
  greet(name: string): string
  hello?(): string
}
export interface Hooks {
  ready?(): void
  run(): void
}
export native error class ProbeError {
  readonly code: string
}
// later, laterText and failLater give promises the case's harness settles after main, in the order they were made
export native function later(v: number): Promise<number, ProbeError>
export native function laterText(s: string): Promise<string>
export native function failLater(code: string): Promise<number, ProbeError>
`,
		Natives: map[string]NativeFunc{
			"log": func(c *Call, _ Value, args []Value) (Value, error) {
				if logs, ok := c.Data().(*[]string); ok {
					*logs = append(*logs, args[0].str)
				}
				if d, ok := c.Data().(*caseData); ok {
					d.logs = append(d.logs, args[0].str)
				}
				return Value{}, nil
			},
			"later": func(c *Call, _ Value, args []Value) (Value, error) {
				return later(c, func(r *Resolver) error { return r.Resolve(args[0]) })
			},
			"laterText": func(c *Call, _ Value, args []Value) (Value, error) {
				return later(c, func(r *Resolver) error { return r.Resolve(args[0]) })
			},
			"failLater": func(c *Call, _ Value, args []Value) (Value, error) {
				return later(c, func(r *Resolver) error {
					e, err := probeHost.Object("ProbeError", args[0].str)
					if err != nil {
						return err
					}
					return r.Reject(e)
				})
			},
			"ProbeError.code": func(_ *Call, this Value, _ []Value) (Value, error) {
				return Str(this.Payload().(string)), nil
			},
			"probe": func(_ *Call, _ Value, args []Value) (Value, error) {
				return probeHost.Object("Probe", args[0].str)
			},
			"Probe.name": func(_ *Call, this Value, _ []Value) (Value, error) {
				return Str(this.Payload().(string)), nil
			},
			"Probe.twice": func(_ *Call, _ Value, args []Value) (Value, error) {
				if args[0].num < 0 {
					return Value{}, fmt.Errorf("no negative numbers")
				}
				return Num(args[0].num * 2), nil
			},
		},
	}
}

// caseData is what a case's natives get: what it logs, and how to settle the promises they made, in order.
type caseData struct {
	logs    []string
	pending []func() error
}

// later makes the promise of a native that the case's harness settles with settle once main returned.
func later(c *Call, settle func(*Resolver) error) (Value, error) {
	d, ok := c.Data().(*caseData)
	if !ok {
		return Value{}, fmt.Errorf("no harness settles promises here")
	}
	v, r, err := c.NewPromise()
	if err != nil {
		return Value{}, err
	}
	d.pending = append(d.pending, func() error { return settle(r) })
	return v, nil
}

// runMain calls the case's main, then settles the promises its natives made, in order, as they are made.
func runMain(s *Script, d *caseData) (Value, error) {
	in, err := s.instantiate(d)
	if err != nil {
		return Value{}, err
	}
	defer in.Close()
	if err := in.initConsts(); err != nil {
		return Value{}, err
	}
	fm := s.meta.Funcs["main"]
	if fm == nil {
		return Value{}, fmt.Errorf("no main")
	}
	got, err := in.invoke(fm.Export, -1, nil, fm.Params, fm.Result)
	for err == nil && len(d.pending) > 0 {
		next := d.pending[0]
		d.pending = d.pending[1:]
		err = next()
	}
	return got, err
}

// probeHost is the test module, compiled once for every case; its probe native makes its Probe objects.
var probeHost *HostModule

func init() { probeHost = testHost() }

// show formats a value for the cases' want sections.
func show(v Value) string {
	switch v.kind {
	case String:
		return strconv.Quote(v.str)
	case Array:
		parts := []string{}
		for _, item := range v.ref.(*arrayVal).items {
			parts = append(parts, show(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Map:
		mv := v.ref.(*mapVal)
		parts := []string{}
		for i := range mv.keys {
			parts = append(parts, show(mv.keys[i])+": "+show(mv.vals[i]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case Object:
		return v.ref.(*object).class.name
	case 0:
		return "nothing"
	}
	return v.String()
}

func TestScriptCases(t *testing.T) {
	files, err := filepath.Glob("testdata/scripts/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no cases: %v", err)
	}
	for _, file := range files {
		for _, c := range readScriptCases(t, file) {
			t.Run(c.file+"/"+c.name, func(t *testing.T) { runScriptCase(t, c) })
		}
	}
}

func runScriptCase(t *testing.T, c *scriptCase) {
	d := &caseData{}
	s, diags := CompileScript("script/main", MapSource(c.modules), ScriptOptions{Hosts: []*HostModule{probeHost}})
	if c.compile != nil {
		if len(diags) == 0 {
			t.Fatalf("compiled; want the diagnostic %s", *c.compile)
		}
		for _, d := range diags {
			if strings.Contains(d.Error(), *c.compile) {
				return
			}
		}
		t.Fatalf("diagnostics:\n%v\nwant one with %q", DiagnosticsError(diags), *c.compile)
	}
	if len(diags) > 0 {
		t.Fatalf("compile:\n%v", DiagnosticsError(diags))
	}
	got, err := runMain(s, d)
	logs := d.logs
	if c.eval != nil {
		if !errors.Is(err, ErrEval) || !strings.Contains(err.Error(), *c.eval) {
			t.Fatalf("main() = %v, %v; want an evaluation error with %q", show(got), err, *c.eval)
		}
	} else if err != nil {
		t.Fatalf("main(): %v", err)
	}
	if c.want != nil && show(got) != *c.want {
		t.Fatalf("main() = %s, want %s", show(got), *c.want)
	}
	if c.logs != nil && strings.Join(logs, "\n") != *c.logs {
		t.Fatalf("logs:\n%s\nwant:\n%s", strings.Join(logs, "\n"), *c.logs)
	}
}

const behaviourHost = `
export interface Behaviour {
  onStart?(): void
  onUpdate?(dt: number): void
}
export native function log(message: string): void
`

func TestInstances(t *testing.T) {
	host := &HostModule{Name: "host", Source: behaviourHost, Natives: map[string]NativeFunc{
		"log": func(c *Call, _ Value, args []Value) (Value, error) {
			*c.Data().(*[]string) = append(*c.Data().(*[]string), args[0].str)
			return Value{}, nil
		},
	}}
	src := MapSource{"script/door": `
import { Behaviour, log } from "host"
export default class Door implements Behaviour {
  greeting = "hi"
  readonly speed: number = 1
  private opened = 0
  onUpdate(dt: number): void {
    this.opened += dt * this.speed
    log(this.greeting + " " + str(this.opened))
  }
}`}
	s, diags := CompileScript("script/door", src, ScriptOptions{Hosts: []*HostModule{host}})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	class, ok := s.DefaultClass()
	if !ok || class.Name() != "Door" {
		t.Fatalf("DefaultClass = %v, %v", class, ok)
	}
	behaviour, err := host.Interface("Behaviour")
	if err != nil {
		t.Fatal(err)
	}
	if why := class.Satisfies(behaviour); why != "" {
		t.Fatalf("Door doesn't satisfy Behaviour: %s", why)
	}
	var logs []string
	in, err := s.New(class, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := in.FieldKind("speed"); !ok || k != Number {
		t.Fatalf("FieldKind(speed) = %v, %v", k, ok)
	}
	if _, ok := in.FieldKind("opened"); ok {
		t.Fatal("a private field can be set")
	}
	if err := in.Set("speed", Str("fast")); err == nil {
		t.Fatal("Set took a string for a number")
	}
	if err := in.Set("speed", Num(2)); err != nil {
		t.Fatal(err)
	}
	if !in.HasMethod("onUpdate") || in.HasMethod("onStart") {
		t.Fatal("HasMethod is wrong")
	}
	for range 2 {
		if _, err := in.Call("onUpdate", Num(0.5)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(logs, ","); got != "hi 1,hi 2" {
		t.Fatalf("logs = %s", got)
	}
	if _, err := in.Call("onUpdate", Str("x")); err == nil {
		t.Fatal("Call took a string for a number")
	}
}

func TestInstanceMemoryIsBounded(t *testing.T) {
	src := MapSource{"script/main": `
export default class Hoarder {
  kept: string[][] = []
  grow(): void {
    let row: string[] = []
    for (let i = 0; i < 1000; i++) {
      row.push("some text that takes room " + str(i))
    }
    this.kept.push(row)
  }
}`}
	limits := DefaultScriptLimits
	limits.MaxHeap = 1 << 20
	s, diags := CompileScript("script/main", src, ScriptOptions{Limits: limits})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	class, _ := s.DefaultClass()
	in, err := s.New(class, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		if _, err = in.Call("grow"); err != nil {
			if !errors.Is(err, ErrEval) || !strings.Contains(err.Error(), "memory") {
				t.Fatalf("call %d: %v, want a memory error", i, err)
			}
			return
		}
	}
	t.Fatal("the instance kept growing past MaxHeap")
}

func TestScriptsAreSafeToShare(t *testing.T) {
	src := MapSource{"script/main": `
export default class Counter {
  n = 0
  add(k: number): number {
    for (let i = 0; i < k; i++) { this.n++ }
    return this.n
  }
}`}
	s, diags := CompileScript("script/main", src, ScriptOptions{})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	class, _ := s.DefaultClass()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := s.New(class, nil)
			if err != nil {
				t.Error(err)
				return
			}
			for j := 1; j <= 50; j++ {
				if got, err := in.Call("add", Num(2)); err != nil || got != Num(float64(2*j)) {
					t.Errorf("add = %v, %v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestDiagnosticsAreCollected(t *testing.T) {
	src := MapSource{"script/main": `
export function a(): number { return "x" }
export function b(): string { return 1 }
export function c(): void { nope() }`}
	_, diags := CompileScript("script/main", src, ScriptOptions{})
	if len(diags) != 3 {
		t.Fatalf("diagnostics = %v, want 3", DiagnosticsError(diags))
	}
	lines := []int{}
	for _, d := range diags {
		lines = append(lines, d.Line)
	}
	sort.Ints(lines)
	if fmt.Sprint(lines) != "[2 3 4]" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestModuleImports(t *testing.T) {
	got, err := ModuleImports(`import { a } from "script/a"
import B, { c as d } from "script/b"
export const x = 1`)
	if err != nil || strings.Join(got, ",") != "script/a,script/b" {
		t.Fatalf("ModuleImports = %v, %v", got, err)
	}
}

func TestExpressionsRefuseScriptSyntax(t *testing.T) {
	for _, src := range []string{"a.b", "a[0]", "x = 1", "{}", "'s'"} {
		_, err := Compile(src, Std(), NewScope("a", "x"), DefaultLimits)
		var ce *Error
		if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "not allowed in an expression") {
			t.Errorf("Compile(%q) = %v, want it refused as script syntax", src, err)
		}
	}
	// Words that are keywords in scripts are still names in expressions.
	prog, err := Compile("new + class", Std(), NewScope("new", "class"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := prog.Eval(MapEnv{"new": Num(1), "class": Num(2)}); got != Num(3) {
		t.Fatalf("Eval = %v", got)
	}
}

func TestHostModuleNeedsItsNatives(t *testing.T) {
	h := &HostModule{Name: "broken", Source: "export native function missing(): void"}
	if err := h.Check(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Check = %v, want the missing native named", err)
	}
}

func TestPromisesSurviveCollections(t *testing.T) {
	small := DefaultScriptLimits
	small.MaxHeap = 64 << 10
	src := MapSource{"script/main": `
import { later, log, ProbeError } from "test"
class Node {
  next: Node = null
  label = ""
}
export default class Waiter {
  async wait(name: string): Promise<void> {
    const keep = "kept " + name
    const xs: string[] = [name + "1", name + "2"]
    for (const x of xs) {
      const [v, err] = await later(1)
      log(keep + " " + x + " " + str(v) + " " + str(err == null))
    }
  }
  start(): void {
    this.wait("a")
    this.wait("b")
  }
  churn(): void {
    for (let k = 0; k < 100; k++) {
      const a = new Node()
      a.label = "garbage garbage garbage " + str(k)
    }
  }
}`}
	s, diags := CompileScript("script/main", src, ScriptOptions{Hosts: []*HostModule{probeHost}, Limits: small})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	class, _ := s.DefaultClass()
	d := &caseData{}
	in, err := s.New(class, d)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.Call("start"); err != nil {
		t.Fatal(err)
	}
	for len(d.pending) > 0 {
		for range 30 {
			if _, err := in.Call("churn"); err != nil {
				t.Fatal(err)
			}
		}
		next := d.pending[0]
		d.pending = d.pending[1:]
		if err := next(); err != nil {
			t.Fatal(err)
		}
	}
	want := "kept a a1 1 true,kept b b1 1 true,kept a a2 1 true,kept b b2 1 true"
	if got := strings.Join(d.logs, ","); got != want {
		t.Fatalf("logs = %s, want %s", got, want)
	}
	if in.pending != 0 {
		t.Fatalf("%d promises still pending", in.pending)
	}
}

func TestResolvers(t *testing.T) {
	src := MapSource{"script/main": `
import { later, laterText, failLater, log, ProbeError, Probe, probe } from "test"
export default class W {
  async a(): Promise<void> {
    const [v, err] = await later(1)
    log("a " + str(v))
  }
  async b(): Promise<void> {
    const s = await laterText("x")
    log("b " + s)
  }
  async c(): Promise<void> {
    const [v, err] = await failLater("no")
    log("c " + err.code)
  }
}`}
	s, diags := CompileScript("script/main", src, ScriptOptions{Hosts: []*HostModule{probeHost}})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	class, _ := s.DefaultClass()
	var resolvers []*Resolver
	host := testHost()
	grab := func(c *Call, _ Value, _ []Value) (Value, error) {
		v, r, err := c.NewPromise()
		resolvers = append(resolvers, r)
		return v, err
	}
	host.Natives["later"], host.Natives["laterText"], host.Natives["failLater"] = grab, grab, grab
	loaded, err := LoadScript(s.Module(), ScriptOptions{Hosts: []*HostModule{host}})
	if err != nil {
		t.Fatal(err)
	}
	d := &caseData{}
	in, err := loaded.New(class, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"a", "b", "c"} {
		if _, err := in.Call(m); err != nil {
			t.Fatal(err)
		}
	}
	a, b, c := resolvers[0], resolvers[1], resolvers[2]
	if err := a.Resolve(Str("one")); err == nil {
		t.Fatal("a string resolved a Promise<number, ProbeError>")
	}
	if err := b.Reject(null); err == nil {
		t.Fatal("a Promise<string> was rejected")
	}
	other, _ := host.Object("Probe", "p")
	if err := c.Reject(other); err == nil {
		t.Fatal("a Probe rejected a promise of ProbeError")
	}
	pe, _ := host.Object("ProbeError", "no")
	if err := c.Reject(pe); err != nil {
		t.Fatal(err)
	}
	if err := b.Resolve(Str("x")); err != nil {
		t.Fatal(err)
	}
	if err := a.Resolve(Num(1)); err != nil {
		t.Fatal(err)
	}
	if a.Pending() || a.Resolve(Num(2)) != nil {
		t.Fatal("a settled promise settled again")
	}
	if got := strings.Join(d.logs, ","); got != "c no,b x,a 1" {
		t.Fatalf("logs = %s", got)
	}
	in.Close()
	if err := resolvers[0].Resolve(Num(1)); err != nil {
		t.Fatalf("after Close: %v", err)
	}
}

func TestInterfacesForOtherRuntimes(t *testing.T) {
	metas, err := probeHost.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	g := metas["Greeter"]
	want := []InterfaceMember{
		{Name: "greet", Method: true, Canon: "(string): string", Text: "(name: string): string"},
		{Name: "hello", Method: true, Optional: true, Canon: "(): string", Text: "(): string"},
	}
	if g.Name != "Greeter" || fmt.Sprint(g.Members) != fmt.Sprint(want) {
		t.Fatalf("Greeter = %+v, want %+v", g, want)
	}
	if _, ok := metas["Hooks"]; !ok || len(metas) != 2 {
		t.Fatalf("interfaces = %v, want Greeter and Hooks", metas)
	}
	named, err := objectHost().Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if m := named["Named"].Members; len(m) != 1 || m[0] != (InterfaceMember{Name: "name", Readonly: true, Canon: "string", Text: "string"}) {
		t.Fatalf("Named = %+v", m)
	}
}
