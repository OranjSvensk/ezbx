package ezbx

import (
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The project's name is ezbx, in prose, code and file names alike. The old
// spellings must not come back: any case of "ez" + "box", with or without a
// space (ezbx-box, the box image's name, is fine). Checks every file git
// tracks or would add, or, outside a git checkout, the carried files.
func TestOneBrandName(t *testing.T) {
	old := regexp.MustCompile(`(?i)\bez[ _]?` + `box`)
	for _, s := range []string{"EZ" + "Box", "ez" + "box", "Ez" + "box", "EZ " + "Box", "ez_" + "box", "the EZ" + "BOX tool"} {
		if !old.MatchString(s) {
			t.Errorf("the check misses %q", s)
		}
	}
	for _, s := range []string{"ezbx", "ezbx-box", "ezbx-box:38dd62af2351", "freezbox"} {
		if old.MatchString(s) {
			t.Errorf("the check wrongly flags %q", s)
		}
	}
	var files []string
	if out, err := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard").Output(); err == nil {
		files = strings.Split(strings.TrimSpace(string(out)), "\n")
	} else {
		_ = fs.WalkDir(Source, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, p)
			}
			return err
		})
	}
	if len(files) == 0 {
		t.Fatal("no files to check")
	}
	for _, f := range files {
		if old.MatchString(f) {
			t.Errorf("file name %s uses the old name; the project is ezbx", f)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			data, err = fs.ReadFile(Source, f)
		}
		if err != nil {
			continue // removed from disk but still in the index
		}
		for i, line := range strings.Split(string(data), "\n") {
			if m := old.FindString(line); m != "" {
				t.Errorf("%s:%d says %q; the project is ezbx", f, i+1, m)
			}
		}
	}
}

// Nothing git ignores may be carried inside the program: those are private
// files (agent notes, local configs, keys). This needs a git checkout, so it
// is skipped elsewhere.
func TestSourceHoldsNoIgnoredFiles(t *testing.T) {
	var files []string
	err := fs.WalkDir(Source, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"go.mod", "source.go", "docker/box.Dockerfile", "docker/gateway.Dockerfile", "harnesses/pi/package-lock.json", "catalog/packs/coding.json"} {
		found := false
		for _, f := range files {
			found = found || f == must
		}
		if !found {
			t.Errorf("%s is not carried", must)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")
	out, err := cmd.Output()
	// check-ignore exits 1 when nothing is ignored.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Errorf("git-ignored files are carried inside the program:\n%s", out)
}
