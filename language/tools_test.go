package language

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cursor marks the position a tools case asks about, in one of its modules.
const cursor = "‸"

// toolCase is one case of testdata/tools: modules (one with the cursor), and what to expect at the cursor:
// hover (detail, then doc), definition (module:line:col, or none), completions (lines "+ label" that must be
// offered and "- label" that mustn't), signature ("label @ active parameter", or none), references
// (module:line:col lines, for what is at the cursor), rename (why it can't be renamed alone, or ok), symbols (the
// outline, indented), diagnostics (lines of diagnostics, or none).
type toolCase struct {
	name     string
	file     string
	modules  map[string]string
	sections map[string]string
}

func readToolCases(t *testing.T, path string) []*toolCase {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []*toolCase
	var cur *toolCase
	section, body := "", []string{}
	flush := func() {
		if cur == nil || section == "" {
			return
		}
		text := strings.TrimRight(strings.Join(body, "\n"), "\n")
		if strings.HasPrefix(section, "script/") {
			cur.modules[section] = text
		} else {
			cur.sections[section] = text
		}
		section, body = "", nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			flush()
			cur = &toolCase{name: strings.TrimPrefix(line, "=== "), file: filepath.Base(path), modules: map[string]string{}, sections: map[string]string{}}
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

// where writes a location as module:line:col.
func where(src MapSource, l Location) string {
	text, ok := src[l.Module]
	if !ok && l.Module == probeHost.Name {
		text = probeHost.Source
	}
	m := newModule(l.Module, text)
	line, col := m.lineCol(l.Start)
	return fmt.Sprintf("%s:%d:%d", l.Module, line, col)
}

func outline(syms []Symbol, indent string) []string {
	var out []string
	for _, s := range syms {
		out = append(out, indent+string(s.Kind)+" "+s.Name)
		out = append(out, outline(s.Children, indent+"  ")...)
	}
	return out
}

func TestToolCases(t *testing.T) {
	files, err := filepath.Glob("testdata/tools/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no cases: %v", err)
	}
	for _, file := range files {
		for _, c := range readToolCases(t, file) {
			t.Run(c.file+"/"+c.name, func(t *testing.T) { runToolCase(t, c) })
		}
	}
}

func runToolCase(t *testing.T, c *toolCase) {
	src := MapSource{}
	module, offset := "", -1
	for id, text := range c.modules {
		if i := strings.Index(text, cursor); i >= 0 {
			module, offset = id, i
			text = text[:i] + text[i+len(cursor):]
		}
		src[id] = text
	}
	if module == "" {
		module = "script/main"
	}
	opts := ScriptOptions{Hosts: []*HostModule{probeHost}}
	a := Analyze(module, src, opts)
	ref, found := a.At(module, offset)
	for section, want := range c.sections {
		var got string
		switch section {
		case "hover":
			if found {
				got = strings.TrimSpace(ref.Detail + "\n" + ref.Doc)
			}
		case "definition":
			got = "none"
			if found && ref.Def != nil {
				got = where(src, *ref.Def)
			}
		case "completions":
			labels := map[string]bool{}
			for _, item := range Complete(module, src, opts, offset) {
				labels[item.Label] = true
			}
			for _, line := range strings.Split(want, "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "+ ") && !labels[line[2:]]:
					t.Errorf("completions lack %q: %v", line[2:], labels)
				case strings.HasPrefix(line, "- ") && labels[line[2:]]:
					t.Errorf("completions offer %q", line[2:])
				}
			}
			continue
		case "signature":
			got = "none"
			if sh, ok := SignatureAt(module, src, opts, offset); ok {
				got = fmt.Sprintf("%s @ %d", sh.Signature.Label, sh.Active)
			}
		case "references":
			if !found || ref.Def == nil {
				got = "none"
				break
			}
			var lines []string
			for _, r := range a.References(*ref.Def) {
				lines = append(lines, where(src, r.Location))
			}
			got = strings.Join(lines, "\n")
		case "rename":
			got = "none"
			if found && ref.Def != nil {
				if got = a.RenameConflict(*ref.Def); got == "" {
					got = "ok"
				}
			}
		case "symbols":
			got = strings.Join(outline(a.Symbols(module), ""), "\n")
		case "diagnostics":
			var lines []string
			for _, d := range a.Diagnostics {
				lines = append(lines, d.Error())
			}
			got = strings.Join(lines, "\n")
			if got == "" {
				got = "none"
			}
		default:
			t.Fatalf("unknown section %q", section)
		}
		if got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", section, got, want)
		}
	}
}

func TestFormatCases(t *testing.T) {
	files, err := filepath.Glob("testdata/format/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no cases: %v", err)
	}
	for _, file := range files {
		for _, c := range readToolCases(t, file) {
			t.Run(c.file+"/"+c.name, func(t *testing.T) {
				in, want := c.sections["in"]+"\n", c.sections["want"]
				got, err := Format(in)
				if want == "error" {
					if err == nil {
						t.Fatalf("formatted:\n%s\nwant an error", got)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got != want+"\n" {
					t.Fatalf("got:\n%s\nwant:\n%s", got, want)
				}
				again, _ := Format(got)
				if again != got {
					t.Fatalf("formatting again changed it:\n%s", again)
				}
				// Formatting changes no meaning: the tokens stay the same
				before, _, _ := lexScript(in)
				after, _, _ := lexScript(got)
				if len(before) != len(after) {
					t.Fatalf("formatting changed the tokens")
				}
				for i := range before {
					if before[i].text != after[i].text || before[i].num != after[i].num {
						t.Fatalf("token %d changed: %q → %q", i, before[i].text, after[i].text)
					}
				}
			})
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "speed", "_x", "Bell2"} {
		if !ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1a", "let", "min", "a-b", "__alo_probe__"} {
		if ValidName(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

func TestHostModulesAreDefinitionFiles(t *testing.T) {
	// Analysed on their own, their names are recorded, with doc comments and definitions in themselves
	a := Analyze("test", MapSource{}, ScriptOptions{Hosts: []*HostModule{probeHost}})
	if len(a.Diagnostics) > 0 {
		t.Fatal(DiagnosticsError(a.Diagnostics))
	}
	at := strings.Index(probeHost.Source, "Probe {") + 1
	ref, ok := a.At("test", at)
	if !ok || ref.Kind != RefClass || ref.Def == nil || ref.Def.Module != "test" || !ref.Decl {
		t.Fatalf("Probe in its own file: %+v, %v", ref, ok)
	}
	var names []string
	for _, s := range a.Symbols("test") {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "log,probe,Probe,Greeter" {
		t.Fatalf("outline = %v", names)
	}
	// Anything but declarations is refused
	for _, src := range []string{
		"export class Plain { x = 1 }",
		"export function f(): number { return 1 }",
		"export const X = 1",
	} {
		h := &HostModule{Name: "bad", Source: src}
		if err := h.Check(); err == nil || !strings.Contains(err.Error(), "definition file") {
			t.Errorf("%q: %v, want it refused", src, err)
		}
	}
}
