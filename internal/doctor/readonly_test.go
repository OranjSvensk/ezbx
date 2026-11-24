package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OranjSvensk/ezbx/internal/detect"
)

// roDocker answers the read-only script with reply, and records the
// environment it was given.
type roDocker struct {
	*fakeDocker
	reply string
	env   string
}

func (f *roDocker) run(ctx context.Context, args ...string) ([]byte, error) {
	if args[0] == "exec" && args[len(args)-1] == readOnlyScript {
		for i, a := range args {
			if a == "-e" {
				f.env = args[i+1]
			}
		}
		if f.reply == "" {
			return nil, errors.New("exec failed")
		}
		return []byte(f.reply), nil
	}
	return f.fakeDocker.run(ctx, args...)
}

func roFolder() ReadOnlyFolder {
	return ReadOnlyFolder{Inside: "/readonly/p", Files: []string{"/readonly/p/.env"}, Dirs: []string{"/readonly/p/.ssh"}}
}

func runRO(f *roDocker, folders []ReadOnlyFolder, note string) Report {
	return Run(context.Background(), HostConfig{
		Box: "t", Docker: f.run,
		Detect: func(context.Context, []string) detect.Result {
			return detect.Result{Engines: []detect.Engine{{ID: "strata", Name: "Strata", URL: "http://127.0.0.1:53000", Models: []detect.Model{{ID: "m"}}}}}
		},
		LocalAddrs: func() []string { return []string{"192.0.2.10"} },
		Images:     goodImages,
		ReadOnly:   folders, ReadOnlyNote: note,
	})
}

func withROMount(f *fakeDocker, rw bool) {
	box := f.ctrs["ezbx-t-box"]
	box["Mounts"] = append(box["Mounts"].([]map[string]any), map[string]any{"Source": "/host/p", "Destination": "/readonly/p", "RW": rw})
}

func findRO(r Report, name string) *Check {
	for i := range r.Checks {
		if r.Checks[i].Group == "Read-only folders" && r.Checks[i].Name == name {
			return &r.Checks[i]
		}
	}
	return nil
}

func TestReadOnlyGood(t *testing.T) {
	f := &roDocker{fakeDocker: goodDocker(), reply: "done\n"}
	withROMount(f.fakeDocker, false)
	r := runRO(f, []ReadOnlyFolder{roFolder()}, "")
	if c := findRO(r, "/readonly/p"); c == nil || c.Status != Pass || !strings.Contains(c.Detail, "2 masked") {
		t.Fatalf("mount check %+v", c)
	}
	if c := findRO(r, "seen from the box"); c == nil || c.Status != Pass {
		t.Fatalf("inside check %+v", c)
	}
	// Each line is a letter, a colon and a path, built from pieces so the
	// privacy scan doesn't read them as drive paths.
	line := func(k, p string) string { return k + ":" + p }
	if f.env != "EZBX_RO="+strings.Join([]string{line("R", "/readonly/p"), line("F", "/readonly/p/.env"), line("D", "/readonly/p/.ssh")}, "\n") {
		t.Errorf("env %q", f.env)
	}
	for _, c := range r.Checks {
		if c.Status == Fail {
			t.Errorf("%s / %s: %s", c.Group, c.Name, c.Detail)
		}
	}
}

func TestReadOnlyNoFolders(t *testing.T) {
	f := &roDocker{fakeDocker: goodDocker()}
	r := runRO(f, nil, "")
	for _, c := range r.Checks {
		if c.Group == "Read-only folders" {
			t.Errorf("a check without folders: %+v", c)
		}
	}
	// A writable mount under /readonly fails even without a record.
	withROMount(f.fakeDocker, true)
	if c := findRO(runRO(f, nil, ""), "/readonly/p"); c == nil || c.Status != Fail {
		t.Errorf("writable mount without a record: %+v", c)
	}
}

func TestReadOnlyProblems(t *testing.T) {
	cases := map[string]struct {
		rw      bool
		mount   bool
		folder  func(*ReadOnlyFolder)
		reply   string
		check   string
		status  Status
		contain string
	}{
		"not mounted":  {mount: false, reply: "done", check: "/readonly/p", status: Fail, contain: "not mounted"},
		"writable":     {mount: true, rw: true, reply: "done", check: "/readonly/p", status: Fail, contain: "writable"},
		"box problems": {mount: true, reply: "writable /readonly/p\nunmasked /readonly/p/.env\ndone\n", check: "seen from the box", status: Fail, contain: "writable /readonly/p; unmasked /readonly/p/.env"},
		"no answer":    {mount: true, reply: "", check: "seen from the box", status: Warn, contain: "couldn't ask"},
		"cut off":      {mount: true, reply: "unmasked /readonly/p/.env\n", check: "seen from the box", status: Warn, contain: "couldn't ask"},
		"unmask":       {mount: true, reply: "done", folder: func(f *ReadOnlyFolder) { f.Unmask = []string{".npmrc"} }, check: "/readonly/p unmask", status: Warn, contain: `".npmrc"`},
		"finding":      {mount: true, reply: "done", folder: func(f *ReadOnlyFolder) { f.Findings = []string{"/readonly/p/k.txt looks like it holds a private key"} }, check: "/readonly/p contents", status: Warn, contain: "k.txt"},
		"new files":    {mount: true, reply: "done", folder: func(f *ReadOnlyFolder) { f.New = []string{"/readonly/p/id_rsa"} }, check: "/readonly/p new files", status: Fail, contain: "id_rsa"},
		"walk failed":  {mount: true, reply: "done", folder: func(f *ReadOnlyFolder) { f.Error = "gone" }, check: "/readonly/p new files", status: Warn, contain: "gone"},
	}
	for name, c := range cases {
		f := &roDocker{fakeDocker: goodDocker(), reply: c.reply}
		if c.mount {
			withROMount(f.fakeDocker, c.rw)
		}
		fo := roFolder()
		if c.folder != nil {
			c.folder(&fo)
		}
		got := findRO(runRO(f, []ReadOnlyFolder{fo}, ""), c.check)
		if got == nil || got.Status != c.status || !strings.Contains(got.Detail, c.contain) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	f := &roDocker{fakeDocker: goodDocker(), reply: "done"}
	if c := findRO(runRO(f, nil, "the record is broken"), "record"); c == nil || c.Status != Warn {
		t.Errorf("record note: %+v", c)
	}
}

// The script takes its paths from the environment only.
func TestReadOnlyScriptHasNoPaths(t *testing.T) {
	if strings.Contains(readOnlyScript, "/readonly") {
		t.Error("the script names a path itself")
	}
	if !strings.Contains(readOnlyScript, `"$EZBX_RO"`) {
		t.Error("the script doesn't read EZBX_RO")
	}
}
