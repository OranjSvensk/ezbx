package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
)

// ezbx up checks read-only folders, then refuses them until mounting with
// masks is built, before it contacts any engine. The engine address here
// is a closed port, so reaching it would give a different error.
func TestUpRefusesReadOnlyFirst(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "cfg", "state")}
	ws := filepath.ToSlash(filepath.Join(root, "ws"))
	ro := filepath.ToSlash(filepath.Join(root, "ref"))
	base := "version = 1\n[box.vt]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n" +
		"[engine]\nuse = 'openai'\n[engine.openai]\nurl = 'http://127.0.0.1:9'\n"
	cases := map[string]string{
		"[[box.vt.readonly]]\npath = '" + ro + "'\n":                           "aren't built yet",
		"[[box.vt.readonly]]\npath = '" + ws + "/sub'\n":                       "overlaps the workspace",
		"[[box.vt.readonly]]\npath = '" + ro + "/.ssh'\n":                      `default ".ssh/"`,
		"[[box.vt.readonly]]\npath = '" + filepath.ToSlash(dirs.State) + "'\n": "ezbx's own folder",
	}
	for extra, want := range cases {
		cfg, err := config.Parse(base + extra)
		if err != nil {
			t.Fatal(err)
		}
		_, err = makePlan(context.Background(), cfg, dirs, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", strings.TrimSpace(extra), err, want)
		}
	}
}
