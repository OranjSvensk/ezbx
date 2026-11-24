package readonly

import (
	"strings"
	"testing"
)

func TestCheckPattern(t *testing.T) {
	for _, ok := range []string{".env", "*.pem", "private/", "./config/secrets.yml", ".docker/config.json", "a?b", "./top/"} {
		if err := CheckPattern(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	bs := string(rune(92))
	bad := map[string]string{
		"":             "empty",
		"  ":           "empty",
		"a" + bs + "b": "backslash",
		"/x":           "starts with /",
		"/":            "starts with /",
		"./":           "names nothing",
		"a//b":         "empty part",
		"../x":         `".."`,
		"a/../b":       `".."`,
		"a/./b":        `"."`,
		"[":            "valid pattern",
	}
	for p, want := range bad {
		err := CheckPattern(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", p, err, want)
		}
	}
}

func TestDefaultsAreValid(t *testing.T) {
	for _, d := range Defaults {
		if err := CheckPattern(d); err != nil {
			t.Errorf("default %q: %v", d, err)
		}
		if !IsDefault(d) {
			t.Errorf("IsDefault(%q) = false", d)
		}
	}
	if IsDefault(".ENV") || IsDefault("env") {
		t.Error("IsDefault must want the default written exactly")
	}
}

func TestDefaultMasks(t *testing.T) {
	m, err := NewMatcher(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	masked := []struct {
		rel string
		dir bool
		pat string
	}{
		{".env", false, ".env"},
		{"sub/.env", false, ".env"},
		{"Upper/.ENV", false, ".env"},
		{".env.local", false, ".env.*"},
		{"keys/server.pem", false, "*.pem"},
		{"tls/site.KEY", false, "*.key"},
		{"id_rsa.pub", false, "id_rsa*"},
		{"a/id_ed25519", false, "id_ed25519*"},
		{".ssh", true, ".ssh/"},
		{"deep/er/.aws", true, ".aws/"},
		{".config/gcloud", true, ".config/gcloud/"},
		{"x/.config/gcloud", true, ".config/gcloud/"},
		{"h/.docker/config.json", false, ".docker/config.json"},
		{".npmrc", false, ".npmrc"},
		{"svc/credentials.json", false, "credentials.json"},
	}
	for _, c := range masked {
		pat, ok := m.Match(c.rel, c.dir)
		if !ok || pat != c.pat {
			t.Errorf("%s (dir %v): got %q %v, want %q", c.rel, c.dir, pat, ok, c.pat)
		}
	}
	shown := []struct {
		rel string
		dir bool
	}{
		{"app.txt", false},
		{"env", false},
		{".environment", false},
		{"x.pem.txt", false},
		{".ssh", false}, // a file named .ssh: folder patterns don't apply
		{"config.json", false},
		{".docker", true},
		{"gcloud", true},
		{"notes/.ssh/readme", false}, // only its folder matches; the walk never goes in
	}
	for _, c := range shown {
		if pat, ok := m.Match(c.rel, c.dir); ok {
			t.Errorf("%s (dir %v): masked by %q", c.rel, c.dir, pat)
		}
	}
}

func TestAnchoredAndUnanchored(t *testing.T) {
	m, err := NewMatcher([]string{"./config/secrets.yml", "vault/token.txt", "private/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel  string
		dir  bool
		want bool
	}{
		{"config/secrets.yml", false, true},
		{"a/config/secrets.yml", false, false},
		{"secrets.yml", false, false},
		{"vault/token.txt", false, true},
		{"a/b/vault/token.txt", false, true},
		{"token.txt", false, false},
		{"private", true, true},
		{"x/private", true, true},
		{"private", false, false},
	}
	for _, c := range cases {
		if _, got := m.Match(c.rel, c.dir); got != c.want {
			t.Errorf("%s (dir %v): masked %v, want %v", c.rel, c.dir, got, c.want)
		}
	}
}

func TestUnmask(t *testing.T) {
	m, err := NewMatcher(nil, []string{".npmrc", ".env.*"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Match(".npmrc", false); ok {
		t.Error(".npmrc still masked")
	}
	if _, ok := m.Match(".env.example", false); ok {
		t.Error(".env.example still masked")
	}
	if _, ok := m.Match(".env", false); !ok {
		t.Error(".env no longer masked")
	}
	if _, err := NewMatcher(nil, []string{"npmrc"}); err == nil || !strings.Contains(err.Error(), "isn't one of the default masks") {
		t.Errorf("unmask of a non-default: %v", err)
	}
	if _, err := NewMatcher([]string{"a//b"}, nil); err == nil || !strings.Contains(err.Error(), "empty part") {
		t.Errorf("bad mask: %v", err)
	}
}

func TestNames(t *testing.T) {
	for _, ok := range []string{"projects", "a", "my-docs_2", "v1.2", "0x"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Projects", "my docs", ".hidden", "-x", "a/b", "a..b", strings.Repeat("a", 64)} {
		if CheckName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	bs := string(rune(92))
	for in, want := range map[string]string{
		"/srv/Projects":                  "projects",
		"/srv/Projects/":                 "projects",
		"srv" + bs + "Docs" + bs + "Ref": "ref",
	} {
		if got := DefaultName(in); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", in, got, want)
		}
	}
}
