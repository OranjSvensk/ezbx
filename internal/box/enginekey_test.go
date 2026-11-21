package box

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A made-up key, easy to search for in everything the fake Docker saw.
const testKey = "kv-docs-example-1"

func TestCleanKey(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                                 "abc",
		"abc\n":                               "abc",
		"abc\r\n":                             "abc",
		"with space and = / +":                "with space and = / +",
		strings.Repeat("k", MaxKeyLen):        strings.Repeat("k", MaxKeyLen),
		strings.Repeat("k", MaxKeyLen) + "\n": strings.Repeat("k", MaxKeyLen),
	} {
		got, err := CleanKey(in)
		if err != nil || got != want {
			t.Errorf("CleanKey(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "\n", "\r\n", "abc\n\n", "a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb", strings.Repeat("k", MaxKeyLen+1)} {
		_, err := CleanKey(bad)
		if err == nil {
			t.Errorf("CleanKey(%q) was accepted", bad)
			continue
		}
		if len(bad) > 2 && strings.Contains(err.Error(), bad) {
			t.Errorf("the error quotes the key: %v", err)
		}
	}
	if _, err := CleanKey("secret-part\nrest"); err == nil || strings.Contains(err.Error(), "secret-part") {
		t.Errorf("error: %v", err)
	}
}

func TestWriteAndReadKeyFile(t *testing.T) {
	dir := t.TempDir()
	file := KeyFileIn(dir)
	if err := WriteKeyFile(file, testKey+"\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != testKey {
		t.Fatalf("file holds %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", st.Mode().Perm())
		}
	}
	if got, err := ReadKeyFile(file); err != nil || got != testKey {
		t.Errorf("read back %q, %v", got, err)
	}
	// Writing again replaces it and leaves no temporary file behind.
	if err := WriteKeyFile(file, "second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadKeyFile(file); got != "second" {
		t.Errorf("after rewrite: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left in the folder: %v", entries)
	}
	// A bad key writes nothing.
	if err := WriteKeyFile(file, "a\nb"); err == nil {
		t.Error("a key with a line break was written")
	}
	if got, _ := ReadKeyFile(file); got != "second" {
		t.Errorf("a refused key changed the file: %q", got)
	}

	// The gateway's side refuses broken files.
	for name, content := range map[string]string{
		"empty":        "",
		"two lines":    "a\nb\n",
		"too long":     strings.Repeat("k", MaxKeyLen+1),
		"far too long": strings.Repeat("k", 1<<20),
	} {
		f := filepath.Join(dir, "bad")
		os.WriteFile(f, []byte(content), 0o600)
		if _, err := ReadKeyFile(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ReadKeyFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestRemoveKeyFile(t *testing.T) {
	dir := t.TempDir()
	if had, err := RemoveKeyFile(dir); had || err != nil {
		t.Errorf("no file: %v %v", had, err)
	}
	os.WriteFile(KeyFileIn(dir), []byte("k"), 0o600)
	if had, err := RemoveKeyFile(dir); !had || err != nil {
		t.Errorf("with a file: %v %v", had, err)
	}
	if _, err := os.Stat(KeyFileIn(dir)); !os.IsNotExist(err) {
		t.Error("the file is still there")
	}
}

func keyRunner(f *fakeDocker, env map[string]string) *Runner {
	r := runner(f)
	r.Getenv = func(k string) string { return env[k] }
	return r
}

// up writes the key to the file the gateway mounts, and the key itself is in
// nothing Docker is told: no argument, nothing sent on standard input.
func TestUpWritesKeyFileOnly(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	p.KeyEnv = "ENGINE_KEY"
	if err := keyRunner(f, map[string]string{"ENGINE_KEY": testKey}).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadKeyFile(p.KeyFile()); err != nil || got != testKey {
		t.Fatalf("key file: %q, %v", got, err)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), testKey) {
			t.Errorf("the key is in a docker command: %v", c)
		}
	}
	if strings.Contains(string(f.stdin), testKey) {
		t.Error("the key was copied into the box")
	}
	var gw []string
	for _, c := range f.calls {
		if c[0] == "create" && has(c, "--name", "ezbx-t-gateway") {
			gw = c
		}
	}
	if !has(gw, "-v", p.KeyFile()+":/run/ezbx/engine-key:ro") || !has(gw, "--key-file", "/run/ezbx/engine-key") {
		t.Errorf("the gateway doesn't get the key file: %v", gw)
	}
	for _, c := range f.calls {
		if c[0] == "create" && has(c, "--name", "ezbx-t-box") && strings.Contains(strings.Join(c, " "), "engine-key") {
			t.Errorf("the box mounts the key file: %v", c)
		}
	}

	// down removes it with the gateway.
	if err := runner(f).Down(context.Background(), "t", p.StateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.KeyFile()); !os.IsNotExist(err) {
		t.Error("down kept the key file")
	}
}

// A key that can't work stops up before anything is made or written.
func TestUpRefusesBadKey(t *testing.T) {
	for name, val := range map[string]string{"empty": "", "two lines": "a\nb"} {
		f := newFake()
		p := testPlan(t)
		p.KeyEnv = "ENGINE_KEY"
		err := keyRunner(f, map[string]string{"ENGINE_KEY": val}).Up(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "ENGINE_KEY") {
			t.Errorf("%s: %v", name, err)
		}
		if len(f.networks)+len(f.volumes)+len(f.containers) != 0 {
			t.Errorf("%s: created something", name)
		}
		if _, err := os.Stat(p.KeyFile()); !os.IsNotExist(err) {
			t.Errorf("%s: wrote a key file", name)
		}
	}
}

// An engine without a key leaves no key file from an earlier engine behind.
func TestUpWithoutKeyRemovesOldFile(t *testing.T) {
	p := testPlan(t)
	os.MkdirAll(p.StateDir, 0o700)
	os.WriteFile(p.KeyFile(), []byte("old"), 0o600)
	if err := runner(newFake()).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.KeyFile()); !os.IsNotExist(err) {
		t.Error("an old key file was kept")
	}
}

// up refuses to run over a box that is up, and then keeps its key file.
func TestUpOnRunningBoxKeepsKey(t *testing.T) {
	f := newFake()
	f.containers["ezbx-t-gateway"], f.containers["ezbx-t-box"] = true, true
	p := testPlan(t)
	p.KeyEnv = "ENGINE_KEY"
	os.MkdirAll(p.StateDir, 0o700)
	os.WriteFile(p.KeyFile(), []byte("in-use"), 0o600)
	err := keyRunner(f, map[string]string{"ENGINE_KEY": testKey}).Up(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "already up") {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p.KeyFile()); string(data) != "in-use" {
		t.Errorf("the running gateway's key was changed: %q", data)
	}
}
