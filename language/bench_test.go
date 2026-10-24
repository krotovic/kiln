package language

import "testing"

// The interpreter these replace took about 1.0 ms for the loop and 2.6 ms for the calls (M2 Max).
const benchLoop = `
export default class Loop {
  run(): number {
    let s = 0
    for (let i = 0; i < 10000; i++) { s = s + i * 2 }
    return s
  }
}`

const benchCalls = `
class V {
  x = 0
  y = 0
  add(dx: number, dy: number): void { this.x += dx; this.y += dy }
}
export default class Calls {
  v: V = new V()
  run(): number {
    for (let i = 0; i < 10000; i++) { this.v.add(1, 2) }
    return this.v.x + this.v.y
  }
}`

const benchStrings = `
export default class Strings {
  run(): number {
    const m = new Map<string, number>()
    for (let i = 0; i < 1000; i++) { m.set("key-" + str(i % 100), i) }
    let n = 0
    for (const k of m.keys()) { n += k.length }
    return n
  }
}`

func benchScript(b *testing.B, src string) {
	limits := DefaultScriptLimits
	limits.MaxSteps = 1 << 30
	s, diags := CompileScript("script/main", MapSource{"script/main": src}, ScriptOptions{Limits: limits})
	if len(diags) > 0 {
		b.Fatal(DiagnosticsError(diags))
	}
	class, _ := s.DefaultClass()
	in, err := s.New(class, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer in.Close()
	b.ResetTimer()
	for range b.N {
		if _, err := in.Call("run"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScriptLoop(b *testing.B)    { benchScript(b, benchLoop) }
func BenchmarkScriptCalls(b *testing.B)   { benchScript(b, benchCalls) }
func BenchmarkScriptStrings(b *testing.B) { benchScript(b, benchStrings) }
