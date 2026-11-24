package setup

import (
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/OranjSvensk/ezbx/catalog"
	"github.com/OranjSvensk/ezbx/harnesses"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/tools"
)

func realFacts(t *testing.T) Facts {
	t.Helper()
	cat, err := tools.Load(catalog.FS)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		t.Fatal(err)
	}
	var verified []harness.Manifest
	for _, m := range hs {
		if m.Verified.Status == "verified" {
			verified = append(verified, m)
		}
	}
	f := facts(t)
	f.Catalog = cat
	f.Harnesses = verified
	return f
}

// Every shipped profile matches real verified harnesses and tools, and
// produces a valid ezbx.toml that round-trips cleanly.
func TestAllRealProfilesRoundTrip(t *testing.T) {
	f := realFacts(t)
	if len(f.Catalog.Profiles) != 4 {
		t.Fatalf("want 4 profiles, got %d", len(f.Catalog.Profiles))
	}
	for _, id := range f.Catalog.ProfileIDs() {
		pr := f.Catalog.Profiles[id]
		ws := filepath.Join(t.TempDir(), "ws")
		p, _ := prompter("")
		p.Auto = true
		res, err := Gather(f, Options{Profile: id, Later: true, Workspace: ws}, p)
		if err != nil {
			t.Fatalf("profile %s: %v", id, err)
		}
		text := ConfigText(res.Answers, time.Now())
		if strings.Contains(text, "profile =") {
			t.Errorf("profile %s should expand to plain fields, not a profile key:\n%s", id, text)
		}
		c, err := config.Parse(text)
		if err != nil {
			t.Fatalf("profile %s config parse: %v\n%s", id, err, text)
		}
		back := FromConfig(c)
		if back.Network != pr.Network || strings.Join(back.Tools, ",") != strings.Join(pr.Tools, ",") ||
			strings.Join(back.Harnesses, ",") != strings.Join(pr.Harnesses, ",") ||
			strings.Join(back.Allow, ",") != strings.Join(pr.Allow, ",") {
			t.Errorf("profile %s mismatch: got %+v, want profile %+v", id, back, pr)
		}
	}
}

func TestProfileInteractivePromptAndDefaults(t *testing.T) {
	f := realFacts(t)
	ws := filepath.Join(t.TempDir(), "ws")

	// Default on a fresh config picks offline (item 1).
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{Later: true, Workspace: ws}, p)
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers; a.Network != "off" || len(a.Allow) != 0 || strings.Join(a.Tools, ",") != "pack:coding" || strings.Join(a.Harnesses, ",") != "pi" {
		t.Errorf("default profile wrong: %+v", a)
	}
	for _, id := range []string{"offline", "python-ml", "research", "web-dev"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("prompt missing profile %s\n%s", id, out.String())
		}
	}

	// Interactive selection: engine L, bad profile input then 4 (web-dev), then accept defaults.
	input := strings.Join([]string{"L", "99", "4", "", "", ws, "", "", "", ""}, "\n") + "\n"
	p, out = prompter(input)
	res, err = Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Type a number from 1 to 4.") {
		t.Errorf("missing profile validation hint:\n%s", out.String())
	}
	if a := res.Answers; a.Network != "allow" || strings.Join(a.Tools, ",") != "pack:coding,pack:web" ||
		strings.Join(a.Allow, ",") != "api.github.com,github.com,raw.githubusercontent.com,registry.npmjs.org" {
		t.Errorf("picked web-dev wrong: %+v", a)
	}
}

func TestProfileFlagOverridesAndReruns(t *testing.T) {
	f := realFacts(t)
	ws := filepath.Join(t.TempDir(), "ws")

	// Flags override profile fields, and switching network away from allow clears profile Allow.
	p, _ := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{
		Profile:   "web-dev",
		Later:     true,
		Harnesses: []string{"claude"},
		Tools:     []string{"jq"},
		Workspace: ws,
		Network:   "off",
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers; strings.Join(a.Harnesses, ",") != "claude" || strings.Join(a.Tools, ",") != "jq" ||
		a.Network != "off" || len(a.Allow) != 0 {
		t.Errorf("flag overrides failed: %+v", a)
	}

	// Interactive override of network from web-dev's allow to off also clears Allow.
	input := strings.Join([]string{"L", "4", "", "", ws, "", "off", ""}, "\n") + "\n"
	p, out := prompter(input)
	res, err = Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Answers.Network != "off" || len(res.Answers.Allow) != 0 {
		t.Errorf("interactive network override kept Allow: %+v", res.Answers)
	}

	// Re-running on an existing config without --profile skips the profile prompt and changes nothing.
	existingText := ConfigText(res.Answers, time.Now())
	c, err := config.Parse(existingText)
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = c
	p, out = prompter("")
	p.Auto = true
	rerun, err := Gather(f, Options{Later: true}, p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Starter profiles") {
		t.Errorf("re-run without --profile should not ask starter profile:\n%s", out.String())
	}
	if ch := Changes(FromConfig(c), rerun.Answers); len(ch) != 0 {
		t.Errorf("re-run changed answers: %v", ch)
	}

	// Re-running with --profile research from an offline config updates tools, network and allow.
	offRes, err := Gather(realFacts(t), Options{Profile: "offline", Later: true, Workspace: ws}, p)
	if err != nil {
		t.Fatal(err)
	}
	cOff, err := config.Parse(ConfigText(offRes.Answers, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	fOff := realFacts(t)
	fOff.Existing = cOff
	resResearch, err := Gather(fOff, Options{Profile: "research", Later: true}, p)
	if err != nil {
		t.Fatal(err)
	}
	diff := strings.Join(Changes(FromConfig(cOff), resResearch.Answers), "\n")
	if !strings.Contains(diff, "network    off -> allow") || !strings.Contains(diff, "arxiv.org") || !strings.Contains(diff, "pack:coding -> pack:coding,pack:web") {
		t.Errorf("re-run with --profile research diff wrong:\n%s", diff)
	}

	// Re-running with --profile offline clears the allowlist.
	cResearch, err := config.Parse(ConfigText(resResearch.Answers, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = cResearch
	resOffline, err := Gather(f, Options{Profile: "offline", Later: true}, p)
	if err != nil {
		t.Fatal(err)
	}
	if resOffline.Answers.Network != "off" || len(resOffline.Answers.Allow) != 0 {
		t.Errorf("re-run with --profile offline did not clear allowlist: %+v", resOffline.Answers)
	}
}

func TestProfileErrorsAndFormat(t *testing.T) {
	f := realFacts(t)
	p, _ := prompter("")
	p.Auto = true

	if _, err := Gather(f, Options{Profile: "nope", Later: true}, p); err == nil || !strings.Contains(err.Error(), "offline, python-ml, research, web-dev") {
		t.Errorf("unknown profile: %v", err)
	}

	// Unknown profile when catalog has no profiles at all is also refused.
	fNoProfiles := facts(t)
	if _, err := Gather(fNoProfiles, Options{Profile: "offline", Later: true}, p); err == nil || !strings.Contains(err.Error(), "no starter profiles available") {
		t.Errorf("no profiles available: %v", err)
	}

	// Profile naming an unavailable harness is refused.
	cat, err := tools.Load(fstest.MapFS{
		"tools/fd.json":       {Data: []byte(`{"schema":1,"id":"fd","name":"fd","description":"d","debian":["fd"],"commands":["fd"],"verified":{"status":"verified","observedOn":"2026-01-02","notes":"ok"}}`)},
		"profiles/weird.json": {Data: []byte(`{"schema":1,"id":"weird","name":"W","description":"D","harnesses":["missing"],"tools":["fd"],"network":"off","allow":[]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fBadHarness := realFacts(t)
	fBadHarness.Catalog = cat
	if _, err := Gather(fBadHarness, Options{Profile: "weird", Later: true}, p); err == nil || !strings.Contains(err.Error(), `harness "missing", which is not available`) {
		t.Errorf("missing harness in profile: %v", err)
	}

	if got := FormatProfiles(nil); !strings.Contains(got, "No starter profiles") {
		t.Errorf("FormatProfiles(nil) = %q", got)
	}
	formatted := FormatProfiles(f.Catalog)
	for _, want := range []string{"offline", "Offline coding", "python-ml", "pypi.org", "research", "arxiv.org", "web-dev", "registry.npmjs.org"} {
		if !strings.Contains(formatted, want) {
			t.Errorf("FormatProfiles missing %q\n%s", want, formatted)
		}
	}

	// A profile with no extra tools sets an explicit empty slice rather than nil.
	emptyToolsCat, err := tools.Load(fstest.MapFS{
		"tools/fd.json":      {Data: []byte(`{"schema":1,"id":"fd","name":"fd","description":"d","debian":["fd"],"commands":["fd"],"verified":{"status":"verified","observedOn":"2026-01-02","notes":"ok"}}`)},
		"profiles/bare.json": {Data: []byte(`{"schema":1,"id":"bare","name":"Bare","description":"D","harnesses":["pi"],"tools":[],"network":"off","allow":[]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fBare := realFacts(t)
	fBare.Catalog = emptyToolsCat
	resBare, err := Gather(fBare, Options{Profile: "bare", Later: true, Workspace: filepath.Join(t.TempDir(), "ws")}, p)
	if err != nil {
		t.Fatal(err)
	}
	if resBare.Answers.Tools == nil || len(resBare.Answers.Tools) != 0 {
		t.Errorf("bare profile should set empty non-nil tools, got %#v", resBare.Answers.Tools)
	}
}
