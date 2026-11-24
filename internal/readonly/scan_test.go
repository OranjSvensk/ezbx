package readonly

import (
	"fmt"
	"strings"
	"testing"
)

// Fake secrets, built from pieces so no whole one sits in the repository.
var (
	fakeKey    = "-----BEGIN " + "OPENSSH PRIVATE" + " KEY-----\nabc\n"
	fakeRSA    = "-----BEGIN " + "RSA PRIVATE" + " KEY-----\n"
	fakeAWS    = "id = AK" + "IA" + strings.Repeat("Q", 16) + "\n"
	fakeGitHub = "gh" + "p_" + strings.Repeat("a1", 18)
	fakeSK     = "s" + "k-ant-" + strings.Repeat("b2", 12)
	fakeSlack  = "xo" + "xb-" + "1234567890-abc"
	fakeGoogle = "AI" + "za" + strings.Repeat("C", 35)
	fakeRemote = "[remote \"origin\"]\n\turl = https://" + "name:" + "pw" + "@example.com/r.git\n"
)

func TestScan(t *testing.T) {
	files := map[string]string{
		"keys.txt":        fakeKey,
		"rsa.txt":         fakeRSA,
		"aws.ini":         fakeAWS,
		"gh.txt":          "token: " + fakeGitHub,
		"sk.txt":          "key=" + fakeSK,
		"slack.txt":       fakeSlack,
		"google.txt":      fakeGoogle,
		".git/config":     fakeRemote,
		"sub/.git/config": fakeRemote,
		"notes.txt":       fakeRemote, // a URL with a password outside .git/config isn't checked
		"binary.dat":      "\x00" + fakeKey,
		"plain.txt":       "nothing to see; AKIA too short; sk-short",
		"two.txt":         fakeAWS + fakeGitHub,
	}
	root := tree(t, files)
	var rels []string
	for r := range files {
		rels = append(rels, r)
	}
	got := map[string]bool{}
	for _, f := range Scan(root, rels) {
		got[fmt.Sprintf("%s: %s", f.Rel, f.Kind)] = true
	}
	want := []string{
		"keys.txt: a private key",
		"rsa.txt: a private key",
		"aws.ini: an AWS access key",
		"gh.txt: a GitHub token",
		"sk.txt: an API key (sk-)",
		"slack.txt: a Slack token",
		"google.txt: a Google API key",
		".git/config: a password in a remote's URL",
		"sub/.git/config: a password in a remote's URL",
		"two.txt: an AWS access key",
		"two.txt: a GitHub token",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q", w)
		}
		delete(got, w)
	}
	for extra := range got {
		t.Errorf("unexpected %q", extra)
	}
}

func TestScanSkipsUnreadable(t *testing.T) {
	root := tree(t, map[string]string{"a.txt": fakeAWS})
	if f := Scan(root, []string{"gone.txt", "a.txt"}); len(f) != 1 || f[0].Rel != "a.txt" {
		t.Errorf("got %v", f)
	}
}
