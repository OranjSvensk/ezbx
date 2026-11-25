package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
)

// createHome is an empty ezbx home, with a folder for workspaces.
func createHome(t *testing.T) (home, work string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("EZBX_HOME", home)
	t.Setenv("EZBX_CONFIG", "")
	work = filepath.Join(t.TempDir(), "work")
	return home, work
}

func createJSON(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"create", "--json", "--memory", "4g"}, args...), &out, &errOut)
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("%v: %q / %q", err, out.String(), errOut.String())
	}
	return code, env, errOut.String()
}

func errCode(env map[string]any) string {
	if e, ok := env["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestCreate(t *testing.T) {
	home, work := createHome(t)
	cfgFile := filepath.Join(home, "ezbx.toml")

	// The first box makes the file, with the server address given.
	code, env, stderr := createJSON(t, "--box", "agent-ada", "--workspace", filepath.Join(work, "ada"), "--url", "http://127.0.0.1:18080")
	if code != 0 || env["ok"] != true || env["box"] != "agent-ada" {
		t.Fatalf("first: %d %v %s", code, env, stderr)
	}
	data := env["data"].(map[string]any)
	if data["network"] != "off" || data["configFile"] != cfgFile || strings.Join(toStrings(data["harnesses"]), ",") != "pi" || strings.Join(toStrings(data["tools"]), ",") != "pack:coding" {
		t.Errorf("defaults (the offline profile): %v", data)
	}
	if st, err := os.Stat(filepath.Join(work, "ada")); err != nil || !st.IsDir() {
		t.Error("the workspace wasn't made")
	}

	// A second box shares the engine, with its own settings.
	code, env, stderr = createJSON(t, "--box", "agent-bea", "--workspace", filepath.Join(work, "bea"), "--network", "allow", "--allow", "pypi.org,GitHub.com",
		"--harness", "opencode", "--tools", "none")
	if code != 0 {
		t.Fatalf("second: %d %v %s", code, env, stderr)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Names(), ","); got != "agent-ada,agent-bea" || cfg.Engine.URL != "http://127.0.0.1:18080" {
		t.Errorf("boxes %s, engine %+v", got, cfg.Engine)
	}
	b, _ := cfg.Get("agent-bea")
	if b.Network != "allow" || strings.Join(b.Allow, ",") != "pypi.org,github.com" || b.Harnesses[0] != "opencode" || b.Tools == nil || len(b.Tools) != 0 || b.Memory != "4g" {
		t.Errorf("agent-bea: %+v", b)
	}
	if _, err := os.Stat(cfgFile + ".previous"); err != nil {
		t.Error("the old file wasn't kept")
	}

	cases := []struct {
		name string
		args []string
		code string
	}{
		{"same box again", []string{"--box", "agent-ada", "--workspace", filepath.Join(work, "x")}, "box_exists"},
		{"no box", []string{"--workspace", filepath.Join(work, "x")}, "invalid_args"},
		{"bad box name", []string{"--box", "Agent", "--workspace", filepath.Join(work, "x")}, "invalid_args"},
		{"no workspace", []string{"--box", "c"}, "invalid_args"},
		{"another box's workspace", []string{"--box", "c", "--workspace", filepath.Join(work, "ada")}, "invalid_args"},
		{"inside another box's", []string{"--box", "c", "--workspace", filepath.Join(work, "ada", "sub")}, "invalid_args"},
		{"allow without allow mode", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--allow", "pypi.org"}, "invalid_args"},
		{"bad mode", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--network", "open"}, "invalid_args"},
		{"bad host", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--network", "allow", "--allow", "10.0.0.1"}, "invalid_args"},
		{"new engine address", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--url", "http://127.0.0.1:53000"}, "invalid_args"},
		{"engine key", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--key-env", "K"}, "invalid_args"},
		{"unknown harness", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "--harness", "nope"}, "invalid_args"},
		{"stray word", []string{"--box", "c", "--workspace", filepath.Join(work, "c"), "extra"}, "invalid_args"},
	}
	before, _ := os.ReadFile(cfgFile)
	for _, tc := range cases {
		code, env, _ := createJSON(t, tc.args...)
		if code == 0 || env["ok"] != false || errCode(env) != tc.code {
			t.Errorf("%s: exit %d, %v", tc.name, code, env)
		}
	}
	if after, _ := os.ReadFile(cfgFile); !bytes.Equal(before, after) {
		t.Error("a refused create changed ezbx.toml")
	}
	// The same address as the config's is fine.
	if code, env, _ := createJSON(t, "--box", "c", "--workspace", filepath.Join(work, "c"), "--url", "http://127.0.0.1:18080/", "--network", "airgap"); code != 0 {
		t.Errorf("same address: %v", env)
	}
}

// With one box in ezbx.toml, a missing --box isn't taken to mean that box.
func TestCreateNeedsTheBoxName(t *testing.T) {
	_, work := createHome(t)
	if code, env, _ := createJSON(t, "--box", "main", "--workspace", filepath.Join(work, "m")); code != 0 {
		t.Fatalf("first: %v", env)
	}
	if code, env, _ := createJSON(t, "--workspace", filepath.Join(work, "x")); code != 2 || errCode(env) != "invalid_args" || !strings.Contains(env["error"].(map[string]any)["message"].(string), "--box NAME") {
		t.Errorf("no --box: %d %v", code, env)
	}
	// A read-only folder is taken as given, and a missing one is refused.
	ro := filepath.Join(t.TempDir(), "docs")
	os.MkdirAll(ro, 0o700)
	if code, env, _ := createJSON(t, "--box", "r", "--workspace", filepath.Join(work, "r"), "--readonly", ro); code != 0 || strings.Join(toStrings(env["data"].(map[string]any)["readonly"]), ",") != ro {
		t.Errorf("readonly: %d %v", code, env)
	}
	if code, env, _ := createJSON(t, "--box", "s", "--workspace", filepath.Join(work, "s"), "--readonly", filepath.Join(ro, "gone")); code != 2 || errCode(env) != "invalid_args" {
		t.Errorf("missing readonly: %d %v", code, env)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestStartStopUsage(t *testing.T) {
	createHome(t)
	for _, cmd := range []string{"start", "stop"} {
		var out, errOut bytes.Buffer
		if code := run([]string{cmd, "--json", "extra"}, &out, &errOut); code != 2 || !strings.Contains(out.String(), `"invalid_args"`) {
			t.Errorf("%s: %d %s", cmd, code, out.String())
		}
	}
	// No config: start says so as a JSON error.
	var out, errOut bytes.Buffer
	if code := run([]string{"start", "--json"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), `"ok":false`) || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("start without a config: %d %q", code, out.String())
	}
}

func TestDestroyJSONNeedsYes(t *testing.T) {
	createHome(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"destroy", "--json", "--box", "t"}, &out, &errOut); code != 2 || !strings.Contains(out.String(), "needs --yes") {
		t.Errorf("%d %s", code, out.String())
	}
}
