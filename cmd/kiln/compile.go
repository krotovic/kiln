package main

import (
	"fmt"
	"sort"

	"github.com/krotovic/kiln/language"
)

// Request is what to compile: the host modules by their definition files, the scripts' sources by ID, and the
// jobs. Imports lists scripts whose imports the response gives.
type Request struct {
	Hosts   []Host            `json:"hosts"`
	Scripts map[string]string `json:"scripts"`
	Imports []string          `json:"imports,omitempty"`
	Jobs    []Job             `json:"jobs"`
}

// Host is a host module: its name, as scripts import it, and its definition file.
type Host struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Job compiles the script Entry against the host modules Hosts names (every one when empty). A module named in
// Refuse (and not among the job's host modules) can't be imported, with its message as the reason. Satisfies names interfaces of host modules
// ("host.Interface") that the result says whether the default class satisfies.
type Job struct {
	Entry     string            `json:"entry"`
	Hosts     []string          `json:"hosts,omitempty"`
	Refuse    map[string]string `json:"refuse,omitempty"`
	Satisfies []string          `json:"satisfies,omitempty"`
}

// Response has the imports asked for, by script (none for a script that doesn't parse), and a result per job.
type Response struct {
	Imports map[string][]string `json:"imports"`
	Results []Result            `json:"results"`
}

// Result is a compiled script: its WebAssembly module (base64 in JSON), the IDs of its modules (the entry and the
// scripts it imports), and its default class; or the diagnostics of a script that doesn't compile.
type Result struct {
	Entry        string                `json:"entry"`
	Module       []byte                `json:"module,omitempty"`
	Modules      []string              `json:"modules,omitempty"`
	DefaultClass *DefaultClass         `json:"default_class,omitempty"`
	Diagnostics  []language.Diagnostic `json:"diagnostics,omitempty"`
}

// DefaultClass is the class the entry exports as default: its name, how many arguments its constructor takes, and
// why it doesn't satisfy each interface the job named that it doesn't satisfy.
type DefaultClass struct {
	Name              string            `json:"name"`
	ConstructorParams int               `json:"constructor_params"`
	Unsatisfied       map[string]string `json:"unsatisfied,omitempty"`
}

// Compile runs a request. Problems in a script are its result's diagnostics; an error is a request that can't be
// run: a host module that doesn't check, or a job naming a host or an interface there isn't.
func Compile(req Request) (*Response, error) {
	hosts := map[string]*language.HostModule{}
	var all []*language.HostModule
	for _, h := range req.Hosts {
		if _, dup := hosts[h.Name]; dup {
			return nil, fmt.Errorf("the host module %q is given twice", h.Name)
		}
		m := &language.HostModule{Name: h.Name, Source: h.Source, Declarations: true}
		if err := m.Check(); err != nil {
			return nil, fmt.Errorf("the host module %q: %w", h.Name, err)
		}
		hosts[h.Name] = m
		all = append(all, m)
	}
	resp := &Response{Imports: map[string][]string{}, Results: []Result{}}
	for _, id := range req.Imports {
		imports, err := language.ModuleImports(req.Scripts[id])
		if err != nil || imports == nil {
			imports = []string{}
		}
		resp.Imports[id] = imports
	}
	for _, job := range req.Jobs {
		opts := language.ScriptOptions{Hosts: all}
		if len(job.Hosts) > 0 {
			opts.Hosts = nil
			for _, name := range job.Hosts {
				h, ok := hosts[name]
				if !ok {
					return nil, fmt.Errorf("%s: no host module %q", job.Entry, name)
				}
				opts.Hosts = append(opts.Hosts, h)
			}
		}
		ifaces := map[string]*language.Interface{}
		for _, name := range job.Satisfies {
			i, err := hostInterface(hosts, name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", job.Entry, err)
			}
			ifaces[name] = i
		}
		src := source{scripts: req.Scripts, refuse: job.Refuse}
		script, diags := language.CompileScript(job.Entry, src, opts)
		if len(diags) > 0 {
			resp.Results = append(resp.Results, Result{Entry: job.Entry, Diagnostics: diags})
			continue
		}
		res := Result{Entry: job.Entry, Module: script.Module(), Modules: script.Modules()}
		if class, ok := script.DefaultClass(); ok {
			res.DefaultClass = &DefaultClass{Name: class.Name(), ConstructorParams: class.ConstructorParams()}
			names := make([]string, 0, len(ifaces))
			for name := range ifaces {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if why := class.Satisfies(ifaces[name]); why != "" {
					if res.DefaultClass.Unsatisfied == nil {
						res.DefaultClass.Unsatisfied = map[string]string{}
					}
					res.DefaultClass.Unsatisfied[name] = why
				}
			}
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

// hostInterface finds "host.Interface".
func hostInterface(hosts map[string]*language.HostModule, name string) (*language.Interface, error) {
	for i := len(name) - 1; i > 0; i-- {
		if name[i] != '.' {
			continue
		}
		h, ok := hosts[name[:i]]
		if !ok {
			return nil, fmt.Errorf("no host module %q", name[:i])
		}
		return h.Interface(name[i+1:])
	}
	return nil, fmt.Errorf("%q names no interface: write it host.Interface", name)
}

// source loads the request's scripts; a refused module fails with its reason.
type source struct {
	scripts map[string]string
	refuse  map[string]string
}

func (s source) Load(id string) (string, error) {
	if why, ok := s.refuse[id]; ok {
		return "", fmt.Errorf("%s", why)
	}
	return language.MapSource(s.scripts).Load(id)
}
