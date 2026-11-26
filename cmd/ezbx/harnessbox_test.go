package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/harness"
)

func TestRenderPiDefaultAlias(t *testing.T) {
	chosen, err := boxHarnesses(&config.Box{Harnesses: []string{"pi"}})
	if err != nil {
		t.Fatal(err)
	}
	eng := detect.Engine{Models: []detect.Model{{ID: "small"}, {ID: "large", ContextWindow: 65536, Input: []string{"text", "image"}}, {ID: "default"}}}
	cfg := &config.Config{Engine: config.Engine{DefaultModel: "large"}, Models: map[string]config.Model{"large": {MaxTokens: 4096}}}
	files, err := renderHome(cfg, chosen, eng)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct{ DefaultProvider, DefaultModel string }
	var catalog struct {
		Providers map[string]struct{ Models []harness.Model }
	}
	for _, f := range files {
		switch f.Path {
		case ".pi/agent/settings.json":
			err = json.Unmarshal(f.Data, &settings)
		case ".pi/agent/models.json":
			err = json.Unmarshal(f.Data, &catalog)
		}
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
	}
	models := catalog.Providers["ezbx"].Models
	if settings.DefaultProvider != "ezbx" || settings.DefaultModel != "default" || len(models) != 3 || models[0].ID != "small" || models[1].ID != "large" {
		t.Fatalf("settings %+v, models %+v", settings, models)
	}
	if alias := models[2]; alias.ID != "default" || alias.ContextWindow != 65536 || alias.MaxTokens != 4096 || strings.Join(alias.Input, ",") != "text,image" {
		t.Errorf("alias did not inherit the target's facts: %+v", alias)
	}
	cfg.Engine.DefaultModel = "missing"
	if _, err := renderHome(cfg, chosen, eng); err == nil || !strings.Contains(err.Error(), `default model "missing" is not one the engine reports`) {
		t.Errorf("missing target: %v", err)
	}
	cfg.Engine.DefaultModel = ""
	files, err = renderHome(cfg, chosen, eng)
	if err != nil || !strings.Contains(string(files[1].Data), `"defaultModel": "small"`) {
		t.Errorf("alias disabled: %v %v", files, err)
	}
}

func TestRenderDefaultAliasForOtherHarnesses(t *testing.T) {
	for _, tc := range []struct {
		id, path string
		want     []string
	}{
		{"opencode", ".config/opencode/opencode.json", []string{`"model": "ezbx/default"`, `"default": {`, `"large": {`}},
		{"omp", ".omp/agent/config.yml", []string{`defaultModel: "default"`}},
		{"codex", ".codex/config.toml", []string{`model = "default"`}},
		{"claude", ".claude/settings.json", []string{`"model": "default"`, `"ANTHROPIC_MODEL": "default"`, `"ANTHROPIC_DEFAULT_HAIKU_MODEL": "default"`, `"ANTHROPIC_DEFAULT_SONNET_MODEL": "default"`, `"ANTHROPIC_DEFAULT_OPUS_MODEL": "default"`}},
		{"hermes", ".hermes/config.yaml", []string{`default: "default"`, `default_model: "default"`}},
		{"openclaw", ".openclaw/openclaw.json", []string{`"model": "ezbx/default"`, `"ezbx/default": {}`, `"ezbx/large": {}`}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			chosen, err := boxHarnesses(&config.Box{Harnesses: []string{tc.id}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Engine: config.Engine{DefaultModel: "large"}}
			eng := detect.Engine{Models: []detect.Model{{ID: "small"}, {ID: "large"}}}
			files, err := renderHome(cfg, chosen, eng)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.Path == tc.path {
					for _, want := range tc.want {
						if !strings.Contains(string(file.Data), want) {
							t.Errorf("%s lacks %q", file.Path, want)
						}
					}
					return
				}
			}
			t.Errorf("missing %s", tc.path)
		})
	}
}

func TestHarnessEnv(t *testing.T) {
	a := harness.Manifest{ID: "a", Env: map[string]string{"A_QUIET": "1", "SHARED": "x"}}
	b := harness.Manifest{ID: "b", Env: map[string]string{"B_QUIET": "1", "SHARED": "x"}}
	env, err := harnessEnv([]harness.Manifest{a, b})
	if err != nil || len(env) != 3 || env["A_QUIET"] != "1" || env["B_QUIET"] != "1" || env["SHARED"] != "x" {
		t.Fatalf("%v %v", env, err)
	}
	b.Env["SHARED"] = "y"
	if _, err := harnessEnv([]harness.Manifest{a, b}); err == nil || !strings.Contains(err.Error(), "set SHARED differently") {
		t.Errorf("conflict: %v", err)
	}
}

func TestImageSpecsHarnesses(t *testing.T) {
	// Without a config: every verified harness.
	_, all, err := imageSpecs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Harnesses) != 7 {
		t.Fatalf("got %d harnesses, want 7", len(all.Harnesses))
	}
	for _, m := range all.Harnesses {
		if m.Verified.Status != "verified" {
			t.Errorf("%s is not verified but was built in", m.ID)
		}
	}
	_, pi, err := imageSpecs(nil, []string{"pi"})
	if err != nil || len(pi.Harnesses) != 1 || pi.Harnesses[0].ID != "pi" {
		t.Fatalf("pi only: %v %v", pi.Harnesses, err)
	}
	if strings.Contains(strings.Join(pi.Files(), " "), "harnesses/opencode/") || strings.Contains(strings.Join(pi.Files(), " "), "harnesses/hermes/") {
		t.Error("a Pi-only box must not carry other harnesses' files")
	}
	if strings.Contains(strings.Join(pi.Tools.IDs(), ","), "python3") || !strings.Contains(strings.Join(all.Tools.IDs(), ","), "python3") {
		t.Errorf("python3 is only added when a harness or tool pack needs it: pi=%v all=%v", pi.Tools.IDs(), all.Tools.IDs())
	}
	if _, _, err := imageSpecs(nil, []string{"nope"}); err == nil || !strings.Contains(err.Error(), `unknown harness "nope"`) {
		t.Errorf("unknown: %v", err)
	}
}

// The banner is for people at a terminal: help that goes into a buffer (a
// pipe or a script) gets only the usage text.
func TestHelpBannerOnlyAtATerminal(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(errb.String(), "███") || strings.Contains(errb.String(), "\033") {
		t.Errorf("banner in non-terminal output:\n%s", errb.String())
	}
	if !strings.HasPrefix(errb.String(), "ezbx - boxed-up local AI agents") {
		t.Errorf("usage: %.80q", errb.String())
	}
}

// A file that isn't a terminal (help sent to a file) gets no banner either.
func TestBannerSkipsFiles(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	showBanner(f)
	if st, _ := f.Stat(); st.Size() != 0 {
		t.Errorf("wrote %d bytes to a plain file", st.Size())
	}
}

func TestVersionFlags(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v0.1.0"

	for _, arg := range []string{"--version", "-v", "version"} {
		var out, errb strings.Builder
		if code := run([]string{arg}, &out, &errb); code != 0 {
			t.Fatalf("%s exit %d: %s", arg, code, errb.String())
		}
		want := "ezbx v0.1.0 (" + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
		if out.String() != want || errb.Len() != 0 {
			t.Errorf("%s: got out=%q err=%q, want %q", arg, out.String(), errb.String(), want)
		}
	}

	version = ""
	if got := resolveVersion(); got == "" {
		t.Error("resolveVersion returned empty string")
	}
}
