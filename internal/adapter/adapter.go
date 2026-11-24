// Package adapter loads and checks the API kind files
// (providers/<id>/adapter.json): how ezbx recognises a model server by the
// API it speaks, and where it reads model facts. See docs/engine-adapters.md.
package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Adapter describes one kind of model server API.
type Adapter struct {
	Schema       int      `json:"schema"`
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Detect       Detect   `json:"detect"`
	OpenAI       OpenAI   `json:"openai"`
	Models       Models   `json:"models"`
	Auth         Auth     `json:"auth"`
	Capabilities []string `json:"capabilities"`
	Verified     Verified `json:"verified"`
}

// Detect says how to recognise the API. HintPorts are common local ports
// that ezbx init and ezbx engine detect look at; nothing else scans.
type Detect struct {
	HintPorts []int   `json:"hintPorts"`
	Probes    []Probe `json:"probes"`
}

// Probe is one request used to recognise the API.
type Probe struct {
	Path     string `json:"path"`
	Expect   Expect `json:"expect"`
	Optional bool   `json:"optional"`
}

// Expect lists what a reply must contain. HasFields must lead to real values
// (a path with "[]" needs a non-empty list). HasLists must be lists, which may
// be empty, so a server with no models is still recognised.
type Expect struct {
	HasFields []string `json:"hasFields"`
	HasLists  []string `json:"hasLists"`
}

// OpenAI holds the paths of the OpenAI-style API.
type OpenAI struct {
	BasePath   string `json:"basePath"`
	ModelsPath string `json:"modelsPath"`
	ChatPath   string `json:"chatPath"`
}

// Models says where to read model facts from the model list.
// A nil entry means the API does not report that fact.
//
// Loaded has one of two forms:
//
//	"data[].status.value == loaded"   a field in the model list equals a value
//	"/api/ps models[].name"           the model's id is listed by another route
//
// List is the route the facts are read from, when it isn't the OpenAI model
// list (openai.modelsPath): a server's own list often says more. Its ids
// must be the ones the chat route accepts.
//
// Skip leaves out models that can't chat, such as embedding models, in the
// form "models[].type == embedding": a field in the list equals a value.
type Models struct {
	List          string  `json:"list,omitempty"`
	ID            string  `json:"id"`
	ContextWindow *string `json:"contextWindow"`
	Input         *string `json:"input"`
	Loaded        *string `json:"loaded"`
	Skip          *string `json:"skip,omitempty"`
}

// ListPath is the route the model facts are read from.
func (a Adapter) ListPath() string {
	if a.Models.List != "" {
		return a.Models.List
	}
	return a.OpenAI.ModelsPath
}

// Auth describes the API's key handling. It never holds a key.
type Auth struct {
	Mode   string  `json:"mode"`
	KeyEnv *string `json:"keyEnv"`
	Note   string  `json:"note"`
}

// IsAnthropicAuth reports whether the API expects Anthropic authentication
// headers (x-api-key and anthropic-version) rather than Authorization: Bearer.
func (a Adapter) IsAnthropicAuth() bool {
	return a.Auth.Mode == "optional-anthropic"
}

// Verified records what has actually been tested.
type Verified struct {
	Status string  `json:"status"`
	On     *string `json:"on"`
	Notes  string  `json:"notes"`
}

// LoadFS reads every <folder>/adapter.json in fsys, checks each one and
// returns them sorted by id. Unknown fields are errors, so typos are caught.
func LoadFS(fsys fs.FS) ([]Adapter, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}

	var out []Adapter
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name() + "/adapter.json"
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		var a Adapter
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := a.validate(entry.Name()); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, a)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a Adapter) validate(folder string) error {
	switch {
	case a.Schema != 1:
		return fmt.Errorf("unsupported schema %d", a.Schema)
	case a.ID == "" || a.ID != folder:
		return fmt.Errorf("id %q must match its folder %q", a.ID, folder)
	case a.Name == "":
		return fmt.Errorf("name is required")
	case !strings.HasPrefix(a.OpenAI.ModelsPath, "/"):
		return fmt.Errorf("openai.modelsPath must start with /")
	case a.Models.ID == "":
		return fmt.Errorf("models.id is required")
	}

	switch a.Auth.Mode {
	case "none", "optional-bearer", "optional-anthropic":
	default:
		return fmt.Errorf("unsupported auth.mode %q", a.Auth.Mode)
	}

	if err := a.Models.validate(); err != nil {
		return err
	}

	required := 0
	for _, p := range a.Detect.Probes {
		if !strings.HasPrefix(p.Path, "/") {
			return fmt.Errorf("probe path %q must start with /", p.Path)
		}
		if len(p.Expect.HasFields)+len(p.Expect.HasLists) == 0 {
			return fmt.Errorf("probe %q expects nothing, so any answer would match", p.Path)
		}
		for _, l := range p.Expect.HasLists {
			if strings.Contains(l, "[]") {
				return fmt.Errorf("probe %q: hasLists takes a plain path such as \"data\", not %q", p.Path, l)
			}
		}
		if !p.Optional {
			required++
		}
	}
	if required == 0 {
		return fmt.Errorf("at least one required probe is needed")
	}
	return nil
}

// validate checks that every model fact uses a form the detector can read.
// A fact it can't read would otherwise be silently ignored.
func (m Models) validate() error {
	list, _, ok := strings.Cut(m.ID, "[]")
	if !ok || list == "" {
		return fmt.Errorf("models.id %q must point into a list, such as data[].id", m.ID)
	}
	prefix := list + "[]."
	if m.List != "" && (!strings.HasPrefix(m.List, "/") || strings.ContainsAny(m.List, " ?#")) {
		return fmt.Errorf("models.list %q must be a route such as /api/v1/models", m.List)
	}
	for name, p := range map[string]*string{"contextWindow": m.ContextWindow, "input": m.Input} {
		if p != nil && !strings.HasPrefix(*p, prefix) {
			return fmt.Errorf("models.%s %q must start with %q, like models.id", name, *p, prefix)
		}
	}
	if m.Skip != nil {
		path, want, found := strings.Cut(*m.Skip, " == ")
		if !found || !strings.HasPrefix(path, prefix) || len(path) == len(prefix) || want == "" {
			return fmt.Errorf("models.skip %q must be \"%sfield == value\"", *m.Skip, prefix)
		}
	}
	if m.Loaded == nil {
		return nil
	}
	if path, want, found := strings.Cut(*m.Loaded, " == "); found {
		if !strings.HasPrefix(path, prefix) || want == "" {
			return fmt.Errorf("models.loaded %q: the field must start with %q and have a value after ==", *m.Loaded, prefix)
		}
		return nil
	}
	route, path, found := strings.Cut(*m.Loaded, " ")
	if !found || !strings.HasPrefix(route, "/") || !strings.Contains(path, "[]") || strings.Contains(path, " ") {
		return fmt.Errorf("models.loaded %q must be \"%sfield == value\" or \"/route list[].field\"", *m.Loaded, prefix)
	}
	return nil
}

// LoadedIn returns the route and list path when Loaded has the
// "/route list[].field" form.
func (m Models) LoadedIn() (route, path string, ok bool) {
	if m.Loaded == nil || strings.Contains(*m.Loaded, " == ") {
		return "", "", false
	}
	return strings.Cut(*m.Loaded, " ")
}
