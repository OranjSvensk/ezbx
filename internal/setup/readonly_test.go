package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/readonly"
)

func makeFiles(t *testing.T, root string, files map[string]string) {
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

var fakeAWS = "AK" + "IA" + strings.Repeat("Q", 16)

func TestReadOnlyRoundTrip(t *testing.T) {
	a := Answers{Box: "main", Workspace: "/w", Memory: "4g", Network: "off", Harnesses: []string{"pi"}, Engine: "auto",
		ReadOnly: []config.ReadOnly{
			{Path: "/p/one"},
			{Path: "/p/two", Name: "docs", Mask: []string{"./k.txt", "private/"}, Unmask: []string{".npmrc"}},
		}}
	text := ConfigText(a, time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
	c, err := config.Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	back := FromConfig(c)
	if readOnlySetting(back.ReadOnly) != readOnlySetting(a.ReadOnly) {
		t.Errorf("round trip:\n%s\n%s\n%s", readOnlySetting(back.ReadOnly), readOnlySetting(a.ReadOnly), text)
	}
	// The answers are a copy: changing them leaves the config alone.
	back.ReadOnly[1].Mask[0] = "changed"
	if _, b := c.Only(); b.ReadOnly[1].Mask[0] != "./k.txt" {
		t.Error("FromConfig shares the config's lists")
	}
	if ch := Changes(a, Answers{Box: "main", Workspace: "/w", Memory: "4g", Network: "off", Harnesses: []string{"pi"}, Engine: "auto"}); len(ch) != 1 || !strings.HasPrefix(ch[0], "readonly") {
		t.Errorf("changes %v", ch)
	}
}

// With --readonly and --yes: the folders given, an existing entry's settings
// kept, and a secret-looking file masked by the default yes.
func TestGatherReadOnlyFlags(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(f.Home, "ws")
	one, two := filepath.Join(f.Home, "one"), filepath.Join(f.Home, "two")
	makeFiles(t, one, map[string]string{"a.txt": "x", ".env": "x", "notes/key.txt": "id = " + fakeAWS})
	makeFiles(t, two, map[string]string{"b.txt": "x"})
	c, err := config.Parse("version = 1\n[box.main]\nworkspace = '" + filepath.ToSlash(ws) + "'\nharnesses = ['pi']\n" +
		"[[box.main.readonly]]\npath = '" + filepath.ToSlash(two) + "'\nname = 'second'\nunmask = ['.npmrc']\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = c
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{ReadOnly: []string{"~/one", filepath.ToSlash(two)}}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ro := res.Answers.ReadOnly
	if len(ro) != 2 || ro[0].Path != one || strings.Join(ro[0].Mask, ",") != "./notes/key.txt" || ro[1].Name != "second" || ro[1].Unmask[0] != ".npmrc" {
		t.Fatalf("%+v\n%s", ro, out)
	}
	if !strings.Contains(out.String(), "looks like it holds an AWS access key. Hide it from the agent?") || strings.Contains(out.String(), fakeAWS) {
		t.Errorf("question:\n%s", out)
	}
	sum := res.ReadOnly
	if len(sum) != 2 || sum[0].Inside != "/readonly/one" || sum[0].Masked != 2 || sum[1].Inside != "/readonly/second" || sum[1].Masked != 0 {
		t.Errorf("summary %+v", sum)
	}
	// Asking again keeps the mask instead of asking twice.
	f.Existing = nil
	p, out = prompter("")
	p.Auto = true
	o := Options{ReadOnly: []string{one}}
	res2, err := gatherWith(f, o, p, ro[:1])
	if err != nil || strings.Contains(out.String(), "Hide it") || strings.Join(res2.ReadOnly[0].Mask, ",") != "./notes/key.txt" {
		t.Errorf("again: %v %+v\n%s", err, res2.ReadOnly, out)
	}
}

// gatherWith runs only the read-only step, starting from had.
func gatherWith(f Facts, o Options, p *Prompter, had []config.ReadOnly) (Answers, error) {
	a := Answers{Workspace: filepath.Join(f.Home, "ws"), ReadOnly: cloneReadOnly(had)}
	var res Result
	err := askReadOnly(f, o, &a, p, &res)
	return a, err
}

func TestGatherReadOnlyTyped(t *testing.T) {
	f := facts(t)
	keep, drop, add := filepath.Join(f.Home, "keep"), filepath.Join(f.Home, "drop"), filepath.Join(f.Home, "add")
	for _, d := range []string{keep, drop, add} {
		makeFiles(t, d, map[string]string{"a.txt": "x"})
	}
	makeFiles(t, add, map[string]string{"k.pem.txt": "-----BEGIN " + "PRIVATE" + " KEY-----"})
	had := []config.ReadOnly{{Path: keep}, {Path: drop}}
	// keep: yes; drop: no; then a relative path, the workspace, a folder
	// that doesn't exist, the good one, and Enter; then "no" to masking.
	input := strings.Join([]string{"y", "n", "rel", filepath.Join(f.Home, "ws"), filepath.Join(f.Home, "gone"), add, "", "n"}, "\n") + "\n"
	p, out := prompter(input)
	a, err := gatherWith(f, Options{}, p, had)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(a.ReadOnly) != 2 || a.ReadOnly[0].Path != keep || a.ReadOnly[1].Path != add || len(a.ReadOnly[1].Mask) != 0 {
		t.Fatalf("%+v\n%s", a.ReadOnly, out)
	}
	for _, want := range []string{"must be a full path", "overlaps the workspace", "doesn't exist", "looks like it holds a private key"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// --yes with no folders asks nothing more and keeps none.
	p, _ = prompter("")
	p.Auto = true
	if a, err := gatherWith(f, Options{}, p, nil); err != nil || a.ReadOnly != nil {
		t.Errorf("none: %+v %v", a.ReadOnly, err)
	}
}

func TestReadOnlyNetWarning(t *testing.T) {
	cases := map[string]string{"off": "", "": "", "allow": "an allowed host", "on": "the public internet"}
	for mode, want := range cases {
		got := ReadOnlyNetWarning(mode, 1)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: %q", mode, got)
		}
	}
	if ReadOnlyNetWarning("on", 0) != "" {
		t.Error("a warning without read-only folders")
	}
}

func TestReviewReadOnly(t *testing.T) {
	p := plan(t)
	if !strings.Contains(p.Review(), "Read-only     none\n") {
		t.Error(p.Review())
	}
	p.Result.ReadOnly = []ReadOnlySummary{{Path: "/p/one", Inside: "/readonly/one", Masked: 3}, {Path: "/p/two", Inside: "/readonly/docs", Unmask: []string{".npmrc"}}}
	p.Result.Answers.Network = "allow"
	text := p.Review()
	for _, want := range []string{
		"Read-only     /p/one at /readonly/one, 3 hidden\n",
		"              /p/two at /readonly/docs, 0 hidden, shown after all: .npmrc\n",
		"warning: with the network allow, the agent could send what it reads",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestGatherReadOnlyMaskLimit(t *testing.T) {
	old := readonly.MaxMasks
	readonly.MaxMasks = 1
	defer func() { readonly.MaxMasks = old }()
	f := facts(t)
	one := filepath.Join(f.Home, "one")
	makeFiles(t, one, map[string]string{".env": "x", "b/.env": "x"})
	p, _ := prompter("")
	p.Auto = true
	if _, err := gatherWith(f, Options{ReadOnly: []string{one}}, p, nil); err == nil || !strings.Contains(err.Error(), "need 2 masks") {
		t.Fatalf("got %v", err)
	}
}
