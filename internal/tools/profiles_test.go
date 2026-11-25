package tools

import (
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/catalog"
)

func TestRealProfiles(t *testing.T) {
	c, err := Load(catalog.FS)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.ProfileIDs(), ",")
	if got != "offline,open,python-ml,research,web-dev" {
		t.Fatalf("ProfileIDs = %q, want offline,open,python-ml,research,web-dev", got)
	}
	if off := c.Profiles["offline"]; off.Network != "off" || len(off.Allow) != 0 {
		t.Errorf("offline profile wrong: %+v", off)
	}
	if op := c.Profiles["open"]; op.Network != "on" || len(op.Allow) != 0 || strings.Join(op.Tools, ",") != "pack:coding,pack:web,pack:media" {
		t.Errorf("open profile wrong: %+v", op)
	}
	if wd := c.Profiles["web-dev"]; wd.Network != "allow" || len(wd.Allow) == 0 || strings.Join(wd.Tools, ",") != "pack:coding,pack:web" {
		t.Errorf("web-dev profile wrong: %+v", wd)
	}
}

func TestProfileIDsWithoutOffline(t *testing.T) {
	c, err := load(map[string]string{
		"tools/a.json":      tool("a", nil),
		"profiles/b.json":   `{"schema":1,"id":"b","name":"B","description":"D","harnesses":["pi"],"tools":["a"],"network":"off","allow":[]}`,
		"profiles/a.json":   `{"schema":1,"id":"a","name":"A","description":"D","harnesses":["pi"],"tools":["a"],"network":"off","allow":[]}`,
		"profiles/off.json": `{"schema":1,"id":"off","name":"O","description":"D","harnesses":["pi"],"tools":["a"],"network":"on","allow":[]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ProfileIDs(), ","); got != "a,b,off" {
		t.Errorf("got %q, want a,b,off", got)
	}
}

func TestProfileIDsPutsOfflineFirstEvenAfterAlpha(t *testing.T) {
	c, err := load(map[string]string{
		"tools/a.json":          tool("a", nil),
		"profiles/alpha.json":   `{"schema":1,"id":"alpha","name":"Alpha","description":"D","harnesses":["pi"],"tools":["a"],"network":"off","allow":[]}`,
		"profiles/offline.json": `{"schema":1,"id":"offline","name":"Offline","description":"D","harnesses":["pi"],"tools":["a"],"network":"off","allow":[]}`,
		"profiles/zeta.json":    `{"schema":1,"id":"zeta","name":"Zeta","description":"D","harnesses":["pi"],"tools":["a"],"network":"off","allow":[]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ProfileIDs(), ","); got != "offline,alpha,zeta" {
		t.Errorf("got %q, want offline,alpha,zeta", got)
	}
}

func TestLoadRefusesBadProfiles(t *testing.T) {
	baseTools := map[string]string{
		"tools/a.json": tool("a", nil),
		"tools/wip.json": tool("wip", func(s string) string {
			return strings.Replace(s, `"status": "verified", "observedOn": "2026-01-02", "notes": "checked"`, `"status": "unverified", "observedOn": "", "notes": ""`, 1)
		}),
	}
	goodProfile := `{"schema": 1, "id": "p", "name": "P", "description": "D", "harnesses": ["pi"], "tools": ["a"], "network": "allow", "allow": ["github.com"]}`
	repl := func(old, new string) string {
		return strings.Replace(goodProfile, old, new, 1)
	}
	cases := map[string]struct {
		file string
		body string
		want string
	}{
		"unknown field":        {"profiles/p.json", repl(`"name":`, `"extra": 1, "name":`), "unknown field"},
		"wrong schema":         {"profiles/p.json", repl(`"schema": 1`, `"schema": 2`), "schema"},
		"bad id":               {"profiles/P.json", repl(`"id": "p"`, `"id": "P"`), "lowercase"},
		"id vs file":           {"profiles/p.json", repl(`"id": "p"`, `"id": "other"`), "match the file name"},
		"empty name":           {"profiles/p.json", repl(`"name": "P"`, `"name": ""`), "name and description"},
		"empty description":    {"profiles/p.json", repl(`"description": "D"`, `"description": ""`), "name and description"},
		"empty harnesses":      {"profiles/p.json", repl(`["pi"]`, `[]`), "at least one harness"},
		"missing tools":        {"profiles/p.json", repl(`"tools": ["a"], `, ``), "tools list is required"},
		"bad harness id":       {"profiles/p.json", repl(`["pi"]`, `["Pi!"]`), "lowercase"},
		"duplicate harness":    {"profiles/p.json", repl(`["pi"]`, `["pi", "pi"]`), "listed twice"},
		"unknown tool":         {"profiles/p.json", repl(`["a"]`, `["nope"]`), `unknown tool "nope"`},
		"unverified tool":      {"profiles/p.json", repl(`["a"]`, `["wip"]`), `"wip" isn't verified`},
		"unknown pack":         {"profiles/p.json", repl(`["a"]`, `["pack:nope"]`), `unknown tool pack`},
		"bad network":          {"profiles/p.json", repl(`"allow"`, `"open"`), "use airgap, off, allow or on"},
		"allow when off":       {"profiles/p.json", repl(`"network": "allow"`, `"network": "off"`), "cannot have an allow list"},
		"allow when on":        {"profiles/p.json", repl(`"network": "allow"`, `"network": "on"`), "cannot have an allow list"},
		"allow IP address":     {"profiles/p.json", repl(`"github.com"`, `"10.0.0.1"`), "is not a host name"},
		"allow TLD wildcard":   {"profiles/p.json", repl(`"github.com"`, `"*.com"`), "is not a host name"},
		"allow uppercase host": {"profiles/p.json", repl(`"github.com"`, `"GitHub.com"`), "must be lowercase"},
		"allow duplicate host": {"profiles/p.json", repl(`["github.com"]`, `["github.com", "github.com"]`), "listed twice"},
	}
	for name, tc := range cases {
		files := map[string]string{}
		for k, v := range baseTools {
			files[k] = v
		}
		files[tc.file] = tc.body
		if _, err := load(files); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
