package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecArgs(t *testing.T) {
	got, err := execArgs("t", execOptions{command: []string{"npm", "test"}})
	if err != nil || strings.Join(got, " ") != "exec -i -w /workspace ezbx-t-box npm test" {
		t.Errorf("plain: %q %v", got, err)
	}
	got, err = execArgs("agent-ada", execOptions{cwd: "/workspace/app", env: []string{"CI=1", "NODE_ENV=test"}, timeout: 90 * time.Second, command: []string{"make", "-j4"}})
	if err != nil || strings.Join(got, " ") != "exec -i -w /workspace/app -e CI=1 -e NODE_ENV=test ezbx-agent-ada-box timeout --kill-after=5s 90s make -j4" {
		t.Errorf("full: %q %v", got, err)
	}
	if got, _ := execArgs("t", execOptions{timeout: 1500 * time.Millisecond, command: []string{"x"}}); !strings.Contains(strings.Join(got, " "), "timeout --kill-after=5s 1.5s x") {
		t.Errorf("fractional timeout: %q", got)
	}
	bad := map[string]execOptions{
		"no command":    {},
		"relative cwd":  {cwd: "workspace", command: []string{"x"}},
		"unclean cwd":   {cwd: "/workspace/../etc", command: []string{"x"}},
		"trailing /":    {cwd: "/workspace/", command: []string{"x"}},
		"negative time": {timeout: -time.Second, command: []string{"x"}},
		"bad env":       {env: []string{"NOVALUE"}, command: []string{"x"}},
	}
	for name, o := range bad {
		if _, err := execArgs("t", o); err == nil {
			t.Errorf("%s: accepted", name)
		} else if _, ok := err.(usageError); !ok {
			t.Errorf("%s: %T is not a usage error", name, err)
		}
	}
}

func TestCheckExecEnv(t *testing.T) {
	for _, ok := range []string{"CI=1", "NODE_ENV=test", "EMPTY=", "A_B=x=y", "lower=1"} {
		if err := checkExecEnv(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"=x", "1A=x", "A-B=x", "NOEQUALS", "HTTP_PROXY=x", "https_proxy=x", "NO_PROXY=x", "ALL_PROXY=x", "HOME=/tmp",
		"PATH=/bin", "USER=root", "API_KEY=x", "OPENAI_API_KEY=x", "GITHUB_TOKEN=x", "Client_Secret=x", "DB_PASSWORD=x", "MONKEY=x"} {
		if err := checkExecEnv(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	for _, s := range []string{"ab", "cd", "efg", "h"} {
		if n, err := c.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q: %d %v", s, n, err)
		}
	}
	if c.buf.String() != "abcde" || c.dropped != 3 {
		t.Errorf("%q, dropped %d", c.buf.String(), c.dropped)
	}
	c = &capped{max: 4}
	c.Write([]byte("abcd"))
	if c.dropped != 0 {
		t.Errorf("exactly full dropped %d", c.dropped)
	}
}

func TestTimedOut(t *testing.T) {
	o := execOptions{timeout: time.Second}
	cases := []struct {
		o    execOptions
		code int
		took time.Duration
		want bool
	}{
		{o, 124, 2 * time.Second, true},
		{o, 137, 7 * time.Second, true},
		{o, 124, 100 * time.Millisecond, false}, // the command's own 124
		{o, 1, 2 * time.Second, false},
		{execOptions{}, 124, time.Hour, false}, // no timeout asked for
	}
	for _, tc := range cases {
		if got := timedOut(tc.o, tc.code, tc.took); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

// execEnv makes a config with box t and swaps out Docker.
func execEnv(t *testing.T, running bool, run func(argv []string, stdout, stderr io.Writer) int) *[]string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EZBX_HOME", home)
	t.Setenv("EZBX_CONFIG", "")
	ws := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(ws, 0o700)
	os.WriteFile(filepath.Join(home, "ezbx.toml"), []byte("version = 1\n[box.t]\nworkspace = '"+filepath.ToSlash(ws)+"'\nharnesses = ['pi']\n"), 0o600)
	var called []string
	oldUp, oldRun := boxRunning, runDocker
	t.Cleanup(func() { boxRunning, runDocker = oldUp, oldRun })
	boxRunning = func(context.Context, string) (bool, error) { return running, nil }
	runDocker = func(_ context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		called = argv
		return run(argv, stdout, stderr), nil
	}
	return &called
}

func decodeExec(t *testing.T, out []byte) (env struct {
	OK    bool
	Box   string
	Data  execResult
	Error struct{ Code, Message string }
}) {
	t.Helper()
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return env
}

func TestRunExecJSON(t *testing.T) {
	called := execEnv(t, true, func(argv []string, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "hello\n")
		io.WriteString(stderr, strings.Repeat("e", maxCaptured+10))
		return 3
	})
	var out, errOut bytes.Buffer
	code := run([]string{"exec", "--json", "--env", "CI=1", "--", "sh", "-c", "exit 3"}, &out, &errOut)
	env := decodeExec(t, out.Bytes())
	// The command's own exit code is in the result; ezbx succeeded.
	if code != 0 || !env.OK || env.Box != "t" || env.Data.ExitCode != 3 || env.Data.Stdout != "hello\n" || env.Data.StdoutTruncated ||
		len(env.Data.Stderr) != maxCaptured || !env.Data.StderrTruncated || env.Data.TimedOut {
		t.Errorf("exit %d: %+v / %s", code, env, errOut.String())
	}
	if strings.Join(*called, " ") != "exec -i -w /workspace -e CI=1 ezbx-t-box sh -c exit 3" {
		t.Errorf("docker %q", *called)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Error("more than one line on stdout")
	}
}

func TestRunExecPlain(t *testing.T) {
	execEnv(t, true, func(argv []string, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "streamed")
		return 7
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"exec", "--", "false"}, &out, &errOut); code != 7 || out.String() != "streamed" {
		t.Errorf("exit %d, %q / %q", code, out.String(), errOut.String())
	}
}

func TestRunExecTimeout(t *testing.T) {
	execEnv(t, true, func(argv []string, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "partial")
		time.Sleep(60 * time.Millisecond)
		return 124
	})
	var out, errOut bytes.Buffer
	code := run([]string{"exec", "--json", "--timeout", "50ms", "--", "sleep", "9"}, &out, &errOut)
	env := decodeExec(t, out.Bytes())
	if code != 1 || env.OK || env.Error.Code != "timeout" || !env.Data.TimedOut || env.Data.Stdout != "partial" {
		t.Errorf("exit %d: %+v", code, env)
	}
}

func TestRunExecRefusals(t *testing.T) {
	ran := false
	execEnv(t, false, func([]string, io.Writer, io.Writer) int { ran = true; return 0 })
	cases := []struct {
		args []string
		code string
		exit int
	}{
		{[]string{"exec", "--json", "--", "ls"}, "box_not_running", 1},
		{[]string{"exec", "--json", "ls"}, "invalid_args", 2},
		{[]string{"exec", "--json", "ls", "--", "pwd"}, "invalid_args", 2},
		{[]string{"exec", "--json"}, "invalid_args", 2},
		{[]string{"exec", "--json", "--env", "GITHUB_TOKEN=x", "--", "ls"}, "invalid_args", 2},
		{[]string{"exec", "--json", "--box", "nope", "--", "ls"}, "box_not_found", 1},
	}
	for _, tc := range cases {
		var out, errOut bytes.Buffer
		exit := run(tc.args, &out, &errOut)
		env := decodeExec(t, out.Bytes())
		if exit != tc.exit || env.OK || env.Error.Code != tc.code {
			t.Errorf("%v: exit %d, %+v", tc.args, exit, env)
		}
	}
	if ran {
		t.Error("a command ran in a box that isn't up")
	}
}

// exec takes a snapshot only when asked to.
func TestRunExecSnapshot(t *testing.T) {
	execEnv(t, true, func([]string, io.Writer, io.Writer) int { return 0 })
	count := func() int {
		tg, err := resolveBox("", "t")
		if err != nil {
			t.Fatal(err)
		}
		sb, err := openSnapshots(tg)
		if err != nil {
			t.Fatal(err)
		}
		list, err := sb.store.List()
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"exec", "--", "true"}, &out, &errOut); code != 0 || count() != 0 {
		t.Fatalf("without --snapshot: exit %d, %d snapshots (%s)", code, count(), errOut.String())
	}
	if code := run([]string{"exec", "--snapshot", "--", "true"}, &out, &errOut); code != 0 || count() != 1 {
		t.Fatalf("with --snapshot: exit %d, %d snapshots (%s)", code, count(), errOut.String())
	}
}
