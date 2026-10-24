package language

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Tests of scripts compiled to WebAssembly: the collector, the host's handles, natives, values crossing the
// boundary, and loading a compiled module.

func compileMain(t *testing.T, src string, limits ScriptLimits, hosts ...*HostModule) *Script {
	t.Helper()
	s, diags := CompileScript("script/main", MapSource{"script/main": src}, ScriptOptions{Limits: limits, Hosts: hosts})
	if len(diags) > 0 {
		t.Fatal(DiagnosticsError(diags))
	}
	return s
}

func newMain(t *testing.T, s *Script, data any) *Instance {
	t.Helper()
	class, ok := s.DefaultClass()
	if !ok {
		t.Fatal("no default class")
	}
	in, err := s.New(class, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	return in
}

func TestTheCollectorFreesGarbageAndCycles(t *testing.T) {
	limits := DefaultScriptLimits
	limits.MaxHeap = 64 << 10
	s := compileMain(t, `
class Node {
  next: Node
  label = ""
}
export default class Churn {
  kept: Node = new Node()
  churn(i: number): number {
    // garbage, cycles included, far more than the heap budget over all the calls
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
}`, limits)
	in := newMain(t, s, nil)
	for i := range 200 {
		if _, err := in.Call("churn", Num(float64(i))); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	got, err := in.Call("label")
	if err != nil || got != Str("kept 199") {
		t.Fatalf("label() = %v, %v: what the object keeps must survive the collections", got, err)
	}
	if size, _ := in.mem.ReadUint32Le(0); size != 0 {
		t.Fatal("address 0 was written")
	}
}

func TestKeptObjectsSurviveCollections(t *testing.T) {
	limits := DefaultScriptLimits
	limits.MaxHeap = 64 << 10
	s := compileMain(t, `
export class Item {
  name: string
  tags: string[] = []
  constructor(name: string) { this.name = name }
  describe(): string { return this.name + ":" + this.tags.join(",") }
}
export default class Shop {
  make(name: string): Item {
    const it = new Item(name)
    it.tags.push("new", name.toUpperCase())
    return it
  }
  waste(): void {
    let s = ""
    for (let i = 0; i < 400; i++) { s = "garbage " + str(i) }
  }
}`, limits)
	in := newMain(t, s, nil)
	item, err := in.Call("make", Str("apple"))
	if err != nil {
		t.Fatal(err)
	}
	in.Keep([]Value{item})
	for range 50 {
		if _, err := in.Call("waste"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := in.CallOn(item, "describe")
	if err != nil || got != Str("apple:new,APPLE") {
		t.Fatalf("describe() = %v, %v", got, err)
	}
	// an object handed out and not kept is gone after the next call
	other, err := in.Call("make", Str("pear"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Call("waste"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.CallOn(other, "describe"); err == nil {
		t.Fatal("an object that wasn't kept was still callable after another call")
	}
}

// objectHost is a host module whose natives hand out and take objects, arrays and maps.
func objectHost() *HostModule {
	type tag struct{ name string }
	var h *HostModule
	h = &HostModule{
		Name: "objects",
		Source: `
export native class Tag {
  readonly name: string
  shout(): string
}
export native function tag(name: string): Tag
export native function sum(xs: number[]): number
export native function counts(words: string[]): Map<string, number>
export native function same(a: Tag, b: Tag): bool
export interface Named { readonly name: string }
`,
		Natives: map[string]NativeFunc{
			"tag": func(_ *Call, _ Value, args []Value) (Value, error) {
				return h.Object("Tag", &tag{args[0].str})
			},
			"Tag.name": func(_ *Call, this Value, _ []Value) (Value, error) {
				return Str(this.Payload().(*tag).name), nil
			},
			"Tag.shout": func(_ *Call, this Value, _ []Value) (Value, error) {
				return Str(strings.ToUpper(this.Payload().(*tag).name) + "!"), nil
			},
			"sum": func(_ *Call, _ Value, args []Value) (Value, error) {
				total := 0.0
				if args[0].Kind() == Array {
					for _, x := range args[0].ref.(*arrayVal).items {
						total += x.num
					}
				}
				return Num(total), nil
			},
			"counts": func(_ *Call, _ Value, args []Value) (Value, error) {
				mv := &mapVal{index: map[mapKey]int{}}
				for _, w := range args[0].ref.(*arrayVal).items {
					if i, ok := mv.index[keyOf(w)]; ok {
						mv.vals[i] = Num(mv.vals[i].num + 1)
						continue
					}
					mv.index[keyOf(w)] = len(mv.keys)
					mv.keys = append(mv.keys, w)
					mv.vals = append(mv.vals, Num(1))
				}
				return Value{kind: Map, ref: mv}, nil
			},
			"same": func(_ *Call, _ Value, args []Value) (Value, error) {
				return Boolean(args[0].ref == args[1].ref), nil
			},
		},
	}
	return h
}

func TestNativesTakeAndGiveObjectsArraysAndMaps(t *testing.T) {
	host := objectHost()
	s := compileMain(t, `
import { Tag, tag, sum, counts, same, Named } from "objects"
export default class User {
  held: Tag = null
  hold(t: Tag): string {
    this.held = t
    return t.name
  }
  again(t: Tag): bool { return this.held == t && same(this.held, t) }
  run(): string {
    const n: Named = tag("bell")
    const m = counts(["a", "b", "a"])
    return n.name + " " + tag("x").shout() + " " + str(sum([1, 2, 3.5])) + " " + str(m.get("a")) + str(m.get("b")) + str(m.size)
  }
}`, DefaultScriptLimits, host)
	in := newMain(t, s, nil)
	got, err := in.Call("run")
	if err != nil || got != Str("bell X! 6.5 212") {
		t.Fatalf("run() = %v, %v", got, err)
	}
	tg, _ := host.Natives["tag"](&Call{}, Value{}, []Value{Str("bell")})
	if got, err := in.Call("hold", tg); err != nil || got != Str("bell") {
		t.Fatalf("hold() = %v, %v", got, err)
	}
	if got, err := in.Call("again", tg); err != nil || got != Boolean(true) {
		t.Fatalf("again() = %v, %v: the same host object must stay the same object", got, err)
	}
}

func TestNativeObjectsTheScriptDropsAreReleased(t *testing.T) {
	host := objectHost()
	limits := DefaultScriptLimits
	limits.MaxHeap = 32 << 10
	s := compileMain(t, `
import { Tag } from "objects"
export default class Passer {
  last = ""
  see(t: Tag): void {
    this.last = t.name
    let s = ""
    for (let i = 0; i < 100; i++) { s = "padding " + str(i) }
  }
}`, limits, host)
	in := newMain(t, s, nil)
	for i := range 300 {
		tg, _ := host.Natives["tag"](&Call{}, Value{}, []Value{Str(fmt.Sprint("t", i))})
		if _, err := in.Call("see", tg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(in.natObjs); n > 100 {
		t.Fatalf("the instance still holds %d host objects the script let go of", n)
	}
	if got, _ := in.Get("last"); got != Str("t299") {
		t.Fatalf("last = %v", got)
	}
}

func TestArraysAndMapsCrossTheBoundary(t *testing.T) {
	s := compileMain(t, `
export default class Box {
  total(xs: number[], names: Map<string, bool>): string {
    let n = 0
    for (const x of xs) { n += x }
    return str(n) + " " + names.keys().join("+") + " " + str(names.get("b"))
  }
  grid(): number[][] { return [[1, 2], [3]] }
}`, DefaultScriptLimits)
	in := newMain(t, s, nil)
	names := &mapVal{index: map[mapKey]int{}}
	for i, k := range []string{"a", "b"} {
		names.index[keyOf(Str(k))] = i
		names.keys = append(names.keys, Str(k))
		names.vals = append(names.vals, Boolean(k == "b"))
	}
	got, err := in.Call("total", Value{kind: Array, ref: &arrayVal{items: []Value{Num(1), Num(2.5)}}}, Value{kind: Map, ref: names})
	if err != nil || got != Str("3.5 a+b true") {
		t.Fatalf("total() = %v, %v", got, err)
	}
	g, err := in.Call("grid")
	if err != nil || show(g) != "[[1, 2], [3]]" {
		t.Fatalf("grid() = %v, %v", show(g), err)
	}
}

func TestNumbersAndStringsAsGoHasThem(t *testing.T) {
	s := compileMain(t, `
export default class T {
  run(): string[] {
    let negZero = -4 % 2
    return [
      str(negZero), str(-0), str(5.5 % 2), str(-5 % 3), str(1e21), str(0.1 + 0.2), str(1 / 3), str(-1.5e-7),
      str(num("1_000")), str(num(" 0x1p-2 ")), str(int(-2.7)), str(round(-2.5)), str(round(0.49999999999999994)),
      "Straße".toUpperCase(), "İSTANBUL".toLowerCase(), "ǅ".toLowerCase(), "  trimmed　".trim(),
      str(len("héllo")), "naïve café".slice(-4), str("aéb".indexOf("b")), "a,b".split("").join("|"),
      str(bool("true")), str(min(0, -0) == 0)
    ]
  }
}`, DefaultScriptLimits)
	in := newMain(t, s, nil)
	got, err := in.Call("run")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-0", "-0", "1.5", "-2", "1000000000000000000000", "0.30000000000000004", "0.3333333333333333",
		"-0.00000015", "1000", "0.25", "-2", "-3", "0", "STRASSE", "i̇stanbul", "ǆ", "trimmed", "5", "café", "2",
		"a|,|b", "true", "true"}
	// Go's unicode.ToUpper keeps ß (it has no single upper-case rune), and ToLower makes İ an i
	want[13], want[14] = strings.ToUpper("Straße"), strings.ToLower("İSTANBUL")
	items := got.ref.(*arrayVal).items
	for i, w := range want {
		if i >= len(items) || items[i].str != w {
			t.Errorf("item %d = %q, want %q", i, items[i].str, w)
		}
	}
}

func TestStringsFromTheHostMayBeInvalidUTF8(t *testing.T) {
	s := compileMain(t, `
export default class T {
  run(s: string): string { return str(s.length) + "|" + s.slice(1) + "|" + s.toUpperCase() + "|" + str(s.indexOf("b")) }
}`, DefaultScriptLimits)
	in := newMain(t, s, nil)
	odd := "a\xffb"
	got, err := in.Call("run", Str(odd))
	want := fmt.Sprintf("%d|%s|%s|%d", 3, string([]rune(odd)[1:]), strings.ToUpper(odd), 2)
	if err != nil || got.str != want {
		t.Fatalf("run() = %q, %v; want %q", got.str, err, want)
	}
}

func TestDispatchFollowsTheObjectsClass(t *testing.T) {
	s := compileMain(t, `
interface Shape { area(): number; label: string }
class Square implements Shape {
  label = "square"
  side = 2
  area(): number { return this.side * this.side }
}
class Big extends Square {
  area(): number { return super.area() * 10 }
}
class Circle implements Shape {
  label = "circle"
  area(): number { return 3 }
}
export function main(): string {
  const shapes: Shape[] = [new Square(), new Big(), new Circle()]
  let out = ""
  for (const s of shapes) {
    s.label = s.label + "!"
    out += s.label + "=" + str(s.area()) + " "
  }
  const sq: Square = new Big()
  return out + str(sq.area())
}`, DefaultScriptLimits)
	got, err := s.Call("main", nil)
	if err != nil || got != Str("square!=4 square!=40 circle!=3 40") {
		t.Fatalf("main() = %v, %v", got, err)
	}
}

func TestConstantsThatFailAreCompileErrors(t *testing.T) {
	_, diags := CompileScript("script/main", MapSource{"script/main": `
const ZERO = 0
const BAD = 1 / ZERO
export function main(): number { return BAD }`}, ScriptOptions{})
	if len(diags) != 1 || !strings.Contains(diags[0].Error(), "script/main:3:15: division by zero") {
		t.Fatalf("diagnostics = %v", DiagnosticsError(diags))
	}
}

func TestLoadScriptRunsTheModule(t *testing.T) {
	s := compileMain(t, `
import { log } from "test"
export function main(): number { log("loaded"); return 7 }`, DefaultScriptLimits, probeHost)
	loaded, err := LoadScript(s.Module(), ScriptOptions{Hosts: []*HostModule{probeHost}})
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	if got, err := loaded.Call("main", &logs); err != nil || got != Num(7) || strings.Join(logs, ",") != "loaded" {
		t.Fatalf("main() = %v, %v, logs %v", got, err, logs)
	}
	if _, err := LoadScript(s.Module(), ScriptOptions{}); err == nil {
		t.Fatal("a module was loaded without the host module it imports")
	}
	changed := testHost()
	changed.Source += "\nexport interface Extra { x: number }"
	if _, err := LoadScript(s.Module(), ScriptOptions{Hosts: []*HostModule{changed}}); err == nil || !strings.Contains(err.Error(), "another version") {
		t.Fatalf("LoadScript against a changed host module = %v", err)
	}
	if _, err := LoadScript([]byte("not wasm"), ScriptOptions{}); err == nil {
		t.Fatal("LoadScript took something that isn't a module")
	}
	// a module made for another runtime version is refused
	old := []byte(fmt.Sprintf(`{"abi":%d,`, abiVersion))
	if !bytes.Contains(s.Module(), old) {
		t.Fatal("the metadata doesn't start with its ABI version")
	}
	bin := bytes.Replace(s.Module(), old, []byte(fmt.Sprintf(`{"abi":%d,`, abiVersion+1)), 1)
	if _, err := LoadScript(bin, ScriptOptions{Hosts: []*HostModule{probeHost}}); err == nil || !strings.Contains(err.Error(), "runtime version") {
		t.Fatalf("LoadScript of another runtime version = %v", err)
	}
}

func TestTheSameSourcesCompileToTheSameModule(t *testing.T) {
	src := `
class A { x = 1; f(): number { return this.x } }
export default class B extends A { g(s: string): string { return s.toUpperCase() + str(this.f()) } }`
	a := compileMain(t, src, DefaultScriptLimits)
	b := compileMain(t, src, DefaultScriptLimits)
	if !bytes.Equal(a.Module(), b.Module()) {
		t.Fatal("compiling the same script twice gave different modules")
	}
}

func TestErrorsNameWhereTheyHappen(t *testing.T) {
	s := compileMain(t, `
class Box { xs: number[] = [] }
export function main(): number {
  const b = new Box()
  return b.xs[3]
}`, DefaultScriptLimits)
	_, err := s.Call("main", nil)
	var se *ScriptError
	if !errors.As(err, &se) || se.Module != "script/main" || se.Line != 5 || se.Col != 14 || !errors.Is(err, ErrEval) {
		t.Fatalf("error = %#v", err)
	}
}

func TestFunctionsRunWithoutClasses(t *testing.T) {
	limits := DefaultScriptLimits
	limits.MaxHeap = 64 << 10
	s := compileMain(t, `
export function main(): number {
  let xs: string[] = []
  for (let i = 0; i < 2000; i++) { xs.push("text that fills the heap " + str(i)) }
  return xs.length
}`, limits)
	if got, err := s.Call("main", nil); err == nil {
		t.Fatalf("main() = %v: it allocated past the heap budget", got)
	} else if !strings.Contains(err.Error(), "memory") {
		t.Fatalf("main() failed with %v, not the memory budget", err)
	}
}

func TestAHelperReportsItsOwnTrap(t *testing.T) {
	s := compileMain(t, `
export default class Divider {
  divide(a: number, b: number): number { return a / b }
}`, DefaultScriptLimits)
	in := newMain(t, s, nil)
	_, first := in.Call("divide", Num(1), Num(0))
	if first == nil || !strings.Contains(first.Error(), "division by zero") {
		t.Fatalf("divide(1, 0) = %v", first)
	}
	// a handle far past the table reads outside the memory: the trap is the helper's own, not the call's
	_, err := in.helper("__handle_get", 1<<29)
	if err == nil || strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("the helper's trap = %v, not its own", err)
	}
	if got, err := in.Call("divide", Num(6), Num(3)); err != nil || got != Num(2) {
		t.Fatalf("divide(6, 3) = %v, %v after the failures", got, err)
	}
}

func TestAClosedScriptMakesNoInstances(t *testing.T) {
	s := compileMain(t, `export default class A { f(): number { return 1 } }`, DefaultScriptLimits)
	in := newMain(t, s, nil)
	s.Close()
	if got, err := in.Call("f"); err != nil || got != Num(1) {
		t.Fatalf("an instance stopped working when its script closed: %v, %v", got, err)
	}
	class, _ := s.DefaultClass()
	if _, err := s.New(class, nil); err == nil {
		t.Fatal("a closed script made an instance")
	}
	in.Close()
	if s.compiled != nil {
		t.Fatal("the closed script kept its machine code after its last instance closed")
	}
}
