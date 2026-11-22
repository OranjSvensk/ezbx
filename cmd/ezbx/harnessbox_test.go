package main

import (
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/harness"
)

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
	for _, m := range all.Harnesses {
		if m.Verified.Status != "verified" {
			t.Errorf("%s is not verified but was built in", m.ID)
		}
	}
	_, pi, err := imageSpecs(nil, []string{"pi"})
	if err != nil || len(pi.Harnesses) != 1 || pi.Harnesses[0].ID != "pi" {
		t.Fatalf("pi only: %v %v", pi.Harnesses, err)
	}
	if strings.Contains(strings.Join(pi.Files(), " "), "harnesses/opencode/") {
		t.Error("a Pi-only box must not carry OpenCode's files")
	}
	if _, _, err := imageSpecs(nil, []string{"nope"}); err == nil || !strings.Contains(err.Error(), `unknown harness "nope"`) {
		t.Errorf("unknown: %v", err)
	}
}
