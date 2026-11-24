package readonly

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree makes files (content "x" unless given) under a new folder.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if data == "" {
			data = "x"
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func defaults(t *testing.T) Matcher {
	m, err := NewMatcher(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWalk(t *testing.T) {
	root := tree(t, map[string]string{
		"app.txt":          "",
		".env":             "",
		"sub/.env.local":   "",
		"sub/code.go":      "",
		".ssh/id_ed25519":  "",
		".ssh/known_hosts": "",
		"Upper/.ENV":       "",
		"empty/":           "",
		"big.txt":          strings.Repeat("a", MaxScanSize+1),
		"edge.txt":         strings.Repeat("a", MaxScanSize),
	})
	w, err := Walk(root, defaults(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range w.Masks {
		got = append(got, fmt.Sprintf("%s dir=%v link=%v %s", m.Rel, m.Dir, m.Link, m.Pattern))
	}
	want := []string{
		".env dir=false link=false .env",
		".ssh dir=true link=false .ssh/",
		"Upper/.ENV dir=false link=false .env",
		"sub/.env.local dir=false link=false .env.*",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("masks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Not app.txt's neighbours that are masked, not what's inside .ssh, not
	// big.txt (over the limit); edge.txt is exactly at it.
	if s := strings.Join(w.Scan, ","); s != "app.txt,edge.txt,sub/code.go" {
		t.Errorf("scan list %q", s)
	}
	// app.txt, .env, sub and its two files, .ssh itself (not the two inside
	// it, as it isn't entered), Upper and its file, empty, big.txt, edge.txt.
	if w.Entries != 11 {
		t.Errorf("entries %d, want 11", w.Entries)
	}
}

func TestWalkLinks(t *testing.T) {
	root := tree(t, map[string]string{"real.txt": "", "dir/": ""})
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, ".env")); err != nil {
		t.Skip("can't make links here:", err)
	}
	if err := os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, ".aws")); err != nil {
		t.Skip("can't make links here:", err)
	}
	if err := os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, "plain")); err != nil {
		t.Skip("can't make links here:", err)
	}
	w, err := Walk(root, defaults(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range w.Masks {
		got = append(got, fmt.Sprintf("%s dir=%v link=%v", m.Rel, m.Dir, m.Link))
	}
	// A link matches a file pattern (.env) or a folder pattern (.aws/), and
	// is never followed: "plain" isn't entered or scanned.
	if s := strings.Join(got, ","); s != ".aws dir=false link=true,.env dir=false link=true" {
		t.Errorf("masks %q", s)
	}
	if s := strings.Join(w.Scan, ","); s != "real.txt" {
		t.Errorf("scan list %q", s)
	}
}

func TestWalkLimit(t *testing.T) {
	old := MaxEntries
	MaxEntries = 3
	defer func() { MaxEntries = old }()
	root := tree(t, map[string]string{"a": "", "b": "", "c": ""})
	if _, err := Walk(root, defaults(t)); err != nil {
		t.Fatalf("3 entries with a limit of 3: %v", err)
	}
	root = tree(t, map[string]string{"a": "", "b": "", "c": "", "d": ""})
	if _, err := Walk(root, defaults(t)); !errors.Is(err, ErrTooBig) {
		t.Fatalf("4 entries with a limit of 3: %v", err)
	}
}

func TestWalkMissingFolder(t *testing.T) {
	if _, err := Walk(filepath.Join(t.TempDir(), "gone"), defaults(t)); err == nil {
		t.Fatal("a missing folder was walked")
	}
}

func TestCheckMaskCount(t *testing.T) {
	old := MaxMasks
	MaxMasks = 4
	defer func() { MaxMasks = old }()
	a := []Mask{{Pattern: "*.pem"}, {Pattern: "*.pem"}}
	b := []Mask{{Pattern: ".env"}, {Pattern: "*.pem"}}
	if err := CheckMaskCount(a, b); err != nil {
		t.Fatalf("4 masks with a limit of 4: %v", err)
	}
	err := CheckMaskCount(a, b, []Mask{{Pattern: ".env"}})
	if err == nil || !strings.Contains(err.Error(), "need 5 masks, more than 4") || !strings.Contains(err.Error(), `"*.pem" matches 3`) {
		t.Fatalf("5 masks: %v", err)
	}
}

func TestCovered(t *testing.T) {
	ms := []Mask{{Rel: "a"}, {Rel: "b", Dir: true}, {Rel: "c", Link: true}}
	if n := Covered(ms); n != 2 {
		t.Errorf("covered %d, want 2", n)
	}
	if Covered(nil) != 0 {
		t.Error("nil")
	}
}
