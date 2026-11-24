package setup

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/tools"
)

const gib = int64(1 << 30)

func TestMemoryFor(t *testing.T) {
	cases := map[int64]string{0: "2g", 3 * gib: "2g", 4 * gib: "2g", 6 * gib: "3g", 10*gib + 5: "5g", 31*gib + gib/3: "8g", 100 * gib: "8g"}
	for in, want := range cases {
		if got := MemoryFor(in); got != want {
			t.Errorf("%d bytes: got %s, want %s", in, got, want)
		}
	}
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestConfigTextRoundTrip(t *testing.T) {
	a := Answers{
		Box: "main", Workspace: `/home/example\my "work"` + "\tspace", Memory: "8g", CPUs: 2.5, PIDs: 1024,
		Network: "allow", Allow: []string{"registry.npmjs.org", "*.example.com"}, Harnesses: []string{"pi"},
		Tools:  []string{"pack:coding", "ffmpeg"},
		Engine: config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000", KeyEnv: "ENGINE_KEY", Host: "localhost:53000"},
		Models: map[string]config.Model{"big-model": {Reasoning: yes(), MaxTokens: 8192, ContextWindow: 32768, Name: "Big"}, "small": {Reasoning: no()}, "empty": {}},
		Log:    no(),
	}
	text := ConfigText(a, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	c, err := config.Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	back := FromConfig(c)
	if back.Workspace != a.Workspace || back.Memory != "8g" || back.CPUs != 2.5 || back.PIDs != 1024 || back.Network != "allow" ||
		strings.Join(back.Allow, ",") != "registry.npmjs.org,*.example.com" || back.Engine != a.Engine || back.Log == nil || *back.Log {
		t.Errorf("box or engine: %+v", back)
	}
	if strings.Join(back.Tools, ",") != "pack:coding,ffmpeg" {
		t.Errorf("tools: %q", back.Tools)
	}
	if !strings.Contains(text, `api     = "lmstudio"`) || !strings.Contains(text, `url     = "http://127.0.0.1:53000"`) {
		t.Errorf("engine section:\n%s", text)
	}
	// With no server chosen yet, url is a comment and api reads back as auto.
	none := a
	none.Engine = config.Engine{}
	noneText := ConfigText(none, time.Now())
	if c, err := config.Parse(noneText); err != nil || c.Engine != (config.Engine{API: "auto"}) || !strings.Contains(noneText, "# url") {
		t.Errorf("no server: %v\n%s", err, noneText)
	}
	m := back.Models["big-model"]
	if m.Reasoning == nil || !*m.Reasoning || m.MaxTokens != 8192 || m.ContextWindow != 32768 || m.Name != "Big" {
		t.Errorf("model: %+v", m)
	}
	if s := back.Models["small"]; s.Reasoning == nil || *s.Reasoning {
		t.Error("an explicit reasoning = false must be kept")
	}
	if _, ok := back.Models["empty"]; ok {
		t.Error("an empty model should be left out")
	}
	if !strings.Contains(text, "written by ezbx init on 2026-01-02") || strings.Contains(text, "pids      = 512") {
		t.Error(text)
	}
	// Defaults stay out of the file.
	a.PIDs, a.CPUs, a.Log = 0, 0, nil
	text = ConfigText(a, time.Now())
	if strings.Contains(text, "pids") || strings.Contains(text, "cpus") || !strings.Contains(text, "log = true") {
		t.Error(text)
	}
	// No tools setting stays out; an empty one is written, and read back empty.
	a.Tools = nil
	if text = ConfigText(a, time.Now()); strings.Contains(text, "tools") {
		t.Error(text)
	}
	a.Tools = []string{}
	text = ConfigText(a, time.Now())
	if c, err := config.Parse(text); err != nil || !strings.Contains(text, "tools     = []") {
		t.Errorf("%v\n%s", err, text)
	} else if b := FromConfig(c); b.Tools == nil || len(b.Tools) != 0 {
		t.Errorf("an empty tools list must read back empty, not unset: %#v", b.Tools)
	}
}

func TestQuote(t *testing.T) {
	got := quote("a\"b\x01\x7fé\\")
	if got != `"a\"b\u0001\u007Fé\\"` {
		t.Error(got)
	}
}

func facts(t *testing.T) Facts {
	home := filepath.Join(t.TempDir(), "home")
	pi := harness.Manifest{ID: "pi", Name: "Pi", Install: harness.Install{Version: "1.5.0"}, Tools: []string{"fd"}, Verified: harness.Verified{Status: "verified"}}
	other := harness.Manifest{ID: "other", Name: "Other", Network: harness.Network{Domains: []string{"api.example.com"}}, Verified: harness.Verified{Status: "verified"}}
	return Facts{
		Engines: []detect.Engine{
			{ID: "lmstudio", Name: "LM Studio API", URL: "http://127.0.0.1:53000", Models: []detect.Model{{ID: "big-model"}}, AlsoMatched: []string{"openai"}},
			{ID: "openai", Name: "OpenAI-compatible API", URL: "http://127.0.0.1:18080", Models: []detect.Model{{ID: "fake"}}},
		},
		Harnesses: []harness.Manifest{other, pi}, DockerMemory: 31 * gib, Home: home,
		Own:     []string{filepath.Join(home, "appdata", "ezbx")},
		Catalog: testCatalog(t),
	}
}

// testCatalog has two verified tools, one unverified, and one pack.
func testCatalog(t *testing.T) *tools.Catalog {
	tool := func(id, status string) *fstest.MapFile {
		v := `{"status": "unverified", "observedOn": "", "notes": ""}`
		if status == "verified" {
			v = `{"status": "verified", "observedOn": "2026-01-02", "notes": "checked"}`
		}
		return &fstest.MapFile{Data: []byte(`{"schema": 1, "id": "` + id + `", "name": "` + id + `", "description": "d", "debian": ["` + id + `"], "commands": ["` + id + `"], "verified": ` + v + `}`)}
	}
	c, err := tools.Load(fstest.MapFS{
		"tools/fd.json": tool("fd", "verified"), "tools/jq.json": tool("jq", "verified"), "tools/wip.json": tool("wip", "unverified"),
		"packs/coding.json": {Data: []byte(`{"schema": 1, "id": "coding", "name": "Coding", "description": "d", "tools": ["fd", "jq"]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func prompter(input string) (*Prompter, *strings.Builder) {
	var out strings.Builder
	return &Prompter{In: bufio.NewReader(strings.NewReader(input)), Out: &out}, &out
}

func TestGatherDefaults(t *testing.T) {
	f := facts(t)
	p, _ := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if res.Engine == nil || res.Engine.ID != "lmstudio" || a.Engine != (config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000"}) || strings.Join(a.Harnesses, ",") != "pi" ||
		a.Workspace != filepath.Join(f.Home, "ezbx", "workspace") || a.Network != "off" || a.Memory != "8g" || a.Box != "main" {
		t.Errorf("%+v", a)
	}
	if _, ok := a.Models["big-model"]; ok {
		t.Error("reasoning defaults to no, so nothing is written")
	}
	if strings.Join(a.Tools, ",") != "pack:coding" {
		t.Errorf("a new config starts with the coding pack: %q", a.Tools)
	}
}

// An existing config keeps its tools, and one without a tools setting keeps
// none, unless a flag or an answer says otherwise.
func TestGatherToolsOfExistingConfig(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	base := "version = 1\n[box.main]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n"
	for name, tc := range map[string]struct {
		setting string
		o       Options
		typed   string
		want    string // "<nil>" for not set
	}{
		"unset, default":   {"", Options{}, "", "<nil>"},
		"unset, typed":     {"", Options{}, "jq", "jq"},
		"set, default":     {"tools = ['jq']\n", Options{}, "", "jq"},
		"set, typed none":  {"tools = ['jq']\n", Options{}, "none", ""},
		"unset, flag none": {"", Options{Tools: []string{}}, "", ""},
		"set, flag":        {"tools = ['jq']\n", Options{Tools: []string{"pack:coding"}}, "", "pack:coding"},
	} {
		t.Run(name, func(t *testing.T) {
			f := facts(t)
			c, err := config.Parse(base + tc.setting)
			if err != nil {
				t.Fatal(err)
			}
			f.Existing = c
			// Answers in order: engine, reasoning, harness, tools, then the rest default.
			p, out := prompter("\n\n\n" + tc.typed + "\n\n\n\n\n")
			res, err := Gather(f, tc.o, p)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := strings.Join(res.Answers.Tools, ",")
			if res.Answers.Tools == nil {
				got = "<nil>"
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGatherTyped(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	// engine 2; reasoning: bad answer then y; harnesses 1,2; a bad workspace
	// (the home folder) then a good one; allow with hosts; memory.
	input := strings.Join([]string{"2", "maybe", "y", "1,2,1", "pack:nope", "wip", "pack:coding, jq, jq", f.Home, ws, "", "allow", "pypi.org, GitHub.com", "6G"}, "\n") + "\n"
	p, out := prompter(input)
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	a := res.Answers
	if res.Engine.ID != "openai" || a.Engine != (config.Engine{API: "openai", URL: "http://127.0.0.1:18080"}) || strings.Join(a.Harnesses, ",") != "other,pi" || a.Workspace != ws ||
		a.Network != "allow" || strings.Join(a.Allow, ",") != "pypi.org,github.com" || a.Memory != "6g" || strings.Join(a.Tools, ",") != "pack:coding,jq" {
		t.Errorf("%+v", a)
	}
	if r := a.Models["fake"].Reasoning; r == nil || !*r {
		t.Error("reasoning should be set")
	}
	for _, want := range []string{"Answer y or n.", "home folder", "api.example.com", `unknown tool pack "nope"`, `"wip" isn't verified`, "Single tools: jq, wip\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	// fd is in every box through Pi, so it isn't offered as a single tool.
	if strings.Contains(out.String(), "Single tools: fd") {
		t.Error(out.String())
	}
}

func TestGatherFlags(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	p, _ := prompter("")
	p.Auto = true
	// --url picks the server found there (a trailing slash aside).
	o := Options{URL: "http://127.0.0.1:18080/", KeyEnv: "K", Reasoning: true, Harnesses: []string{"pi"}, Workspace: ws, Network: "on", Memory: "3g"}
	res, err := Gather(f, o, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if res.Engine == nil || res.Engine.ID != "openai" || a.Engine != (config.Engine{API: "openai", URL: "http://127.0.0.1:18080", KeyEnv: "K"}) ||
		a.Network != "on" || a.Memory != "3g" || a.Workspace != ws {
		t.Errorf("%+v", a)
	}
	if r := a.Models["fake"].Reasoning; r == nil || !*r {
		t.Error("--reasoning")
	}
	if res, _ = Gather(f, Options{API: "auto"}, p); res.Engine == nil || res.Answers.Engine != (config.Engine{API: "auto", URL: "http://127.0.0.1:53000"}) {
		t.Errorf("--api auto: %+v", res.Answers.Engine)
	}
	// A server with the LM Studio API also speaks OpenAI's, and --api openai
	// uses it as a plain one.
	if res, _ = Gather(f, Options{API: "openai"}, p); res.Engine == nil || res.Engine.ID != "lmstudio" || res.Answers.Engine.API != "openai" {
		t.Errorf("--api openai: %+v", res.Answers.Engine)
	}
	// --api lmstudio offers only servers that speak it.
	if res, _ = Gather(f, Options{API: "lmstudio", URL: "http://127.0.0.1:18080"}, p); res.Engine != nil || res.Answers.Engine != (config.Engine{API: "lmstudio", URL: "http://127.0.0.1:18080"}) {
		t.Errorf("--api lmstudio at a plain server: %+v %+v", res.Engine, res.Answers.Engine)
	}
	if res, _ = Gather(f, Options{Later: true}, p); res.Engine != nil || res.Answers.Engine != (config.Engine{API: "auto"}) {
		t.Errorf("--later: %+v", res.Answers.Engine)
	}
}

func TestGatherRefusals(t *testing.T) {
	f := facts(t)
	auto := func() *Prompter { p, _ := prompter(""); p.Auto = true; return p }
	cases := map[string]Options{
		"unknown harness":    {Harnesses: []string{"nope"}},
		"home as workspace":  {Workspace: f.Home},
		"relative workspace": {Workspace: "work"},
		"own folder":         {Workspace: f.Own[0]},
		"bad network":        {Network: "open"},
		"bad memory":         {Memory: "lots"},
	}
	for name, o := range cases {
		if _, err := Gather(f, o, auto()); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	p, _ := prompter("1\n")
	if _, err := Gather(f, Options{}, p); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("input ending: %v", err)
	}
}

func TestGatherKeepsExisting(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	c, err := config.Parse("version = 1\n[box.work]\nworkspace = '" + ws + "'\nmemory = '5g'\ncpus = 3\nnetwork = 'allow'\nallow = ['pypi.org']\nharnesses = ['pi']\n" +
		"[engine]\napi = 'openai'\nurl = 'http://127.0.0.1:18080'\nhost = 'localhost:18080'\n[models.fake]\nreasoning = true\nmax_tokens = 4096\n[gateway]\nlog = false\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = c
	p, _ := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if a.Box != "work" || a.Engine.API != "openai" || a.Workspace != ws || a.Memory != "5g" || a.CPUs != 3 || a.Network != "allow" ||
		strings.Join(a.Allow, ",") != "pypi.org" || a.Engine.Host != "localhost:18080" || a.Log == nil || *a.Log {
		t.Errorf("%+v", a)
	}
	if m := a.Models["fake"]; m.Reasoning == nil || !*m.Reasoning || m.MaxTokens != 4096 {
		t.Errorf("model: %+v", m)
	}
	if ch := Changes(FromConfig(c), a); len(ch) != 0 {
		t.Errorf("taking every default must change nothing: %v", ch)
	}
	a.Network = "on"
	a.Models["fake"] = config.Model{Reasoning: no(), MaxTokens: 4096}
	ch := Changes(FromConfig(c), a)
	if len(ch) != 2 || !strings.Contains(ch[0], "network") || !strings.Contains(ch[1], "reasoning true") {
		t.Errorf("%v", ch)
	}
	// Each engine setting that changes is named on the review screen.
	b := FromConfig(c)
	b.Engine = config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000", KeyEnv: "K"}
	got := strings.Join(Changes(FromConfig(c), b), "\n")
	for _, want := range []string{"engine     http://127.0.0.1:18080 -> http://127.0.0.1:53000", "api        openai -> lmstudio",
		"key_env    (not set) -> K", "host       localhost:18080 -> (not set)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestGatherNoEngine(t *testing.T) {
	f := facts(t)
	f.Engines = nil
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil || res.Engine != nil || res.Answers.Engine != (config.Engine{API: "auto"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(out.String(), "none.") {
		t.Error(out.String())
	}
	if res, _ = Gather(f, Options{URL: "http://127.0.0.1:18090", API: "openai"}, p); res.Answers.Engine != (config.Engine{API: "openai", URL: "http://127.0.0.1:18090"}) {
		t.Errorf("a server given is written even if it isn't answering yet: %+v", res.Answers.Engine)
	}
}

func plan(t *testing.T) Plan {
	f := facts(t)
	e := f.Engines[0]
	return Plan{
		Result:     Result{Engine: &e, Answers: Answers{Box: "main", Engine: config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000"}, Workspace: filepath.Join(f.Home, "ws"), Network: "off", Memory: "8g", Harnesses: []string{"pi"}}},
		ConfigFile: "ezbx.toml", StateDir: "state",
		Images:    []Image{{Tag: "ezbx-gateway:aaa", Present: true}, {Tag: "ezbx-box:bbb", Present: true}},
		Tools:     []string{"fd"},
		Harnesses: f.Harnesses,
	}
}

func TestPlanStart(t *testing.T) {
	cases := map[string]struct {
		change func(*Plan)
		want   string
	}{
		"ready":       {func(*Plan) {}, ""},
		"no-start":    {func(p *Plan) { p.NoStart = true }, "--no-start"},
		"no engine":   {func(p *Plan) { p.Result.Engine = nil }, "no model server"},
		"no models":   {func(p *Plan) { e := *p.Result.Engine; e.Models = nil; p.Result.Engine = &e }, "no models"},
		"image":       {func(p *Plan) { p.Images[1].Present = false }, ""}, // apply builds it first
		"up, changed": {func(p *Plan) { p.BoxUp = true }, "ezbx down, then ezbx up"},
		"up, same": {func(p *Plan) {
			p.BoxUp = true
			old := p.Result.Answers
			p.Old = &old
		}, "already up"},
		"key empty": {func(p *Plan) {
			p.Result.Answers.Engine.KeyEnv = "K"
		}, "K is empty"},
	}
	for name, tc := range cases {
		p := plan(t)
		tc.change(&p)
		ok, why := p.Start()
		if ok != (tc.want == "") || !strings.Contains(why, tc.want) {
			t.Errorf("%s: %v %q", name, ok, why)
		}
	}
}

func TestReview(t *testing.T) {
	p := plan(t)
	p.Images[0].Present = false
	p.Result.Answers.Tools = []string{"pack:coding"}
	p.Tools = []string{"fd", "jq"}
	text := p.Review()
	for _, want := range []string{"nothing has been changed yet", "LM Studio API at http://127.0.0.1:53000", "Pi 1.5.0 (verified)", "(will be created)", "(new)",
		"Tools         fd, jq   (from pack:coding)", "images        ezbx build: ezbx-gateway:aaa (downloads", "then          ezbx up"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	old := p.Result.Answers
	old.Network = "on"
	old.Tools = nil
	p.Old = &old
	p.Images[0].Present = true
	text = p.Review()
	if !strings.Contains(text, "kept as ezbx.toml.previous") || !strings.Contains(text, "network    on -> off") || !strings.Contains(text, "ezbx up (2 networks") ||
		!strings.Contains(text, "tools      (not set) -> pack:coding") || !strings.Contains(text, "ezbx-gateway:aaa and ezbx-box:bbb (built already)") {
		t.Error(text)
	}
	// Not starting: the missing image is named, with how to build it.
	p.NoStart = true
	p.Images[1].Present = false
	if text = p.Review(); !strings.Contains(text, "not built yet: ezbx-box:bbb. ezbx build makes them") {
		t.Error(text)
	}
}

func TestPrompterAutoShowsAnswers(t *testing.T) {
	p, out := prompter("")
	p.Auto = true
	if v, err := p.YesNo("Ok?", false); err != nil || v {
		t.Fatal(v, err)
	}
	if !strings.Contains(out.String(), "Ok? (y/n) [n] n") {
		t.Error(out.String())
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// A --url where nothing answers yet is written anyway. Moving to a new
// address drops key_env and host: a key is only ever sent to its own url.
func TestGatherURLNotAnswering(t *testing.T) {
	f := facts(t)
	c, err := config.Parse("version = 1\n[box.main]\nworkspace = '" + filepath.Join(t.TempDir(), "ws") + "'\nharnesses = ['pi']\n" +
		"[engine]\nurl = 'http://127.0.0.1:53000'\nkey_env = 'K'\nhost = 'localhost:53000'\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = c
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{URL: "http://127.0.0.1:18090"}, p)
	if err != nil || res.Engine != nil || !strings.Contains(out.String(), "Nothing answers at http://127.0.0.1:18090 now") {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if res.Answers.Engine != (config.Engine{API: "auto", URL: "http://127.0.0.1:18090"}) {
		t.Errorf("the old key_env and host must not follow a new url: %+v", res.Answers.Engine)
	}
	// The same address keeps them.
	res, err = Gather(f, Options{}, p)
	if err != nil || res.Answers.Engine != (config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000", KeyEnv: "K", Host: "localhost:53000"}) {
		t.Errorf("same url: %+v %v", res.Answers.Engine, err)
	}
}

func TestGatherAllowHostsAreChecked(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	input := strings.Join([]string{"1", "n", "1", "none", ws, "", "allow", "10.0.0.1, pypi.org", "pypi.org", "4g"}, "\n") + "\n"
	p, out := prompter(input)
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(res.Answers.Allow, ",") != "pypi.org" || !strings.Contains(out.String(), `"10.0.0.1" is not a host name`) ||
		!strings.Contains(out.String(), "Single tools: fd, jq, wip\n") {
		t.Errorf("%v\n%s", res.Answers.Allow, out)
	}
}

// ezbx up contacts only the configured address (and a key is only sent
// there), so choosing a server always writes where it was found.
func TestGatherPinsTheURL(t *testing.T) {
	f := facts(t)
	p, _ := prompter("")
	p.Auto = true
	o := Options{KeyEnv: "K", Harnesses: []string{"pi"}, Workspace: filepath.Join(t.TempDir(), "ws")}
	res, err := Gather(f, o, p)
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Answers.Engine; e != (config.Engine{API: "lmstudio", URL: "http://127.0.0.1:53000", KeyEnv: "K"}) {
		t.Errorf("%+v", e)
	}
}

func TestGatherExplainsRefusals(t *testing.T) {
	f := facts(t)
	f.Refused = []detect.Refusal{{URL: "http://127.0.0.1:18081"}, {URL: "http://127.0.0.1:18082", KeySent: true}}
	p, out := prompter("")
	p.Auto = true
	if _, err := Gather(f, Options{Later: true, Harnesses: []string{"pi"}, Workspace: filepath.Join(t.TempDir(), "ws")}, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"18081 asks for a key", "--url http://127.0.0.1:18081 --key-env NAME", "18082 refused the key"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

// The disk estimate grows with the harnesses when images must be built,
// and the review warns about a drive with less free space than that.
func TestDiskWarning(t *testing.T) {
	p := plan(t)
	built := p.DiskNeed()
	if built != headroom {
		t.Errorf("with every image built, need %d, want only the headroom %d", built, headroom)
	}
	p.Images[1].Present = false
	one := p.DiskNeed()
	if one != headroom+2*firstHarness {
		t.Errorf("one harness to build: need %d", one)
	}
	p.Result.Answers.Harnesses = []string{"pi", "opencode", "codex"}
	if three := p.DiskNeed(); three != one+2*2*moreHarness {
		t.Errorf("three harnesses to build: need %d", three)
	}
	p.NoStart = true // nothing is built
	if p.DiskNeed() != headroom {
		t.Errorf("--no-start builds nothing: need %d", p.DiskNeed())
	}
	p.NoStart = false

	need := p.DiskNeed()
	p.Disks = []Disk{{What: "the state folder", Path: "/home/example", Free: need}}
	if text := p.Review(); strings.Contains(text, "Warning: about") || !strings.Contains(text, "Disk          "+Size(need)+" free at /home/example, the state folder") {
		t.Errorf("exactly enough space:\n%s", text)
	}
	p.Disks = append(p.Disks, Disk{What: "Docker Desktop's disk", Path: "/srv/docker", Free: need - 1})
	text := p.Review()
	if !strings.Contains(text, "Warning: about "+Size(need)) || !strings.Contains(text, "/srv/docker has only") || !strings.Contains(text, "docker system df") {
		t.Errorf("low space not warned:\n%s", text)
	}
	if len(p.LowDisks()) != 1 {
		t.Errorf("low disks: %+v", p.LowDisks())
	}
	// A drive whose free space couldn't be read is shown, not warned about.
	p.Disks = []Disk{{What: "the state folder", Path: "X:\\", Err: errors.New("no")}}
	if text := p.Review(); strings.Contains(text, "Warning: about") || !strings.Contains(text, "free space unknown at X:\\") {
		t.Errorf("unknown space:\n%s", text)
	}
	if Size(1536<<20) != "1.5 GB" || Size(820<<20) != "820 MB" {
		t.Errorf("Size: %s, %s", Size(1536<<20), Size(820<<20))
	}
}

// A model named by a file path is warned about on the review screen, with
// the fix.
func TestReviewWarnsAboutFileNamedModels(t *testing.T) {
	p := plan(t)
	if strings.Contains(p.Review(), "by a file path") {
		t.Fatal("warned with no file-named model")
	}
	e := *p.Result.Engine
	e.Models = []detect.Model{{ID: "/models/big-model.gguf"}}
	p.Result.Engine = &e
	if text := p.Review(); !strings.Contains(text, "Warning: The server at http://127.0.0.1:53000 names 1 model(s) by a file path") || !strings.Contains(text, "in the server's settings, then run ezbx init again") {
		t.Errorf("no warning:\n%s", text)
	}
}
