package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/readonly"
)

func TestMountSpec(t *testing.T) {
	cases := map[string][]string{
		"type=bind,src=/a,dst=/b,readonly":   {"type=bind", "src=/a", "dst=/b", "readonly"},
		`type=bind,"src=/a,b",dst=/c`:        {"type=bind", "src=/a,b", "dst=/c"},
		`type=bind,"src=/say ""hi""",dst=/c`: {"type=bind", `src=/say "hi"`, "dst=/c"},
	}
	for want, fields := range cases {
		if got := mountSpec(fields...); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestBoxArgsReadOnly(t *testing.T) {
	p := testPlan(t)
	p.ReadOnly = []ReadOnlyMount{{
		Name: "projects", Path: filepath.Join("host", "Projects"),
		Masks: []readonly.Mask{
			{Rel: ".env", Pattern: ".env"},
			{Rel: ".ssh", Dir: true, Pattern: ".ssh/"},
			{Rel: "link/.aws", Link: true, Pattern: ".aws/"},
			{Rel: "a b/c,d.pem", Pattern: "*.pem"},
		},
	}}
	a := strings.Join(p.BoxArgs(), "\n")
	want := strings.Join([]string{
		"--mount", "type=bind,src=" + filepath.Join("host", "Projects") + ",dst=/readonly/projects,readonly",
		"--mount", "type=bind,src=/dev/null,dst=/readonly/projects/.env,readonly",
		"--mount", "type=tmpfs,dst=/readonly/projects/.ssh,tmpfs-size=4k,tmpfs-mode=0555,readonly",
		"--mount", `type=bind,src=/dev/null,"dst=/readonly/projects/a b/c,d.pem",readonly`,
		p.BoxImage.Tag, "sleep", "infinity",
	}, "\n")
	if !strings.HasSuffix(a, want) {
		t.Errorf("box args end:\n%s\nwant:\n%s", a[strings.LastIndex(a, "-v\n"):], want)
	}
	if strings.Contains(a, ".aws") {
		t.Error("a link was mounted over")
	}
	p.ReadOnly = nil
	if strings.Contains(strings.Join(p.BoxArgs(), " "), "--mount") {
		t.Error("mounts without read-only folders")
	}
}

func TestMasksRecord(t *testing.T) {
	state := t.TempDir()
	if ms, ok, err := ReadMasks(state); ms != nil || ok || err != nil {
		t.Fatalf("no record: %v %v %v", ms, ok, err)
	}
	in := []ReadOnlyMount{{Name: "p", Path: filepath.Join(state, "p"), Mask: []string{"x/"}, Unmask: []string{".npmrc"},
		Masks: []readonly.Mask{{Rel: ".env", Pattern: ".env"}, {Rel: "x", Dir: true, Pattern: "x/"}}}}
	if err := WriteMasks(state, in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := ReadMasks(state)
	if err != nil || !ok || len(out) != 1 || out[0].Path != in[0].Path || out[0].Unmask[0] != ".npmrc" || len(out[0].Masks) != 2 || !out[0].Masks[1].Dir {
		t.Fatalf("read back %+v %v %v", out, ok, err)
	}
	if st, err := os.Stat(MasksFileIn(state)); err != nil || (st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("record mode: %v %v", st, err)
	}
	if err := os.WriteFile(MasksFileIn(state), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMasks(state); err == nil || !strings.Contains(err.Error(), "is broken") {
		t.Errorf("broken record: %v", err)
	}
	if err := RemoveMasks(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(MasksFileIn(state)); !os.IsNotExist(err) {
		t.Error("record still there")
	}
	if err := RemoveMasks(state); err != nil {
		t.Errorf("removing a missing record: %v", err)
	}
}

func TestUnmasked(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a.txt", ".env", ".ssh/key"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	m := ReadOnlyMount{Name: "r", Path: root, Mask: []string{"*.txt"}, Unmask: []string{".env"},
		Masks: []readonly.Mask{{Rel: "a.txt", Pattern: "*.txt"}, {Rel: ".ssh", Dir: true, Pattern: ".ssh/"}}}
	got, _, err := Unmasked(m)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing new: %v %v", got, err)
	}
	// The record's own mask (*.txt) and unmask (.env) are used, not the
	// defaults alone; a name differing only in case is the same file.
	os.WriteFile(filepath.Join(root, "B.TXT"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "id_rsa"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, ".ssh", "new"), []byte("x"), 0o644)
	got, _, err = Unmasked(m)
	if err != nil || strings.Join(got, ",") != "B.TXT,id_rsa" {
		t.Fatalf("got %v %v", got, err)
	}
	m.Masks[0].Rel = "A.TXT"
	if got, _, _ := Unmasked(m); strings.Join(got, ",") != "B.TXT,id_rsa" {
		t.Errorf("case: %v", got)
	}
}

// up records the read-only folders before the box starts, up without them
// removes an old record, and down removes it.
func TestUpAndDownKeepTheRecord(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	p.ReadOnly = []ReadOnlyMount{{Name: "docs", Path: t.TempDir(), Masks: []readonly.Mask{{Rel: ".env", Pattern: ".env"}}}}
	if err := runner(f).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ms, ok, err := ReadMasks(p.StateDir)
	if err != nil || !ok || len(ms) != 1 || ms[0].Name != "docs" || ms[0].Masks[0].Rel != ".env" {
		t.Fatalf("after up: %+v %v %v", ms, ok, err)
	}
	if err := runner(f).Down(context.Background(), "t", p.StateDir); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadMasks(p.StateDir); ok {
		t.Error("down left the record")
	}
	if err := WriteMasks(p.StateDir, p.ReadOnly); err != nil {
		t.Fatal(err)
	}
	p.ReadOnly = nil
	if err := runner(newFake()).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadMasks(p.StateDir); ok {
		t.Error("up without read-only folders kept an old record")
	}
}
