package main

// The runtime contract for programs that call ezbx (docs/runtime-contract.md):
// with --json, a command prints one JSON object on standard output and
// nothing else there, and a failure carries a machine-readable code.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
)

// contractVersion goes up with any breaking change to the flags, JSON fields
// or error codes of the contract.
const contractVersion = 1

// Error codes ezbx itself gives, besides the ones in internal/box.
const (
	codeInvalidArgs = "invalid_args"
	codeBoxNotFound = "box_not_found"
	codeTimeout     = "timeout"
	codeInternal    = "internal_error"
)

// features are the contract features this ezbx has. A caller checks for the
// ones it needs (ezbx version --json).
var features = []string{"net-four-modes", "readonly-masks", "status-v1"}

// envelope is the one object a --json command prints.
type envelope struct {
	OK              bool           `json:"ok"`
	Box             string         `json:"box,omitempty"`
	ContractVersion int            `json:"contractVersion"`
	Data            any            `json:"data,omitempty"`
	Error           *contractError `json:"error,omitempty"`
}

type contractError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// codeOf is the contract code for an error.
func codeOf(err error) string {
	var u usageError
	switch {
	case errors.As(err, &u):
		return codeInvalidArgs
	case box.CodeOf(err) != "":
		return box.CodeOf(err)
	case errors.Is(err, context.DeadlineExceeded):
		return codeTimeout
	}
	return codeInternal
}

// writeResult prints the envelope for a result or an error and returns the
// exit code: 0, 2 for a usage mistake, else 1.
func writeResult(stdout io.Writer, boxName string, data any, err error) int {
	if err != nil {
		data = nil
	}
	return writeResultData(stdout, boxName, data, err)
}

// writeResultData is writeResult that keeps the data even with an error,
// for a result that is only partly there, such as the output of a command
// that timed out.
func writeResultData(stdout io.Writer, boxName string, data any, err error) int {
	env := envelope{OK: err == nil, Box: boxName, ContractVersion: contractVersion, Data: data}
	code := 0
	if err != nil {
		env.Error = &contractError{Code: codeOf(err), Message: err.Error()}
		code = 1
		if env.Error.Code == codeInvalidArgs {
			code = 2
		}
	}
	out, _ := json.Marshal(env)
	fmt.Fprintln(stdout, string(out))
	return code
}

// errNoBox says the config has no box by that name.
func errNoBox(name string, have []string) error {
	return box.CodedError{Code: codeBoxNotFound, Err: fmt.Errorf("box %q isn't in ezbx.toml (it has %s). Add it with ezbx init --box %s, or pick another with --box", name, strings.Join(have, ", "), name)}
}

// plainVersion is ezbx's version without the leading v, as in 0.4.0.
func plainVersion() string { return strings.TrimPrefix(resolveVersion(), "v") }

// runVersion prints the version, or with --json the handshake a caller
// checks before it uses any other command.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the version, contract version and features as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx version [--json]")
		return 2
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if !*asJSON {
		fmt.Fprintf(stdout, "ezbx %s (%s)\n", resolveVersion(), platform)
		return 0
	}
	out, _ := json.Marshal(map[string]any{
		"ok": true, "version": plainVersion(), "contractVersion": contractVersion,
		"features": features, "platform": platform,
	})
	fmt.Fprintln(stdout, string(out))
	return 0
}

// boxState is what ezbx status reports.
type boxState struct {
	State          string   `json:"state"` // missing, stopped, running or unhealthy
	Workspace      string   `json:"workspace,omitempty"`
	NetworkMode    string   `json:"networkMode,omitempty"` // the running gateway's, else ezbx.toml's
	ConfigMode     string   `json:"configNetworkMode,omitempty"`
	Allow          []string `json:"allow"`
	GatewayHealthy bool     `json:"gatewayHealthy"`
	ImageTag       string   `json:"imageTag,omitempty"`     // the running box's image, else the one ezbx.toml needs
	ImageCurrent   *bool    `json:"imageCurrent,omitempty"` // the running box's image is the one ezbx.toml needs
	EzbxVersion    string   `json:"ezbxVersion"`
	Problems       []string `json:"problems,omitempty"` // why it is unhealthy
}

// statusOf works out a box's state from Docker and ezbx.toml. It only reads.
func statusOf(ctx context.Context, r *box.Runner, t target) (boxState, error) {
	s := boxState{EzbxVersion: plainVersion(), Allow: []string{}}
	b := t.box()
	if b != nil {
		s.ConfigMode, s.NetworkMode = b.Network, b.Network
		s.Allow = append(s.Allow, b.Allow...)
		if ws, err := workspaceOf(b); err == nil {
			s.Workspace = ws
		}
	}
	want, _ := expectedImages(t)
	if img, ok := want["box"]; ok {
		s.ImageTag = img.Tag
	}
	if err := r.CheckDocker(ctx); err != nil {
		return s, err
	}
	n := box.NamesFor(t.name)
	gw, err := r.Inspect(ctx, n.Gateway)
	if err != nil {
		return s, err
	}
	bx, err := r.Inspect(ctx, n.Box)
	if err != nil {
		return s, err
	}
	s.GatewayHealthy = gw.Running && gw.Health == "healthy"
	switch {
	case !gw.Exists && !bx.Exists && b == nil:
		s.State = "missing"
		return s, nil
	case !gw.Exists && !bx.Exists:
		s.State = "stopped"
		return s, nil
	}
	if bx.Exists {
		s.ImageTag = bx.Image
		if img, ok := want["box"]; ok {
			current := bx.Image == img.Tag
			s.ImageCurrent = &current
		}
	}
	if !gw.Running {
		s.Problems = append(s.Problems, "the gateway isn't running")
	} else if gw.Health != "healthy" {
		s.Problems = append(s.Problems, "the gateway's healthcheck says "+orUnknown(gw.Health))
	}
	if !bx.Running {
		s.Problems = append(s.Problems, "the box container isn't running")
	}
	if gw.Running {
		st, err := r.GatewayStatus(ctx, t.name)
		if err != nil {
			s.Problems = append(s.Problems, "the gateway's admin API didn't answer: "+err.Error())
		} else {
			s.NetworkMode, s.Allow = st.Network.Mode, append([]string{}, st.Network.Allow...)
		}
	}
	s.State = "running"
	if len(s.Problems) > 0 {
		s.State = "unhealthy"
	}
	return s, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "nothing (no healthcheck)"
	}
	return s
}

// runStatus reports a box's state: missing (not in ezbx.toml and nothing
// made), stopped, running or unhealthy.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; needed when ezbx.toml has several")
	asJSON := fs.Bool("json", false, "print the result as one JSON object")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 0 || len(rest) > 0 {
		err = usageError("Usage: ezbx status [--box NAME] [--config FILE] [--json]")
		if *asJSON {
			return writeResult(stdout, *boxFlag, nil, err)
		}
		return report(stderr, err)
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		if *asJSON {
			return writeResult(stdout, *boxFlag, nil, err)
		}
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := statusOf(ctx, &box.Runner{Docker: dockerIn}, t)
	if *asJSON {
		return writeResult(stdout, t.name, s, err)
	}
	if err != nil {
		return report(stderr, err)
	}
	printStatus(stdout, t, s)
	return 0
}

func printStatus(w io.Writer, t target, s boxState) {
	fmt.Fprintf(w, "Box %q: %s\n", t.name, s.State)
	if s.Workspace != "" {
		fmt.Fprintln(w, "  workspace ", s.Workspace)
	}
	if s.NetworkMode != "" {
		mode := s.NetworkMode
		if s.ConfigMode != "" && s.ConfigMode != s.NetworkMode {
			mode += " (ezbx.toml says " + s.ConfigMode + ")"
		}
		fmt.Fprintln(w, "  network   ", mode)
	}
	if s.ImageTag != "" {
		img := s.ImageTag
		if s.ImageCurrent != nil && !*s.ImageCurrent {
			img += " (not the image ezbx.toml needs now; ezbx build, then ezbx down and ezbx up)"
		}
		fmt.Fprintln(w, "  image     ", img)
	}
	for _, p := range s.Problems {
		fmt.Fprintln(w, "  problem:  ", p)
	}
	if s.State == "missing" {
		fmt.Fprintf(w, "ezbx.toml has no box %q and nothing of it exists. Add it with ezbx init --box %s.\n", t.name, t.name)
	}
}
