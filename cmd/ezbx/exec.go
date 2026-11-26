package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
)

// maxCaptured is how much of each of stdout and stderr ezbx exec --json
// keeps. The rest is read and dropped, and the result says it was cut.
const maxCaptured = 1 << 20

// killGrace is how long a command gets after its timeout to end by itself,
// before it is killed.
const killGrace = 5 * time.Second

// execOptions is one ezbx exec call.
type execOptions struct {
	cwd     string
	env     []string // KEY=VALUE
	timeout time.Duration
	command []string
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkExecEnv refuses variables that would change how the box reaches the
// network or who the agent is, and names that look like secrets: keys stay
// with the gateway and never enter the box.
func checkExecEnv(kv string) error {
	k, _, ok := strings.Cut(kv, "=")
	if !ok || !envKeyRE.MatchString(k) {
		return usageError(fmt.Sprintf("--env %q: use NAME=VALUE", kv))
	}
	u := strings.ToUpper(k)
	switch {
	case strings.HasSuffix(u, "_PROXY") || u == "HOME" || u == "PATH" || u == "USER":
		return usageError(fmt.Sprintf("--env %s: ezbx sets this one itself, so the box keeps its network and user", k))
	case strings.Contains(u, "KEY") || strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD"):
		return usageError(fmt.Sprintf("--env %s: this looks like a secret, and secrets don't go into the box. A model server's key goes under [engine] key_env in ezbx.toml, for the gateway", k))
	}
	return nil
}

// execArgs builds the docker exec command. It runs as the box's own user,
// the agent, without a terminal. With a timeout, coreutils' timeout runs the
// command inside the box, so the command really ends there: stopping docker
// exec on the host would leave it running in the box.
func execArgs(boxName string, o execOptions) ([]string, error) {
	if len(o.command) == 0 {
		return nil, usageError("Give the command after --, such as: ezbx exec -- npm test")
	}
	cwd := o.cwd
	if cwd == "" {
		cwd = "/workspace"
	}
	if !path.IsAbs(cwd) || path.Clean(cwd) != cwd || strings.ContainsAny(cwd, "\x00\n") {
		return nil, usageError(fmt.Sprintf("--cwd %q: use a clean full path inside the box, such as /workspace/app", o.cwd))
	}
	if o.timeout < 0 {
		return nil, usageError("--timeout can't be negative")
	}
	a := []string{"exec", "-i", "-w", cwd}
	for _, kv := range o.env {
		if err := checkExecEnv(kv); err != nil {
			return nil, err
		}
		a = append(a, "-e", kv)
	}
	a = append(a, box.NamesFor(boxName).Box)
	if o.timeout > 0 {
		a = append(a, "timeout", "--kill-after="+seconds(killGrace), seconds(o.timeout))
	}
	return append(a, o.command...), nil
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// timedOut tells timeout's own exit codes (124 when it stopped the command,
// 137 when it had to kill it) from the command's, which could be the same
// numbers, by also asking whether the time was up.
func timedOut(o execOptions, exitCode int, took time.Duration) bool {
	return o.timeout > 0 && (exitCode == 124 || exitCode == 137) && took >= o.timeout
}

// capped keeps the first max bytes written to it and counts the rest.
type capped struct {
	buf     bytes.Buffer
	max     int
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	n := min(max(c.max-c.buf.Len(), 0), len(p))
	c.buf.Write(p[:n])
	c.dropped += int64(len(p) - n)
	return len(p), nil
}

// execResult is the data of ezbx exec --json.
type execResult struct {
	ExitCode        int    `json:"exitCode"`
	DurationMs      int64  `json:"durationMs"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool   `json:"stderrTruncated,omitempty"`
	TimedOut        bool   `json:"timedOut,omitempty"`
}

// runDocker runs a docker command with the given streams and returns its exit
// code. An error means docker itself couldn't run. Tests replace it.
var runDocker = func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// boxRunning is swappable in tests.
var boxRunning = func(ctx context.Context, name string) (bool, error) {
	return (&box.Runner{Docker: dockerIn}).Running(ctx, box.NamesFor(name).Box)
}

// runExec runs one command in a running box, as the agent, without a
// terminal, for programs that drive ezbx. Without --json it streams the
// command's output and exits with its exit code; with --json it prints one
// result object (the command's exit code is in it) and exits 0, or non-zero
// with an error when ezbx couldn't run the command or it timed out.
func runExec(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; needed when ezbx.toml has several")
	asJSON := fs.Bool("json", false, "print one JSON result with the exit code and the output")
	snap := fs.Bool("snapshot", false, "take a workspace snapshot first, like ezbx shell does")
	var o execOptions
	fs.StringVar(&o.cwd, "cwd", "/workspace", "folder inside the box to run in")
	fs.DurationVar(&o.timeout, "timeout", 0, "end the command after this long, such as 10m (0: no limit)")
	fs.Func("env", "NAME=VALUE for the command (repeatable; never a secret)", func(s string) error {
		o.env = append(o.env, s)
		return nil
	})
	words, rest, err := parseInterleaved(fs, args)
	fail := func(boxName string, err error) int {
		if *asJSON {
			return writeResult(stdout, boxName, nil, err)
		}
		return report(stderr, err)
	}
	if err != nil {
		return 2
	}
	if len(words) > 0 {
		return fail(*boxFlag, usageError("Put the command after --: ezbx exec [--box NAME] [--json] [--cwd DIR] [--timeout 10m] [--env NAME=VALUE]... [--snapshot] -- COMMAND [ARGS...]"))
	}
	o.command = rest
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return fail(*boxFlag, err)
	}
	argv, err := execArgs(t.name, o)
	if err != nil {
		return fail(t.name, err)
	}
	if t.cfg != nil && t.box() == nil {
		return fail(t.name, errNoBox(t.name, t.cfg.Names()))
	}

	ctx := context.Background()
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	up, err := boxRunning(check, t.name)
	cancel()
	if err != nil {
		return fail(t.name, err)
	}
	if !up {
		return fail(t.name, box.ErrNotUp{Box: t.name, What: "the box container isn't running"})
	}
	if err := beforeSession(t, !*snap, "leave out --snapshot", stderr); err != nil {
		return fail(t.name, err)
	}

	// The host side gives up a little after the box side would have.
	if o.timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, o.timeout+killGrace+30*time.Second)
		defer stop()
	}
	if !*asJSON {
		code, took, err := execWithEvents(ctx, t, o, argv, stdout, stderr, stderr)
		if err != nil {
			return report(stderr, err)
		}
		if timedOut(o, code, took) {
			fmt.Fprintf(stderr, "ezbx exec: the command was stopped after %s (--timeout).\n", o.timeout)
		}
		return code
	}
	out, errOut := &capped{max: maxCaptured}, &capped{max: maxCaptured}
	code, took, err := execWithEvents(ctx, t, o, argv, out, errOut, stderr)
	if err != nil {
		return fail(t.name, err)
	}
	res := execResult{
		ExitCode: code, DurationMs: took.Milliseconds(),
		Stdout: out.buf.String(), Stderr: errOut.buf.String(),
		StdoutTruncated: out.dropped > 0, StderrTruncated: errOut.dropped > 0,
		TimedOut: timedOut(o, code, took),
	}
	if res.TimedOut {
		return writeResultData(stdout, t.name, res, box.CodedError{Code: codeTimeout, Err: fmt.Errorf("the command was stopped after %s (--timeout)", o.timeout)})
	}
	return writeResult(stdout, t.name, res, nil)
}
