package readonly

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is a file whose contents look like they hold a secret. It names
// the kind of secret, never the text that matched.
type Finding struct {
	Rel  string
	Kind string
}

type sign struct {
	kind string
	re   *regexp.Regexp
}

// signs are well-known secret shapes. They are guesses: a match is a
// warning, not proof (docs/readonly.md, R4).
var signs = []sign{
	{"a private key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}|\bgithub_pat_[A-Za-z0-9_]{40,}`)},
	{"an API key (sk-)", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{24,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
}

// A user name and password written into a remote's URL, as in
// https://name:secret@host/repo. Only checked in .git/config.
var urlCredential = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)

// Scan reads each file (paths relative to root, as Walk returns them) and
// reports those that look like they hold a secret, one finding per kind.
// Files that look binary are skipped. Files that can't be read are skipped
// too: the box can't read them either.
func Scan(root string, files []string) []Finding {
	var out []Finding
	for _, rel := range files {
		data, err := readSmall(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || binary(data) {
			continue
		}
		for _, s := range signs {
			if s.re.Match(data) {
				out = append(out, Finding{Rel: rel, Kind: s.kind})
			}
		}
		if isGitConfig(rel) && urlCredential.Match(data) {
			out = append(out, Finding{Rel: rel, Kind: "a password in a remote's URL"})
		}
	}
	return out
}

func readSmall(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxScanSize))
}

// binary reports a NUL byte near the start, as git does.
func binary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) >= 0
}

func isGitConfig(rel string) bool {
	rel = strings.ToLower(rel)
	return rel == ".git/config" || strings.HasSuffix(rel, "/.git/config")
}
