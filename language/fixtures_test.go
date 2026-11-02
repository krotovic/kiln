package language

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWriteBrowserFixtures writes what the browser's runtime is tested against (js/src): every
// case of testdata/scripts that runs, compiled, with what it must give; a few scripts for instances; and how Go
// formats and parses numbers. It runs only with KILN_FIXTURES set to the directory to write
// (js/scripts/fixtures.mjs sets it).
func TestWriteBrowserFixtures(t *testing.T) {
	dir := os.Getenv("KILN_FIXTURES")
	if dir == "" {
		t.Skip("KILN_FIXTURES isn't set")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	type fixtureCase struct {
		Name   string  `json:"name"`
		Module string  `json:"module"`
		Want   *string `json:"want,omitempty"`
		Logs   *string `json:"logs,omitempty"`
		Eval   *string `json:"eval,omitempty"`
	}
	write := func(name string, s *Script) string {
		file := "modules/" + name + ".wasm"
		if err := os.WriteFile(filepath.Join(dir, file), s.Module(), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	var cases []fixtureCase
	files, _ := filepath.Glob("testdata/scripts/*.txt")
	for _, file := range files {
		for i, c := range readScriptCases(t, file) {
			if c.compile != nil {
				continue
			}
			s, diags := CompileScript("script/main", MapSource(c.modules), ScriptOptions{Hosts: []*HostModule{probeHost}})
			if len(diags) > 0 {
				t.Fatalf("%s: %v", c.name, DiagnosticsError(diags))
			}
			name := fmt.Sprintf("%s-%d", strings.TrimSuffix(c.file, ".txt"), i)
			cases = append(cases, fixtureCase{Name: c.file + "/" + c.name, Module: write(name, s), Want: c.want, Logs: c.logs, Eval: c.eval})
		}
	}

	small := DefaultScriptLimits
	small.MaxHeap = 64 << 10
	extras := map[string]struct {
		src    string
		limits ScriptLimits
		hosts  []*HostModule
	}{
		"door": {`
import { Behaviour, log } from "host"
export default class Door implements Behaviour {
  greeting = "hi"
  readonly speed: number = 1
  private opened = 0
  onUpdate(dt: number): void {
    this.opened += dt * this.speed
    log(this.greeting + " " + str(this.opened))
  }
}`, DefaultScriptLimits, []*HostModule{{Name: "host", Source: behaviourHost, Natives: map[string]NativeFunc{
			"log": func(*Call, Value, []Value) (Value, error) { return Value{}, nil }}}}},
		"churn": {`
class Node {
  next: Node
  label = ""
}
export class Item {
  name: string
  tags: string[] = []
  constructor(name: string) { this.name = name }
  describe(): string { return this.name + ":" + this.tags.join(",") }
}
export default class Churn {
  kept: Node = new Node()
  churn(i: number): number {
    for (let k = 0; k < 50; k++) {
      const a = new Node()
      const b = new Node()
      a.next = b
      b.next = a
      a.label = "a node with a label " + str(i) + "/" + str(k)
    }
    this.kept.label = "kept " + str(i)
    return i
  }
  label(): string { return this.kept.label }
  make(name: string): Item {
    const it = new Item(name)
    it.tags.push("new", name.toUpperCase())
    return it
  }
  hoard(n: number): void {
    let xs: string[] = []
    for (let i = 0; i < n; i++) { xs.push("held text " + str(i)) }
    this.kept.label = xs.join("")
  }
}`, small, nil},
		"objects": {`
import { Tag, tag, sum, counts, same, Named } from "objects"
export default class User {
  held: Tag = null
  last = ""
  hold(t: Tag): string {
    this.held = t
    return t.name
  }
  again(t: Tag): bool { return this.held == t && same(this.held, t) }
  see(t: Tag): void {
    this.last = t.name
    let s = ""
    for (let i = 0; i < 100; i++) { s = "padding " + str(i) }
  }
  run(): string {
    const n: Named = tag("bell")
    const m = counts(["a", "b", "a"])
    return n.name + " " + tag("x").shout() + " " + str(sum([1, 2, 3.5])) + " " + str(m.get("a")) + str(m.get("b")) + str(m.size)
  }
  total(xs: number[], names: Map<string, bool>): string {
    let n = 0
    for (const x of xs) { n += x }
    return str(n) + " " + names.keys().join("+") + " " + str(names.get("b"))
  }
}`, small, []*HostModule{objectHost()}},
	}
	unbounded := DefaultScriptLimits
	unbounded.MaxSteps = 1 << 30
	extras["bench-loop"] = struct {
		src    string
		limits ScriptLimits
		hosts  []*HostModule
	}{benchLoop, unbounded, nil}
	extras["bench-calls"] = struct {
		src    string
		limits ScriptLimits
		hosts  []*HostModule
	}{benchCalls, unbounded, nil}
	modules := map[string]string{}
	hostSources := map[string]string{"test": probeHost.Source, "host": behaviourHost, "objects": objectHost().Source}
	for name, e := range extras {
		s, diags := CompileScript("script/main", MapSource{"script/main": e.src}, ScriptOptions{Limits: e.limits, Hosts: e.hosts})
		if len(diags) > 0 {
			t.Fatalf("%s: %v", name, DiagnosticsError(diags))
		}
		modules[name] = write(name, s)
	}

	type formatCase struct {
		Bits string `json:"bits"` // the float64's bits, in hex, so JSON doesn't round it
		Text string `json:"text"`
	}
	type parseCase struct {
		Text string  `json:"text"`
		Bits *string `json:"bits"` // nil: not a number
	}
	rng := rand.New(rand.NewSource(1))
	var formats []formatCase
	addFormat := func(f float64) {
		if finite(f) {
			formats = append(formats, formatCase{Bits: strconv.FormatUint(math.Float64bits(f), 16), Text: numberText(f)})
		}
	}
	for _, f := range []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 0.2, 0.30000000000000004, 1e21, 1e-7, 123456789012345678,
		9007199254740993, 5e-324, math.MaxFloat64, math.SmallestNonzeroFloat64, 1.5e300, 2.5e-300, 1 / 3.0, 100, 1e20} {
		addFormat(f)
	}
	for range 3000 {
		addFormat(math.Float64frombits(rng.Uint64()))
		addFormat(rng.Float64() * math.Pow(10, float64(rng.Intn(40)-20)))
		addFormat(float64(rng.Int63n(1<<62)) / float64(int64(1)<<rng.Intn(60)))
	}
	var parses []parseCase
	for _, text := range []string{"1", "-1", "+5", ".5", "5.", "1e5", "1E-5", "1_000", "1__0", "_1", "1_", "0x10", "0x1p-2",
		"0X1.8P1", "0x_1p0", "0x1_0p0", "0x1p", "0x.8p1", "1e", "1e+", "e5", "", ".", "-", "inf", "-Inf", "infinity", "nan", "1e400",
		"-1e400", "1e-400", "0.000_5", "1_0.5", "1._5", "12a", "1.2.3", "0x", "0x1", "1e1_0", "0b101", "0o17", "007", "1,5",
		"123456789012345678901234567890", "4.9406564584124654e-324", "2.2250738585072011e-308", "1.7976931348623157e308",
		"1.7976931348623159e308", "0x1.fffffffffffffp1023", "0x1p-1074", "0x1p-1075", "9007199254740993", "0.1e-1", "-0", "+0"} {
		pc := parseCase{Text: text}
		if f, err := strconv.ParseFloat(text, 64); err == nil && finite(f) {
			bits := strconv.FormatUint(math.Float64bits(f), 16)
			pc.Bits = &bits
		}
		parses = append(parses, pc)
	}
	for _, f := range formats[:500] {
		bits := f.Bits
		parses = append(parses, parseCase{Text: f.Text, Bits: &bits})
	}
	quotes := map[string]string{}
	for _, s := range []string{"abc", `a"b\c`, "tab\tnew\nline", "é ü 日本", "\x00\x07\x7f", "​­", "😀", " "} {
		quotes[s] = quoteText(s)
	}
	out := map[string]any{"cases": cases, "modules": modules, "hosts": hostSources, "formats": formats, "parses": parses,
		"quotes": quotes}
	raw, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
