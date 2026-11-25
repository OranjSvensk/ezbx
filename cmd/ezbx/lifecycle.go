package main

// ezbx create, start and stop: the contract's names for adding a box to
// ezbx.toml without asking anything, ezbx up and ezbx down
// (docs/runtime-contract.md).

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/setup"
)

// codeBoxExists: ezbx create was asked for a box ezbx.toml already has.
const codeBoxExists = "box_exists"

// listFlag collects a flag given more than once, or a comma-separated list.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// createOptions are ezbx create's flags.
type createOptions struct {
	box, workspace, network, memory, profile string
	url, api, keyEnv                         string
	allow, readonly, harnesses, tools        listFlag
	toolsSet                                 bool
}

// createAnswers works out the new box's settings the way ezbx init does,
// taking every default and asking nothing, and without contacting a model
// server, Docker's images or the internet. The engine settings in ezbx.toml
// are shared by every box, so a new address is only taken for a new file.
func createAnswers(f setup.Facts, c createOptions) (setup.Result, error) {
	switch {
	case c.box == "":
		return setup.Result{}, usageError("ezbx create needs --box NAME")
	case strings.TrimSpace(c.workspace) == "":
		return setup.Result{}, usageError("ezbx create needs --workspace FOLDER: the one folder the agent can change")
	case len(c.allow) > 0 && c.network != "allow":
		return setup.Result{}, usageError("--allow needs --network allow")
	}
	if e := f.Existing; e != nil {
		if _, ok := e.Get(c.box); ok {
			return setup.Result{}, box.CodedError{Code: codeBoxExists, Err: fmt.Errorf("ezbx.toml already has box %q. Change it with ezbx init --box %s, or remove it with ezbx destroy --box %s --remove-config", c.box, c.box, c.box)}
		}
		if (c.url != "" && strings.TrimRight(c.url, "/") != strings.TrimRight(e.Engine.URL, "/")) || c.api != "" || c.keyEnv != "" {
			return setup.Result{}, usageError("--url, --api and --key-env set [engine], which every box in ezbx.toml shares. Change it with ezbx init or ezbx engine use instead")
		}
	}
	o := setup.Options{
		Box: c.box, Profile: c.profile, URL: c.url, API: c.api, KeyEnv: c.keyEnv,
		Workspace: c.workspace, Network: c.network, Memory: c.memory,
		Harnesses: c.harnesses, ReadOnly: []string(c.readonly),
	}
	if c.toolsSet {
		o.Tools = []string(c.tools)
		if o.Tools == nil {
			o.Tools = []string{}
		}
	}
	if c.network == "allow" {
		o.Allow = []string(c.allow)
		if o.Allow == nil {
			o.Allow = []string{}
		}
	}
	p := &setup.Prompter{In: bufio.NewReader(strings.NewReader("")), Out: io.Discard, Auto: true}
	return setup.Gather(f, o, p)
}

// runCreate adds a box to ezbx.toml (making the file if needed) without
// asking anything. Nothing is built, started or downloaded.
func runCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file to add the box to")
	asJSON := fs.Bool("json", false, "print one JSON result")
	var c createOptions
	fs.StringVar(&c.box, "box", "", "the new box's name")
	fs.StringVar(&c.workspace, "workspace", "", "the one folder the agent can change (made if missing)")
	fs.StringVar(&c.network, "network", "", "airgap, off, allow or on (default: the profile's, else off)")
	fs.Var(&c.allow, "allow", "a host for allow mode (repeatable, or comma-separated)")
	fs.Var(&c.readonly, "readonly", "a folder the agent may read, at /readonly/<its name> (repeatable)")
	fs.Var(&c.harnesses, "harness", "harnesses, such as pi,opencode (default: the profile's)")
	fs.Func("tools", "tool packs and tools, such as pack:coding,ffmpeg, or none", func(s string) error {
		c.toolsSet = true
		if !strings.EqualFold(strings.TrimSpace(s), "none") {
			return c.tools.Set(s)
		}
		return nil
	})
	fs.StringVar(&c.profile, "profile", "", "starter profile for the defaults (default: offline)")
	fs.StringVar(&c.memory, "memory", "", "memory limit, such as 8g")
	fs.StringVar(&c.url, "url", "", "the model server's address (only for a new ezbx.toml)")
	fs.StringVar(&c.api, "api", "", "the server's API kind (only for a new ezbx.toml)")
	fs.StringVar(&c.keyEnv, "key-env", "", "NAME of the variable with the server's key (only for a new ezbx.toml)")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	fail := func(err error) int {
		if *asJSON {
			return writeResult(stdout, c.box, nil, err)
		}
		return report(stderr, err)
	}
	if len(words) > 0 || len(rest) > 0 {
		return fail(usageError("Usage: ezbx create --box NAME --workspace FOLDER [--network MODE] [--allow HOST]... [--readonly FOLDER]... [--harness LIST] [--tools LIST] [--profile NAME] [--memory 8g] [--json]"))
	}
	dirs, err := paths.Default()
	if err != nil {
		return fail(err)
	}
	cfgFile := dirs.ConfigFile(*cfgFlag, os.Getenv)
	var existing *config.Config
	if _, err := os.Stat(cfgFile); err == nil {
		if existing, err = config.Load(cfgFile); err != nil {
			return fail(fmt.Errorf("%w\nThe existing config can't be read, so ezbx create won't change it", err))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := baseFacts(ctx, existing, dirs)
	if err != nil {
		return fail(err)
	}
	res, err := createAnswers(f, c)
	if err != nil {
		if !errors.As(err, new(box.CodedError)) && !errors.As(err, new(usageError)) {
			err = usageError(err.Error()) // a refused answer, such as a workspace that isn't allowed
		}
		return fail(err)
	}
	text := setup.ConfigText(res.Answers, time.Now())
	if _, err := config.Parse(text); err != nil {
		return fail(fmt.Errorf("internal error: the config ezbx create would write doesn't load: %w", err))
	}
	if err := writeConfig(cfgFile, text); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(res.Answers.Workspace, 0o700); err != nil {
		return fail(fmt.Errorf("creating the workspace: %w", err))
	}
	a := res.Answers
	if *asJSON {
		return writeResult(stdout, a.Box, map[string]any{
			"configFile": cfgFile, "workspace": a.Workspace, "network": a.Network, "allow": nonNil(a.Allow),
			"harnesses": a.Harnesses, "tools": nonNil(a.Tools), "readonly": readOnlyPaths(a.ReadOnly),
		}, nil)
	}
	fmt.Fprintf(stdout, "Added box %q to %s (workspace %s, network %s, harnesses %s).\n", a.Box, cfgFile, a.Workspace, a.Network, strings.Join(a.Harnesses, ","))
	fmt.Fprintf(stdout, "Next: ezbx build --box %s, then ezbx start --box %s\n", a.Box, a.Box)
	return 0
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func readOnlyPaths(ro []config.ReadOnly) []string {
	out := []string{}
	for _, r := range ro {
		out = append(out, r.Path)
	}
	return out
}

// runStart is ezbx up for one box, without the doctor unless --doctor.
// With --json, the progress goes to stderr and the box's state to stdout.
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box to start; needed when ezbx.toml has several")
	doctorToo := fs.Bool("doctor", false, "run the doctor's checks after starting (up to a few minutes)")
	asJSON := fs.Bool("json", false, "print one JSON result with the box's state")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	out := stdout
	if *asJSON {
		out = stderr
	}
	fail := func(name string, err error) int {
		if *asJSON {
			return writeResult(stdout, name, nil, err)
		}
		return report(stderr, err)
	}
	if len(words) > 0 || len(rest) > 0 {
		return fail(*boxFlag, usageError("Usage: ezbx start [--box NAME] [--config FILE] [--doctor] [--json]"))
	}
	plan, err := startBox(*cfgFlag, *boxFlag, *doctorToo, out, stderr)
	name := plan.Box
	if name == "" {
		name = *boxFlag
	}
	if err != nil {
		return fail(name, err)
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "Box %q is up.\n", name)
		return 0
	}
	return reportState(stdout, *cfgFlag, name)
}

// runStop is ezbx down for one box.
func runStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box to stop; needed when ezbx.toml has several")
	asJSON := fs.Bool("json", false, "print one JSON result with the box's state")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	out := stdout
	if *asJSON {
		out = stderr
	}
	fail := func(name string, err error) int {
		if *asJSON {
			return writeResult(stdout, name, nil, err)
		}
		return report(stderr, err)
	}
	if len(words) > 0 || len(rest) > 0 {
		return fail(*boxFlag, usageError("Usage: ezbx stop [--box NAME] [--config FILE] [--json]"))
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return fail(*boxFlag, err)
	}
	if err := stopBox(t, out); err != nil {
		return fail(t.name, err)
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "Box %q is stopped. Kept: the workspace, the state folder and the volume %s.\n", t.name, box.NamesFor(t.name).Home)
		return 0
	}
	return reportState(stdout, *cfgFlag, t.name)
}

// reportState prints the box's state as the JSON result.
func reportState(stdout io.Writer, cfgFlag, name string) int {
	t, err := resolveBox(cfgFlag, name)
	if err != nil {
		return writeResult(stdout, name, nil, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := statusOf(ctx, &box.Runner{Docker: dockerIn}, t)
	return writeResult(stdout, name, s, err)
}
