package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/harness"
)

func TestChangeHarnesses(t *testing.T) {
	known := []harness.Manifest{{ID: "pi"}, {ID: "opencode"}, {ID: "codex"}}
	cases := []struct {
		have, ids []string
		add       bool
		want      string
		changed   bool
		bad       bool
	}{
		{[]string{"pi"}, []string{"codex", "opencode"}, true, "pi,codex,opencode", true, false},
		{[]string{"pi"}, []string{"pi"}, true, "pi", false, false},
		{[]string{"pi", "codex"}, []string{"pi"}, false, "codex", true, false},
		{[]string{"pi"}, []string{"codex"}, false, "pi", false, false},
		{[]string{"pi"}, []string{"pi"}, false, "", false, true}, // the last one
		{[]string{"pi"}, []string{"nope"}, true, "", false, true},
		{[]string{"pi"}, nil, true, "", false, true},
	}
	for _, tc := range cases {
		got, changed, err := changeHarnesses(tc.have, tc.ids, tc.add, known)
		if (err != nil) != tc.bad || strings.Join(got, ",") != tc.want || changed != tc.changed {
			t.Errorf("%v %v add=%v: %v %v %v", tc.have, tc.ids, tc.add, got, changed, err)
		}
		if err != nil {
			if _, ok := err.(usageError); !ok {
				t.Errorf("%v: %T is not a usage error", tc.ids, err)
			}
		}
	}
	// The input list isn't changed in place.
	have := []string{"pi", "codex"}
	changeHarnesses(have, []string{"pi"}, false, known)
	if strings.Join(have, ",") != "pi,codex" {
		t.Errorf("changed in place: %v", have)
	}
}

func TestHarnessCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EZBX_HOME", home)
	t.Setenv("EZBX_CONFIG", "")
	cfgFile := filepath.Join(home, "ezbx.toml")
	os.WriteFile(cfgFile, []byte("version = 1\n[box.a]\nworkspace = '/a'\nharnesses = ['pi']\n[box.b]\nworkspace = '/b'\nnetwork = 'on'\nharnesses = ['pi']\n"), 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"harness", "add", "opencode", "--box", "a"}, &out, &errOut); code != 0 {
		t.Fatalf("add: %d %s", code, errOut.String())
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := cfg.Get("a")
	b, _ := cfg.Get("b")
	if strings.Join(a.Harnesses, ",") != "pi,opencode" || strings.Join(b.Harnesses, ",") != "pi" || b.Network != "on" {
		t.Errorf("after add: a %v, b %+v", a.Harnesses, b)
	}
	if !strings.Contains(out.String(), "ezbx build --box a") {
		t.Errorf("says what to do next: %s", out.String())
	}

	out.Reset()
	if code := run([]string{"harness", "list", "--box", "a", "--json"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	var rows []struct {
		ID    string
		InBox bool `json:"inBox"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	in := map[string]bool{}
	for _, r := range rows {
		in[r.ID] = r.InBox
	}
	if !in["pi"] || !in["opencode"] || in["codex"] || len(rows) < 7 {
		t.Errorf("list: %+v", rows)
	}
	out.Reset()
	run([]string{"harness", "list", "--box", "b"}, &out, &errOut)
	if !strings.Contains(out.String(), "* pi") || strings.Contains(out.String(), "* opencode") {
		t.Errorf("plain list:\n%s", out.String())
	}

	before, _ := os.ReadFile(cfgFile)
	for _, args := range [][]string{
		{"harness", "remove", "pi", "--box", "b"},       // the last one
		{"harness", "add", "nope", "--box", "a"},        // unknown
		{"harness", "add", "codex"},                     // which box?
		{"harness", "add", "codex", "--box", "missing"}, // not in ezbx.toml
		{"harness", "rename", "pi", "--box", "a"},       // no such verb
		{"harness"},
	} {
		if code := run(args, &out, &errOut); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
	if after, _ := os.ReadFile(cfgFile); !bytes.Equal(before, after) {
		t.Error("a refused change wrote ezbx.toml")
	}
	// Nothing to change writes nothing.
	os.Remove(cfgFile + ".previous")
	if code := run([]string{"harness", "add", "pi", "--box", "a"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if _, err := os.Stat(cfgFile + ".previous"); err == nil {
		t.Error("an add that changes nothing rewrote the file")
	}
}

func TestOnlyVerified(t *testing.T) {
	ok := harness.Verified{Status: "verified"}
	got := onlyVerified([]harness.Manifest{
		{ID: "chat", Kind: "assistant", Verified: ok},
		{ID: "wip", Kind: "coding", Verified: harness.Verified{Status: "unverified"}},
		{ID: "code", Kind: "coding", Verified: ok},
	})
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "code,chat" {
		t.Errorf("%v", ids)
	}
}
