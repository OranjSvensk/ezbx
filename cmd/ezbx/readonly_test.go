package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/readonly"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ezbx up checks read-only folders before it contacts any engine. The
// engine address here is a closed port: a folder that passes reaches it and
// fails there instead.
func TestUpChecksReadOnlyFirst(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "cfg", "state")}
	ws := filepath.ToSlash(filepath.Join(root, "ws"))
	ro := filepath.ToSlash(filepath.Join(root, "ref"))
	write(t, root, map[string]string{"ref/a.txt": "x", "ref/.ssh/k": "x"})
	base := "version = 1\n[box.vt]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n" +
		"[engine]\napi = 'openai'\nurl = 'http://127.0.0.1:9'\n"
	cases := map[string]string{
		"[[box.vt.readonly]]\npath = '" + ro + "'\n":                           "Nothing answered at http://127.0.0.1:9",
		"[[box.vt.readonly]]\npath = '" + ro + "/gone'\n":                      "doesn't exist",
		"[[box.vt.readonly]]\npath = '" + ro + "/a.txt'\n":                     "isn't a folder",
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

func TestReadOnlyPlan(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ws := filepath.Join(home, "ws")
	fakeAWS := "AK" + "IA" + strings.Repeat("Q", 16)
	write(t, root, map[string]string{
		"home/Projects/app.txt":       "x",
		"home/Projects/.env":          "x",
		"home/Projects/.npmrc":        "x",
		"home/Projects/.ssh/id":       "x",
		"home/Projects/notes/key.txt": "id = " + fakeAWS,
		"docs/manual.txt":             "x",
		"docs/private/plan.txt":       "x",
	})
	b := &config.Box{ReadOnly: []config.ReadOnly{
		{Path: "~/Projects", Unmask: []string{".npmrc"}},
		{Path: filepath.Join(root, "docs"), Name: "manuals", Mask: []string{"private/"}},
	}}
	var out bytes.Buffer
	ms, err := readOnlyPlan(b, ws, home, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "projects" || ms[1].Name != "manuals" || ms[1].Mask[0] != "private/" || ms[0].Unmask[0] != ".npmrc" {
		t.Fatalf("%+v", ms)
	}
	var rels []string
	for _, m := range ms {
		for _, mk := range m.Masks {
			rels = append(rels, m.Name+":"+mk.Rel)
		}
	}
	if s := strings.Join(rels, ","); s != "projects:.env,projects:.ssh,manuals:private" {
		t.Errorf("masks %q", s)
	}
	text := out.String()
	for _, want := range []string{
		`note: /readonly/projects shows files matching ".npmrc", a default mask, because of unmask`,
		"warning: /readonly/projects/notes/key.txt looks like it holds an AWS access key",
		"/readonly/projects, 2 masked",
		"/readonly/manuals, 1 masked",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q; it was\n%s", want, text)
		}
	}
	if strings.Contains(text, fakeAWS) {
		t.Error("the output shows the secret itself")
	}
	if ms, err := readOnlyPlan(&config.Box{}, ws, home, nil, &out); err != nil || ms != nil {
		t.Errorf("no folders: %v %v", ms, err)
	}
}

func TestCheckMasks(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	ro := filepath.Join(root, "ref")
	write(t, root, map[string]string{"ref/a.txt": "x", "ref/.env": "x"})
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkMasks(state); err != nil {
		t.Fatalf("no record: %v", err)
	}
	b := &config.Box{ReadOnly: []config.ReadOnly{{Path: ro}}}
	ms, err := readOnlyPlan(b, filepath.Join(root, "ws"), filepath.Join(root, "home"), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.WriteMasks(state, ms); err != nil {
		t.Fatal(err)
	}
	if err := checkMasks(state); err != nil {
		t.Fatalf("nothing new: %v", err)
	}
	write(t, root, map[string]string{"ref/sub/server.pem": "x", "ref/other.txt": "x"})
	err = checkMasks(state)
	if err == nil || !strings.Contains(err.Error(), "/readonly/ref/sub/server.pem") || !strings.Contains(err.Error(), "ezbx down, then ezbx up") {
		t.Fatalf("a new secret file: %v", err)
	}
	if strings.Contains(err.Error(), "other.txt") || strings.Contains(err.Error(), "/readonly/ref/.env") {
		t.Errorf("lists more than the new masked file: %v", err)
	}
	if err := os.RemoveAll(ro); err != nil {
		t.Fatal(err)
	}
	if err := checkMasks(state); err == nil || !strings.Contains(err.Error(), "checking read-only folder") {
		t.Fatalf("a folder that went away: %v", err)
	}
}

// A session doesn't start when a read-only folder has a new secret file,
// even with --no-snapshot.
func TestBeforeSessionChecksMasks(t *testing.T) {
	root := t.TempDir()
	tg := target{name: "t", dirs: paths.Dirs{Config: root, State: filepath.Join(root, "state")}}
	state := tg.dirs.Box("t")
	ro := filepath.Join(root, "ref")
	write(t, root, map[string]string{"ref/a.txt": "x"})
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := box.WriteMasks(state, []box.ReadOnlyMount{{Name: "ref", Path: ro}}); err != nil {
		t.Fatal(err)
	}
	if err := beforeSession(tg, true, io.Discard); err != nil {
		t.Fatalf("nothing new: %v", err)
	}
	write(t, root, map[string]string{"ref/.env": "x"})
	if err := beforeSession(tg, true, io.Discard); err == nil || !strings.Contains(err.Error(), "/readonly/ref/.env") {
		t.Fatalf("a new .env: %v", err)
	}
}

func TestReadOnlyPlanMaskLimit(t *testing.T) {
	old := readonly.MaxMasks
	readonly.MaxMasks = 1
	defer func() { readonly.MaxMasks = old }()
	root := t.TempDir()
	write(t, root, map[string]string{"ref/.env": "x", "ref/b/.env": "x"})
	b := &config.Box{ReadOnly: []config.ReadOnly{{Path: filepath.Join(root, "ref")}}}
	if _, err := readOnlyPlan(b, filepath.Join(root, "ws"), filepath.Join(root, "home"), nil, io.Discard); err == nil || !strings.Contains(err.Error(), "need 2 masks, more than 1") {
		t.Fatalf("got %v", err)
	}
}
