package box

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveReadOnly(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "example")
	ws := filepath.Join(home, "ezbx", "workspace")
	state := filepath.Join(home, "appdata", "ezbx")
	sys := filepath.Join(root, "windows")
	own, system := []string{state}, []string{sys}

	got, err := ResolveReadOnly([]ReadOnlyEntry{
		{Path: "~/Projects"},
		{Path: filepath.Join(root, "ref", "Docs") + string(filepath.Separator), Name: "manuals", Mask: []string{"private/"}, Unmask: []string{".npmrc"}},
	}, ws, home, own, system)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != filepath.Join(home, "Projects") || got[0].Name != "projects" ||
		got[1].Path != filepath.Join(root, "ref", "Docs") || got[1].Name != "manuals" {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got[1].Matcher.Match("x/private", true); !ok {
		t.Error("the folder's own mask isn't used")
	}
	if _, ok := got[1].Matcher.Match(".npmrc", false); ok {
		t.Error("the folder's unmask isn't used")
	}
	if _, ok := got[0].Matcher.Match(".npmrc", false); !ok {
		t.Error("one folder's unmask reached another")
	}

	vol := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		vol = filepath.VolumeName(root) + `\`
	}
	bad := map[string]struct {
		entries []ReadOnlyEntry
		want    string
	}{
		"relative":      {[]ReadOnlyEntry{{Path: "projects"}}, "full path"},
		"drive root":    {[]ReadOnlyEntry{{Path: vol}}, "root of a drive"},
		"home":          {[]ReadOnlyEntry{{Path: home}}, "home folder"},
		"above home":    {[]ReadOnlyEntry{{Path: filepath.Dir(home)}}, "home folder"},
		"state":         {[]ReadOnlyEntry{{Path: filepath.Join(state, "x")}}, "ezbx's own folder"},
		"holds state":   {[]ReadOnlyEntry{{Path: filepath.Join(home, "appdata")}}, "ezbx's own folder"},
		"system":        {[]ReadOnlyEntry{{Path: filepath.Join(sys, "system32")}}, "system folder"},
		"workspace":     {[]ReadOnlyEntry{{Path: ws}}, "overlaps the workspace"},
		"in workspace":  {[]ReadOnlyEntry{{Path: filepath.Join(ws, "sub")}}, "overlaps the workspace"},
		"holds ws":      {[]ReadOnlyEntry{{Path: filepath.Join(home, "ezbx")}}, "overlaps the workspace"},
		"ssh":           {[]ReadOnlyEntry{{Path: filepath.Join(home, ".ssh")}}, `default ".ssh/"`},
		"SSH any case":  {[]ReadOnlyEntry{{Path: filepath.Join(home, ".SSH")}}, `default ".ssh/"`},
		"gcloud":        {[]ReadOnlyEntry{{Path: filepath.Join(home, ".config", "gcloud")}}, `default ".config/gcloud/"`},
		"overlap":       {[]ReadOnlyEntry{{Path: filepath.Join(root, "a")}, {Path: filepath.Join(root, "a", "b")}}, "overlap"},
		"overlap other": {[]ReadOnlyEntry{{Path: filepath.Join(root, "a", "b")}, {Path: filepath.Join(root, "a")}}, "overlap"},
		"same name":     {[]ReadOnlyEntry{{Path: filepath.Join(root, "x", "docs")}, {Path: filepath.Join(root, "y", "Docs")}}, "both be /readonly/docs"},
		"name vs given": {[]ReadOnlyEntry{{Path: filepath.Join(root, "x", "docs")}, {Path: filepath.Join(root, "y", "z"), Name: "docs"}}, "both be /readonly/docs"},
		"unusable name": {[]ReadOnlyEntry{{Path: filepath.Join(root, "My Docs")}}, "set name"},
		"bad unmask":    {[]ReadOnlyEntry{{Path: filepath.Join(root, "p"), Unmask: []string{"npmrc"}}}, "isn't one of the default masks"},
		"bad mask":      {[]ReadOnlyEntry{{Path: filepath.Join(root, "p"), Mask: []string{"/abs"}}}, "starts with /"},
	}
	for name, c := range bad {
		_, err := ResolveReadOnly(c.entries, ws, home, own, system)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
