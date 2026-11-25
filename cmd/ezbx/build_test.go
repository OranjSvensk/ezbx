package main

import (
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/images"
)

func tags(specs []images.Spec) string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Tag())
	}
	return strings.Join(out, " ")
}

// ezbx build builds every box's image, or only the named box's, and --prune
// always keeps every box's current image.
func TestBuildSets(t *testing.T) {
	cfg, err := config.Parse("version = 1\n" +
		"[box.alpha]\nworkspace = '/a'\nharnesses = ['pi']\ntools = []\n" +
		"[box.beta]\nworkspace = '/b'\nharnesses = ['opencode']\ntools = []\n" +
		"[box.gamma]\nworkspace = '/c'\nharnesses = ['pi']\ntools = []\n")
	if err != nil {
		t.Fatal(err)
	}
	gw, alpha, err := imageSpecs(nil, []string{"pi"})
	if err != nil {
		t.Fatal(err)
	}
	_, beta, _ := imageSpecs(nil, []string{"opencode"})
	if alpha.Tag() == beta.Tag() || !strings.HasPrefix(gw.Tag(), "ezbx-gateway:") {
		t.Fatalf("test images: %s %s %s", gw.Tag(), alpha.Tag(), beta.Tag())
	}
	cases := map[string]struct{ build, current string }{
		// alpha and gamma need the same image: it appears once.
		"":     {gw.Tag() + " " + alpha.Tag() + " " + beta.Tag(), gw.Tag() + " " + alpha.Tag() + " " + beta.Tag()},
		"beta": {gw.Tag() + " " + beta.Tag(), gw.Tag() + " " + beta.Tag() + " " + alpha.Tag()},
	}
	for only, want := range cases {
		build, current, err := buildSets(cfg, only)
		if err != nil {
			t.Fatal(only, err)
		}
		if got := tags(build); got != want.build {
			t.Errorf("--box %q builds %s, want %s", only, got, want.build)
		}
		if got := tags(current); got != want.current {
			t.Errorf("--box %q keeps %s, want %s", only, got, want.current)
		}
	}
	if _, _, err := buildSets(cfg, "delta"); err == nil || !strings.Contains(err.Error(), "alpha, beta, gamma") {
		t.Errorf("a box the config lacks: %v", err)
	}
	// No config: one box image with every verified harness.
	build, current, err := buildSets(nil, "")
	if err != nil || len(build) != 2 || tags(build) != tags(current) || build[1].Tag() == alpha.Tag() {
		t.Errorf("no config: %s / %s %v", tags(build), tags(current), err)
	}
}
