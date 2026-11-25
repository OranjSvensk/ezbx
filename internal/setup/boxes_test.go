package setup

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OranjSvensk/ezbx/internal/config"
)

// twoBoxes is a config with two boxes, one with snapshot settings and a
// read-only folder, so a rewrite that drops anything shows.
func twoBoxes(t *testing.T, f Facts) (*config.Config, string, string) {
	t.Helper()
	ws1 := filepath.Join(f.Home, "p", "ws1")
	ws2 := filepath.Join(f.Home, "ws2")
	ro := filepath.Join(f.Home, "docs")
	c, err := config.Parse("version = 1\n" +
		"[box.main]\nworkspace = '" + ws1 + "'\nmemory = '4g'\nnetwork = 'off'\nharnesses = ['pi']\ntools = ['pack:coding']\n" +
		"[box.main.snapshots]\nskip = ['dist']\nmax_file = '10m'\nkeep = 5\n" +
		"[box.agent-ada]\nworkspace = '" + ws2 + "'\nmemory = '2g'\ncpus = 1.5\npids = 256\nnetwork = 'allow'\nallow = ['pypi.org']\nharnesses = ['pi']\n" +
		"[[box.agent-ada.readonly]]\npath = '" + ro + "'\nname = 'docs'\nmask = ['private/']\n" +
		"[engine]\napi = 'openai'\nurl = 'http://127.0.0.1:18080'\n[models.fake]\nreasoning = true\n")
	if err != nil {
		t.Fatal(err)
	}
	return c, ws1, ws2
}

// Writing the answers for one box keeps every other box exactly as it was,
// with its snapshot settings and read-only folders.
func TestConfigTextKeepsOtherBoxes(t *testing.T) {
	f := facts(t)
	c, _, ws2 := twoBoxes(t, f)
	a, found := FromConfigBox(c, "main")
	if !found || a.Snapshots.Keep != 5 || a.Snapshots.MaxFile != "10m" || strings.Join(a.Snapshots.Skip, ",") != "dist" {
		t.Fatalf("main: %v %+v", found, a.Snapshots)
	}
	if len(a.Others) != 1 {
		t.Fatalf("others: %+v", a.Others)
	}
	a.Network = "on"
	text := ConfigText(a, time.Now())
	back, err := config.Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if got := strings.Join(back.Names(), ","); got != "agent-ada,main" {
		t.Fatalf("boxes: %s\n%s", got, text)
	}
	m, _ := back.Get("main")
	if m.Network != "on" || m.Snapshots.Keep != 5 || m.Snapshots.MaxFile != "10m" || strings.Join(m.Snapshots.Skip, ",") != "dist" {
		t.Errorf("main after the rewrite: %+v", m)
	}
	o, _ := back.Get("agent-ada")
	if o.Workspace != ws2 || o.Memory != "2g" || o.CPUs != 1.5 || o.PIDs != 256 || o.Network != "allow" || strings.Join(o.Allow, ",") != "pypi.org" ||
		len(o.ReadOnly) != 1 || o.ReadOnly[0].Name != "docs" || o.ReadOnly[0].Mask[0] != "private/" || o.Tools != nil {
		t.Errorf("agent-ada after the rewrite: %+v", o)
	}
	// The answers win over an old copy of the same box in Others.
	stale := a
	stale.Others = map[string]config.Box{"main": {Workspace: "/old", Network: "allow", Harnesses: []string{"pi"}}}
	if b, err := config.Parse(ConfigText(stale, time.Now())); err != nil {
		t.Fatal(err)
	} else if m, _ := b.Get("main"); m.Network != "on" || m.Workspace == "/old" {
		t.Errorf("an old copy of the box won: %+v", m)
	}
	// Answers don't share lists with the config they came from.
	a.Others["agent-ada"].Allow[0] = "changed.example.com"
	if b, _ := c.Get("agent-ada"); b.Allow[0] != "pypi.org" {
		t.Error("FromConfigBox shares the other boxes' lists")
	}
}

// An empty skip list (leave nothing out) is not the same as no list (the
// defaults), and survives a rewrite.
func TestConfigTextEmptySkip(t *testing.T) {
	f := facts(t)
	c, err := config.Parse("version = 1\n[box.main]\nworkspace = '" + filepath.Join(f.Home, "w") + "'\nharnesses = ['pi']\n[box.main.snapshots]\nskip = []\n")
	if err != nil {
		t.Fatal(err)
	}
	back, err := config.Parse(ConfigText(FromConfig(c), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := back.Get("main"); b.Snapshots.Skip == nil || len(b.Snapshots.Skip) != 0 {
		t.Errorf("skip: %#v", b.Snapshots.Skip)
	}
}

func TestGatherSeveralBoxes(t *testing.T) {
	f := realFacts(t)
	c, ws1, _ := twoBoxes(t, f)
	f.Existing = c
	p, _ := prompter("")
	p.Auto = true

	// Which box isn't guessed.
	if _, err := Gather(f, Options{}, p); err == nil || !strings.Contains(err.Error(), "--box NAME") {
		t.Fatalf("no --box with two boxes: %v", err)
	}
	if _, err := Gather(f, Options{Box: "Bad_Name"}, p); err == nil || !strings.Contains(err.Error(), "box name") {
		t.Fatalf("bad box name: %v", err)
	}

	// A named box that exists starts from its own settings.
	res, err := Gather(f, Options{Box: "main"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers; a.Workspace != ws1 || a.Memory != "4g" || len(a.Others) != 1 {
		t.Errorf("main: %+v", a)
	}
	if ch := Changes(mustBox(t, c, "main"), res.Answers); len(ch) != 0 {
		t.Errorf("taking every default must change nothing: %v", ch)
	}

	// A new box shares the engine, gets the starter profile question, and
	// keeps both boxes that were there.
	out := &strings.Builder{}
	p.Out = out
	ws3 := filepath.Join(f.Home, "ws3")
	res, err = Gather(f, Options{Box: "agent-bea", Workspace: ws3}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if a.Box != "agent-bea" || a.Workspace != ws3 || a.Engine.URL != "http://127.0.0.1:18080" || len(a.Others) != 2 || a.Network != "off" {
		t.Errorf("new box: %+v", a)
	}
	if !strings.Contains(out.String(), "Starter profiles") {
		t.Errorf("a new box gets the starter profile question:\n%s", out)
	}
	if strings.Join(a.Tools, ",") != "pack:coding" {
		t.Errorf("a new box starts from the offline profile's tools: %q", a.Tools)
	}
	old := mustBox(t, c, "agent-bea")
	if got := strings.Join(Changes(old, a), "\n"); !strings.Contains(got, "new box    agent-bea") {
		t.Errorf("the review names the new box:\n%s", got)
	}
	back, err := config.Parse(ConfigText(a, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(back.Names(), ","); got != "agent-ada,agent-bea,main" {
		t.Errorf("boxes after adding one: %s", got)
	}

	// Another box's workspace, or a folder inside it, is refused.
	for _, ws := range []string{ws1, filepath.Join(ws1, "sub"), filepath.Dir(ws1)} {
		if _, err := Gather(f, Options{Box: "agent-bea", Workspace: ws}, p); err == nil {
			t.Errorf("workspace %s overlaps box main's and was taken", ws)
		}
	}
}

func mustBox(t *testing.T, c *config.Config, name string) Answers {
	t.Helper()
	a, _ := FromConfigBox(c, name)
	return a
}

// airgap can be typed at the network question, and a mistake names all four.
func TestGatherAirgap(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(f.Home, "w")
	input := strings.Join([]string{"", "", "", "", ws, "", "closed", "airgap", ""}, "\n") + "\n"
	p, out := prompter(input)
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Answers.Network != "airgap" || len(res.Answers.Allow) != 0 {
		t.Errorf("network: %+v", res.Answers)
	}
	for _, want := range []string{"Type airgap, off, allow or on.", "airgap  nothing at all, not even the model"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := config.Parse(ConfigText(res.Answers, time.Now())); err != nil {
		t.Error(err)
	}
}
