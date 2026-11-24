// Package readonly works out what a read-only folder shows the agent: which
// files and folders in it are masked, and which other files look like they
// hold secrets. It reads the host folder only; mounting is done by box.
//
// See docs/readonly.md for the design.
package readonly

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Defaults are masked in every read-only folder. A trailing slash means
// folders only. See docs/readonly.md, "The default masks".
var Defaults = []string{
	// environment files
	".env", ".env.*",
	// keys and certificates
	"*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ecdsa*", "id_ed25519*",
	// credential folders
	".ssh/", ".aws/", ".gnupg/", ".kube/", ".azure/", ".config/gcloud/",
	// credential files
	".netrc", ".npmrc", ".pypirc", ".git-credentials", ".docker/config.json", "credentials.json",
}

// IsDefault reports whether p is one of the default masks, written exactly.
func IsDefault(p string) bool {
	for _, d := range Defaults {
		if p == d {
			return true
		}
	}
	return false
}

// CheckPattern refuses a mask or unmask pattern that can't mean what it
// seems to: empty, absolute, with backslashes, ".." or empty parts.
func CheckPattern(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("is empty")
	}
	if strings.Contains(p, `\`) {
		return errors.New("has a backslash; use / between folders")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("starts with /; start it with ./ to match from the folder's root")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
	if body == "" {
		return errors.New("names nothing")
	}
	for _, seg := range strings.Split(body, "/") {
		switch seg {
		case "":
			return errors.New("has an empty part (//)")
		case ".", "..":
			return fmt.Errorf("has %q in it", seg)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("isn't a valid pattern: %v", err)
		}
	}
	return nil
}

type rule struct {
	pattern  string   // as written, for messages
	segs     []string // lowercased
	anchored bool     // started with ./
	dirOnly  bool     // ended with /
}

// Matcher decides which names in a folder are masked.
type Matcher struct{ rules []rule }

// NewMatcher combines the defaults, minus unmask, with mask. Patterns must
// already pass CheckPattern, and each unmask entry must be a default.
func NewMatcher(mask, unmask []string) (Matcher, error) {
	var m Matcher
	skip := map[string]bool{}
	for _, u := range unmask {
		if !IsDefault(u) {
			return m, fmt.Errorf("unmask %q isn't one of the default masks; write it exactly as listed in docs/readonly.md", u)
		}
		skip[u] = true
	}
	for _, p := range append(append([]string{}, Defaults...), mask...) {
		if skip[p] {
			continue
		}
		if err := CheckPattern(p); err != nil {
			return m, fmt.Errorf("mask %q %v", p, err)
		}
		m.rules = append(m.rules, compile(p))
	}
	return m, nil
}

func compile(p string) rule {
	r := rule{pattern: p, dirOnly: strings.HasSuffix(p, "/"), anchored: strings.HasPrefix(p, "./")}
	body := strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
	r.segs = strings.Split(strings.ToLower(body), "/")
	return r
}

// Match reports the first pattern that masks rel, a path inside the folder
// with / between parts. Case is ignored, as on Windows.
func (m Matcher) Match(rel string, isDir bool) (string, bool) {
	parts := strings.Split(strings.ToLower(rel), "/")
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		n := len(r.segs)
		if len(parts) < n || (r.anchored && len(parts) != n) {
			continue
		}
		tail := parts[len(parts)-n:]
		ok := true
		for i, seg := range r.segs {
			if hit, _ := path.Match(seg, tail[i]); !hit {
				ok = false
				break
			}
		}
		if ok {
			return r.pattern, true
		}
	}
	return "", false
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// CheckName refuses a name that can't be a folder under /readonly.
func CheckName(n string) error {
	if !nameRE.MatchString(n) || strings.Contains(n, "..") {
		return fmt.Errorf("name %q: use lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit", n)
	}
	return nil
}

// DefaultName is the folder's own name, lowercased: what it is called under
// /readonly when the config gives no name.
func DefaultName(folder string) string {
	// path.Base drops trailing slashes itself.
	return strings.ToLower(path.Base(strings.ReplaceAll(folder, `\`, "/")))
}
