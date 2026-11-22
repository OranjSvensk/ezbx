package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/detect"
)

func TestEmbeddedManifestsLoad(t *testing.T) {
	list, err := LoadFS(harnesses.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "opencode" || list[1].ID != "pi" {
		t.Fatalf("want opencode and pi, got %d manifests", len(list))
	}
}

func TestRenderOpenCodeConfig(t *testing.T) {
	list, err := LoadFS(harnesses.FS)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := Find(list, "opencode")
	files, err := Render(oc, Values{
		GatewayBaseURL: "http://gateway:8000/v1",
		GatewayKey:     "ezbx-placeholder",
		Models:         Models(strataModels(), Defaults{}, nil),
		Default:        "small-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != ".config/opencode/opencode.json" || files[0].Mode != 0o600 {
		t.Fatalf("files: %+v", files)
	}
	var cfg struct {
		Model     string
		Update    string
		Providers map[string]struct {
			Package  string
			Settings struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			}
			Models map[string]struct {
				Name         string
				Capabilities struct {
					Tools bool
					Input []string
				}
				Limit struct{ Context, Output int }
			}
		}
	}
	if err := json.Unmarshal(files[0].Data, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, files[0].Data)
	}
	p := cfg.Providers["ezbx"]
	if cfg.Model != "ezbx/small-model" || cfg.Update != "disable" || p.Package != "@opencode/ai/providers/openai-compatible" ||
		p.Settings.BaseURL != "http://gateway:8000/v1" || p.Settings.APIKey != "ezbx-placeholder" || len(p.Models) != 2 {
		t.Fatalf("config: %s", files[0].Data)
	}
	if m := p.Models["big-model"]; m.Name != "big-model" || !m.Capabilities.Tools || strings.Join(m.Capabilities.Input, ",") != "text,image" || m.Limit.Context != 262144 || m.Limit.Output != 8192 {
		t.Errorf("big-model: %+v", m)
	}
	if strings.Contains(string(files[0].Data), "${") {
		t.Error("a placeholder was left unfilled")
	}
}

const goodManifest = `{
  "schema": 1, "id": "demo", "name": "Demo", "kind": "coding",
  "install": {"type": "npm", "package": "demo", "version": "1.2.3", "script": "install.sh", "lockfile": "package-lock.json"},
  "binary": "demo", "runtime": "node", "env": {"DEMO_QUIET": "1"},
  "config": [{"path": "~/.demo/config.json", "mode": "0600", "content": {"url": "${gateway.baseUrl}"}}],
  "launch": {"command": ["demo"], "cwd": "/workspace"},
  "autonomy": {"flag": null, "note": ""},
  "network": {"domains": [], "optional": {}},
  "secrets": [],
  "verified": {"version": "1.2.3", "status": "unverified", "observedOn": "", "reverify": false, "notes": ""}
}`

func loadOne(body string) error {
	_, err := LoadFS(fstest.MapFS{"demo/manifest.json": {Data: []byte(body)}})
	return err
}

func TestManifestValidation(t *testing.T) {
	if err := loadOne(goodManifest); err != nil {
		t.Fatalf("a good manifest must load: %v", err)
	}
	bad := map[string][2]string{
		"version range":         {`"version": "1.2.3"`, `"version": "^1.2.3"`},
		"version latest":        {`"version": "1.2.3"`, `"version": "latest"`},
		"npm without lockfile":  {`, "lockfile": "package-lock.json"`, ``},
		"unknown field":         {`"binary": "demo"`, `"binary": "demo", "binaray": "x"`},
		"secret-looking env":    {`"DEMO_QUIET": "1"`, `"DEMO_API_KEY": "1"`},
		"placeholder in env":    {`"DEMO_QUIET": "1"`, `"DEMO_URL": "${gateway.baseUrl}"`},
		"config outside home":   {`"~/.demo/config.json"`, `"/etc/demo.json"`},
		"config climbs out":     {`"~/.demo/config.json"`, `"~/../root/x.json"`},
		"config with backslash": {`"~/.demo/config.json"`, `"~/.demo\\x.json"`},
		"executable mode":       {`"mode": "0600"`, `"mode": "0700"`},
		"world-writable mode":   {`"mode": "0600"`, `"mode": "0602"`},
		"short mode":            {`"mode": "0600"`, `"mode": "600"`},
		"bad kind":              {`"kind": "coding"`, `"kind": "toy"`},
		"bad status":            {`"status": "unverified"`, `"status": "works"`},
		"id not folder":         {`"id": "demo"`, `"id": "other"`},
		"bad tool id":           {`"kind": "coding"`, `"kind": "coding", "tools": ["Rip Grep"]`},
		"proxy env":             {`"DEMO_QUIET": "1"`, `"HTTPS_PROXY": "http://elsewhere"`},
		"no-proxy env":          {`"DEMO_QUIET": "1"`, `"NO_PROXY": "*"`},
		"path env":              {`"DEMO_QUIET": "1"`, `"PATH": "/tmp"`},
		"binary with a space":   {`"binary": "demo"`, `"binary": "demo x"`},
		"binary with a slash":   {`"binary": "demo"`, `"binary": "../demo"`},
		"binary with a quote":   {`"binary": "demo"`, `"binary": "demo'"`},
		"no binary":             {`"binary": "demo"`, `"binary": ""`},
	}
	for name, sub := range bad {
		body := strings.Replace(goodManifest, sub[0], sub[1], 1)
		if body == goodManifest {
			t.Fatalf("%s: substitution did nothing", name)
		}
		if err := loadOne(body); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func strataModels() []detect.Model {
	loaded := true
	return []detect.Model{
		{ID: "big-model", ContextWindow: 262144, Input: []string{"text", "image"}, Loaded: &loaded},
		{ID: "small-model"},
	}
}

func TestModelsFillsDefaultsAndOverrides(t *testing.T) {
	yes := true
	got := Models(strataModels(), Defaults{MaxTokens: 4096}, map[string]Override{
		"small-model": {Name: "Small", ContextWindow: 2048, Reasoning: &yes},
	})
	big, small := got[0], got[1]
	if big.ContextWindow != 262144 || big.MaxTokens != 4096 || big.Reasoning || strings.Join(big.Input, ",") != "text,image" || big.Name != "big-model" {
		t.Errorf("big: %+v", big)
	}
	// The override's 2048 context is smaller than the default 4096 reply, so
	// the reply limit comes down to fit.
	if small.Name != "Small" || small.ContextWindow != 2048 || small.MaxTokens != 2048 || !small.Reasoning || strings.Join(small.Input, ",") != "text" {
		t.Errorf("small: %+v", small)
	}
	if def := Models([]detect.Model{{ID: "x"}}, Defaults{}, nil)[0]; def.ContextWindow != 32768 || def.MaxTokens != 8192 {
		t.Errorf("built-in defaults: %+v", def)
	}
}

func TestRenderPiConfig(t *testing.T) {
	list, err := LoadFS(harnesses.FS)
	if err != nil {
		t.Fatal(err)
	}
	pi, _ := Find(list, "pi")
	files, err := Render(pi, Values{
		GatewayBaseURL: "http://gateway:8000/v1",
		GatewayKey:     "ezbx-placeholder",
		Models:         Models(strataModels(), Defaults{Reasoning: true}, nil),
		Default:        "big-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != ".pi/agent/models.json" || files[1].Path != ".pi/agent/settings.json" || files[0].Mode != 0o600 {
		t.Fatalf("files: %+v", files)
	}

	var models struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID            string   `json:"id"`
				ContextWindow int      `json:"contextWindow"`
				MaxTokens     int      `json:"maxTokens"`
				Reasoning     bool     `json:"reasoning"`
				Input         []string `json:"input"`
				Cost          map[string]int
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(files[0].Data, &models); err != nil {
		t.Fatalf("models.json is not valid JSON: %v\n%s", err, files[0].Data)
	}
	p := models.Providers["ezbx"]
	if p.BaseURL != "http://gateway:8000/v1" || p.APIKey != "ezbx-placeholder" || len(p.Models) != 2 {
		t.Fatalf("provider: %+v", p)
	}
	// Placeholders that are a whole value keep their type.
	if m := p.Models[0]; m.ID != "big-model" || m.ContextWindow != 262144 || m.MaxTokens != 8192 || !m.Reasoning || strings.Join(m.Input, ",") != "text,image" || m.Cost["input"] != 0 {
		t.Errorf("first model: %+v", m)
	}
	if strings.Contains(string(files[0].Data), "${") || strings.Contains(string(files[1].Data), "${") {
		t.Error("a placeholder was left unfilled")
	}

	var settings map[string]any
	if err := json.Unmarshal(files[1].Data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["defaultModel"] != "big-model" || settings["defaultProvider"] != "ezbx" {
		t.Errorf("settings: %v", settings)
	}
}

func render(t *testing.T, content string) ([]File, error) {
	t.Helper()
	m := Manifest{Name: "Demo", Config: []ConfigFile{{Path: "~/.demo/c.json", Mode: "0600", Content: json.RawMessage(content)}}}
	return Render(m, Values{GatewayBaseURL: "http://gateway:8000/v1", GatewayKey: "k", Models: Models(strataModels(), Defaults{}, nil)})
}

func TestRenderTemplateRules(t *testing.T) {
	files, err := render(t, `{"a": "${gateway.baseUrl}/chat", "n": 5, "list": {"$each": "models", "as": "${id}:${contextWindow}"}, "def": "${models.default.id}"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(files[0].Data, &got)
	if got["a"] != "http://gateway:8000/v1/chat" || got["n"] != float64(5) || got["def"] != "big-model" {
		t.Errorf("rendered: %v", got)
	}
	if l, _ := got["list"].([]any); len(l) != 2 || l[0] != "big-model:262144" || l[1] != "small-model:32768" {
		t.Errorf("$each with text: %v", got["list"])
	}

	bad := map[string]string{
		"unknown placeholder":       `{"a": "${gateway.password}"}`,
		"model fact outside $each":  `{"a": "${id}"}`,
		"list inside a string":      `{"list": {"$each": "models", "as": "x ${input}"}}`,
		"unknown $ key":             `{"$if": true}`,
		"$each over something else": `{"x": {"$each": "secrets", "as": "${id}"}}`,
		"$each with extra keys":     `{"x": {"$each": "models", "as": "${id}", "sort": true}}`,
		"nested $each":              `{"x": {"$each": "models", "as": {"y": {"$each": "models", "as": "${id}"}}}}`,
		"placeholder in a key":      `{"${id}": 1}`,
		"unfinished placeholder":    `{"a": "${gateway.baseUrl"}`,
	}
	for name, content := range bad {
		if _, err := render(t, content); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRenderKeyedEach(t *testing.T) {
	files, err := render(t, `{"models": {"$each": "models", "key": "m-${id}", "as": {"name": "${name}", "limit": {"context": "${contextWindow}"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Models map[string]struct {
			Name  string
			Limit struct{ Context int }
		}
	}
	if err := json.Unmarshal(files[0].Data, &got); err != nil {
		t.Fatalf("%v\n%s", err, files[0].Data)
	}
	if len(got.Models) != 2 || got.Models["m-big-model"].Name != "big-model" || got.Models["m-big-model"].Limit.Context != 262144 || got.Models["m-small-model"].Limit.Context != 32768 {
		t.Errorf("keyed $each: %s", files[0].Data)
	}

	// Two models with the same name would give the same key.
	twin := Manifest{Name: "Demo", Config: []ConfigFile{{Path: "~/.demo/c.json", Mode: "0600", Content: json.RawMessage(`{"m": {"$each": "models", "key": "${name}", "as": 1}}`)}}}
	models := Models(strataModels(), Defaults{}, map[string]Override{"small-model": {Name: "big-model"}})
	if _, err := Render(twin, Values{Models: models}); err == nil || !strings.Contains(err.Error(), "for two models") {
		t.Errorf("duplicate keys: %v", err)
	}

	bad := map[string]string{
		"key that is a number": `{"m": {"$each": "models", "key": "${contextWindow}", "as": 1}}`,
		"key that is a list":   `{"m": {"$each": "models", "key": "${input}", "as": 1}}`,
		"key that isn't text":  `{"m": {"$each": "models", "key": 5, "as": 1}}`,
		"empty key":            `{"m": {"$each": "models", "key": "", "as": 1}}`,
		"unknown key fact":     `{"m": {"$each": "models", "key": "${colour}", "as": 1}}`,
		"key with extra keys":  `{"m": {"$each": "models", "key": "${id}", "as": 1, "sort": true}}`,
	}
	want := map[string]string{
		"key that is a number": "must give text",
		"key that is a list":   "must give text",
		"key that isn't text":  "must be text",
		"empty key":            "must be text",
		"unknown key fact":     "unknown placeholder",
		"key with extra keys":  `optionally "key"`,
	}
	for name, content := range bad {
		if _, err := render(t, content); err == nil || !strings.Contains(err.Error(), want[name]) {
			t.Errorf("%s: got %v, want an error with %q", name, err, want[name])
		}
	}
}

func TestRenderNeedsModels(t *testing.T) {
	m := Manifest{Name: "Demo", Config: []ConfigFile{{Path: "~/.demo/c.json", Mode: "0600", Content: json.RawMessage(`{}`)}}}
	if _, err := Render(m, Values{}); err == nil {
		t.Error("rendering with no models should fail with a clear message")
	}
	if _, err := Render(m, Values{Models: []Model{{ID: "a"}}, Default: "b"}); err == nil {
		t.Error("a default model the engine doesn't report should be refused")
	}
}
