package readonly

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Junctions need no special rights on Windows, unlike links, so they test
// on every Windows computer that links are never followed.
func TestWalkJunctions(t *testing.T) {
	root := tree(t, map[string]string{"real.txt": "", "dir/inside.txt": ""})
	for _, name := range []string{".aws", "plain"} {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, name), filepath.Join(root, "dir")).CombinedOutput()
		if err != nil {
			t.Skipf("can't make a junction: %v %s", err, out)
		}
	}
	w, err := Walk(root, defaults(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Masks) != 1 || w.Masks[0].Rel != ".aws" || !w.Masks[0].Link || w.Masks[0].Dir {
		t.Errorf("masks %+v", w.Masks)
	}
	// "plain" isn't entered, so only the real folder's file is listed.
	if s := strings.Join(w.Scan, ","); s != "dir/inside.txt,real.txt" {
		t.Errorf("scan list %q", s)
	}
}
