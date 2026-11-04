package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/krotovic/kiln/language"
)

const bellHost = `
export interface Ringer {
  ring(times: number): string;
}
export native function log(msg: string): void
`

func bellRequest() Request {
	return Request{
		Hosts: []Host{{Name: "bell", Source: bellHost}, {Name: "gong", Source: "export interface Gong { strike(): void; }\n"}},
		Scripts: map[string]string{
			"script/main": `import { Ringer, log } from "bell";
import { twice } from "script/util";
export default class Tower implements Ringer {
  ring(times: number): string { log("ring"); return "dong x" + twice(times); }
}
`,
			"script/util":   "export function twice(n: number): number { return n * 2; }\n",
			"script/broken": "export function f(): number { return \"no\"; }\n",
			"script/args":   "export default class Gate { constructor(n: number) {} }\n",
		},
	}
}

func TestCompileMatchesTheLibrary(t *testing.T) {
	req := bellRequest()
	req.Imports = []string{"script/main", "script/util", "script/nothing"}
	req.Jobs = []Job{{Entry: "script/main", Satisfies: []string{"bell.Ringer"}}}
	resp, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	// The host that runs it, with the code of its natives
	bell := &language.HostModule{Name: "bell", Source: bellHost, Natives: map[string]language.NativeFunc{
		"log": func(*language.Call, language.Value, []language.Value) (language.Value, error) {
			return language.Value{}, nil
		},
	}}
	want, diags := language.CompileScript("script/main", language.MapSource(req.Scripts), language.ScriptOptions{Hosts: []*language.HostModule{bell}})
	if len(diags) > 0 {
		t.Fatal(diags)
	}
	got := resp.Results[0]
	if !bytes.Equal(got.Module, want.Module()) || len(got.Diagnostics) > 0 {
		t.Fatalf("the module differs from CompileScript's: %+v", got.Diagnostics)
	}
	if strings.Join(got.Modules, ",") != "script/main,script/util" && strings.Join(got.Modules, ",") != "script/util,script/main" {
		t.Errorf("modules = %v", got.Modules)
	}
	if c := got.DefaultClass; c == nil || c.Name != "Tower" || c.ConstructorParams != 0 || len(c.Unsatisfied) != 0 {
		t.Errorf("default class = %+v", c)
	}
	if strings.Join(resp.Imports["script/main"], ",") != "bell,script/util" || len(resp.Imports["script/util"]) != 0 ||
		resp.Imports["script/nothing"] == nil {
		t.Errorf("imports = %v", resp.Imports)
	}
	// The module loads against the same host module, as a host's runtime loads it
	script, err := language.LoadScript(got.Module, language.ScriptOptions{Hosts: []*language.HostModule{bell}})
	if err != nil {
		t.Fatal(err)
	}
	script.Close()

}

func TestCompileReportsProblems(t *testing.T) {
	req := bellRequest()
	req.Jobs = []Job{
		{Entry: "script/broken"},
		{Entry: "script/main", Hosts: []string{"gong"}, Refuse: map[string]string{"bell": `"bell" rings only in towers`}},
		{Entry: "script/args", Satisfies: []string{"bell.Ringer"}},
		{Entry: "script/missing"},
	}
	resp, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if d := resp.Results[0].Diagnostics; len(d) == 0 || d[0].Module != "script/broken" || d[0].Line != 1 || resp.Results[0].Module != nil {
		t.Errorf("a script that doesn't type-check: %+v", resp.Results[0])
	}
	if d := resp.Results[1].Diagnostics; len(d) == 0 || !strings.Contains(d[0].Msg, "rings only in towers") {
		t.Errorf("a refused module: %+v", d)
	}
	if c := resp.Results[2].DefaultClass; c == nil || c.Name != "Gate" || c.ConstructorParams != 1 || c.Unsatisfied["bell.Ringer"] == "" {
		t.Errorf("a class that rings not: %+v", c)
	}
	if d := resp.Results[3].Diagnostics; len(d) == 0 {
		t.Errorf("a missing script compiled: %+v", resp.Results[3])
	}
}

func TestCompileRefusesBadRequests(t *testing.T) {
	for name, req := range map[string]Request{
		"a host that doesn't check": {Hosts: []Host{{Name: "bell", Source: "export function f(): number { return 1; }"}}},
		"a host given twice":        {Hosts: []Host{{Name: "bell", Source: bellHost}, {Name: "bell", Source: bellHost}}},
		"an unknown host":           {Hosts: []Host{{Name: "bell", Source: bellHost}}, Jobs: []Job{{Entry: "script/x", Hosts: []string{"gong"}}}},
		"an unknown interface":      {Hosts: []Host{{Name: "bell", Source: bellHost}}, Jobs: []Job{{Entry: "script/x", Satisfies: []string{"bell.Gong"}}}},
		"an interface without host": {Hosts: []Host{{Name: "bell", Source: bellHost}}, Jobs: []Job{{Entry: "script/x", Satisfies: []string{"Ringer"}}}},
	} {
		if _, err := Compile(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRunSpeaksJSON(t *testing.T) {
	req := bellRequest()
	req.Jobs = []Job{{Entry: "script/util"}}
	in, _ := json.Marshal(req)
	var out bytes.Buffer
	if err := run(bytes.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Results []struct {
			Module  string   `json:"module"`
			Modules []string `json:"modules"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || len(resp.Results) != 1 || resp.Results[0].Module == "" ||
		strings.Join(resp.Results[0].Modules, ",") != "script/util" {
		t.Fatalf("response = %s (%v)", out.String(), err)
	}
	if err := run(strings.NewReader(`{"hostz":[]}`), &out); err == nil {
		t.Error("an unknown field was taken")
	}
}

func TestCompileDescribesHostModules(t *testing.T) {
	req := Request{
		Hosts: []Host{{Name: "bell", Source: bellHost + `
export interface Loud extends Ringer {
  readonly volume: number
}
export native class Clapper {
  readonly weight: number
  swing(times: number): void
}
`}},
		Describe: []string{"bell"},
	}
	resp, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	d := resp.Hosts["bell"]
	if strings.Join(d.Natives, ",") != "Clapper.swing,Clapper.weight,log" {
		t.Errorf("natives = %v", d.Natives)
	}
	// The interfaces as the Go package describes them, those they extend included
	want, err := (&language.HostModule{Name: "bell", Source: req.Hosts[0].Source, Declarations: true}).Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(d.Interfaces)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) || len(d.Interfaces["Loud"].Members) != 2 {
		t.Errorf("interfaces = %s, want %s", got, wantJSON)
	}
	if _, err := Compile(Request{Hosts: req.Hosts, Describe: []string{"gong"}}); err == nil {
		t.Error("an unknown host module was described")
	}
}
