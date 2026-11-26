package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/catalog"
	"github.com/OranjSvensk/ezbx/harnesses"
	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/disk"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/images"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/setup"
	"github.com/OranjSvensk/ezbx/internal/tools"
)

// stdin is where init reads answers. Tests replace it.
var stdin io.Reader = os.Stdin

// runInit is the guided setup: it asks the questions, shows the plan, and
// only then writes ezbx.toml and starts the box. See docs/init-flow.md.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file to write (and to start from, if it exists)")
	plan := fs.Bool("plan", false, "show what would be done, without asking or changing anything")
	yes := fs.Bool("yes", false, "take every default without asking")
	noStart := fs.Bool("no-start", false, "write the config, but don't start the box")
	listProfiles := fs.Bool("list-profiles", false, "list starter profiles and exit")
	var o setup.Options
	fs.StringVar(&o.Box, "box", "", "the box to set up: one in ezbx.toml, or a new one to add beside the others")
	fs.StringVar(&o.Profile, "profile", "", "starter profile: offline, open, python-ml, research or web-dev")
	fs.StringVar(&o.URL, "url", "", "the model server's address, such as http://127.0.0.1:1234, if it isn't found")
	fs.StringVar(&o.API, "api", "", "the server's API kind: auto, openai, lmstudio or anthropic")
	fs.BoolVar(&o.Later, "later", false, "no model server yet: write the config without one")
	fs.StringVar(&o.KeyEnv, "key-env", "", "NAME of an environment variable holding the server's key")
	fs.BoolVar(&o.Reasoning, "reasoning", false, "mark the server's models as reasoning models")
	harnessList := fs.String("harness", "", "harnesses, separated by commas: pi, opencode, omp, codex, claude, hermes, openclaw")
	toolList := fs.String("tools", "", "tool packs and tools, separated by commas, such as pack:coding,ffmpeg, or none")
	fs.StringVar(&o.Workspace, "workspace", "", "the workspace folder")
	fs.Func("readonly", "a read-only folder (give it again for more; --readonly none for none)", func(s string) error {
		if o.ReadOnly == nil {
			o.ReadOnly = []string{}
		}
		if !strings.EqualFold(s, "none") {
			o.ReadOnly = append(o.ReadOnly, s)
		}
		return nil
	})
	fs.StringVar(&o.Network, "network", "", "airgap, off, allow or on")
	fs.StringVar(&o.Memory, "memory", "", "memory limit, such as 8g")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx init [--plan] [--yes] [--no-start] [--box NAME] [--config FILE] [--profile NAME] [--list-profiles] [--url ADDRESS] [--api auto|openai|lmstudio|anthropic] [--key-env NAME] [--later] [--harness pi,opencode,omp,codex,claude,hermes,openclaw] [--tools pack:coding] [--workspace PATH] [--readonly PATH]... [--network airgap|off|allow|on] [--memory 8g]")
		return 2
	}
	if *listProfiles {
		if o.Profile != "" || o.URL != "" || o.Later || *plan || *yes || *noStart {
			return report(stderr, usageError("--list-profiles cannot be combined with setup flags"))
		}
		cat, err := tools.Load(catalog.FS)
		if err != nil {
			return report(stderr, fmt.Errorf("internal error loading the tool catalogue: %w", err))
		}
		fmt.Fprint(stdout, setup.FormatProfiles(cat))
		return 0
	}
	if o.Profile != "" {
		cat, err := tools.Load(catalog.FS)
		if err != nil {
			return report(stderr, fmt.Errorf("internal error loading the tool catalogue: %w", err))
		}
		if _, ok := cat.Profiles[o.Profile]; !ok {
			return report(stderr, usageError(fmt.Sprintf("unknown profile %q (available: %s)", o.Profile, strings.Join(cat.ProfileIDs(), ", "))))
		}
	}
	if *harnessList != "" {
		for _, h := range strings.Split(*harnessList, ",") {
			o.Harnesses = append(o.Harnesses, strings.TrimSpace(h))
		}
	}
	if *toolList != "" {
		o.Tools = []string{}
		if !strings.EqualFold(*toolList, "none") {
			for _, t := range strings.Split(*toolList, ",") {
				if t = strings.TrimSpace(t); t != "" {
					o.Tools = append(o.Tools, t)
				}
			}
		}
	}
	if o.URL != "" && !strings.HasPrefix(o.URL, "http://") && !strings.HasPrefix(o.URL, "https://") {
		return report(stderr, usageError("--url must start with http:// or https://"))
	}
	if o.Later && o.URL != "" {
		return report(stderr, usageError("--later and --url can't go together: --later is for when there is no server yet"))
	}
	if o.KeyEnv != "" && !envName(o.KeyEnv) {
		return report(stderr, usageError("--key-env is the NAME of an environment variable, such as ENGINE_API_KEY, never the key itself"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout, Auto: *yes || *plan}

	// 0. Preflight.
	showBanner(stdout)
	fmt.Fprintln(stdout, "ezbx setup")
	if err := preflight(ctx, stdout); err != nil {
		return report(stderr, err)
	}
	dirs, err := paths.Default()
	if err != nil {
		return report(stderr, err)
	}
	cfgFile := dirs.ConfigFile(*cfgFlag, os.Getenv)
	var existing *config.Config
	if _, err := os.Stat(cfgFile); err == nil {
		if existing, err = config.Load(cfgFile); err != nil {
			return report(stderr, fmt.Errorf("%w\nThe existing config can't be read, so init won't replace it. Fix it, or move it away, then run ezbx init again", err))
		}
		fmt.Fprintln(stdout, "Starting from the existing config:", cfgFile)
	}

	facts, err := gatherFacts(ctx, existing, o, dirs)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout)
	res, err := setup.Gather(facts, o, p)
	if err != nil {
		return report(stderr, err)
	}

	text := setup.ConfigText(res.Answers, time.Now())
	if _, err := config.Parse(text); err != nil {
		return report(stderr, fmt.Errorf("internal error: the config init would write doesn't load: %w", err))
	}
	gw, bx, err := imageSpecs(res.Answers.Tools, res.Answers.Harnesses)
	if err != nil {
		return report(stderr, err)
	}
	pl := setup.Plan{
		Result: res, ConfigFile: cfgFile, StateDir: dirs.Box(res.Answers.Box),
		Images: imagesPresent(ctx, gw, bx), Tools: bx.Tools.Chosen, Harnesses: facts.Harnesses, NoStart: *noStart,
	}
	newBox := true
	if existing != nil {
		old, found := setup.FromConfigBox(existing, res.Answers.Box)
		newBox = !found
		pl.Old = &old
	}
	pl.BoxUp = boxExists(ctx, res.Answers.Box)
	if res.Engine != nil {
		if k := res.Answers.Engine.KeyEnv; k != "" {
			pl.KeyEnvSet = os.Getenv(k) != ""
		}
	}
	if st, err := os.Stat(res.Answers.Workspace); err == nil && st.IsDir() {
		pl.Workspace = true
	}
	pl.Disks = diskChecks(dirs.State, dockerDataDir())
	fmt.Fprint(stdout, pl.Review())

	if *plan {
		fmt.Fprintln(stdout, "\nThis was a plan only (--plan): nothing was changed.")
		return 0
	}
	start, why := pl.Start()
	if !pl.Changed() && !start {
		fmt.Fprintf(stdout, "\nNothing to change (%s).\n", why)
		return 0
	}
	if !*yes {
		fmt.Fprintln(stdout)
		ok, err := p.YesNo("Continue?", false)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "Nothing was changed.")
			return 0
		}
	}

	// 7. Apply.
	if pl.Changed() {
		if err := writeConfig(cfgFile, text); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "wrote", cfgFile)
	}
	if err := os.MkdirAll(res.Answers.Workspace, 0o700); err != nil {
		return report(stderr, fmt.Errorf("creating the workspace: %w", err))
	}
	if newBox && pl.Changed() {
		recordEvent(target{dirs: dirs, name: res.Answers.Box}, "box.created", map[string]any{"networkMode": res.Answers.Network}, 0, stderr)
	}
	if !start {
		fmt.Fprintf(stdout, "\nThe box was not started: %s.\n", why)
		return 0
	}
	if len(pl.MissingImages()) > 0 {
		fmt.Fprintln(stdout)
		if err := buildImages(ctx, dirs, stdout, gw, bx); err != nil {
			return report(stderr, fmt.Errorf("%w\nThe config is written. Fix what is shown above, then run ezbx build and ezbx up (or ezbx init again)", err))
		}
	}
	fmt.Fprintln(stdout)
	if code := runUp([]string{"--config", cfgFile, "--box", res.Answers.Box}, stdout, stderr); code != 0 {
		fmt.Fprintln(stderr, "The config is written. Fix what is shown above, then run ezbx up (or ezbx init again).")
		return code
	}
	if len(res.Answers.Harnesses) > 0 {
		fmt.Fprintf(stdout, "\nNext: ezbx shell %s   (or ezbx shell for a terminal in the box)\n", res.Answers.Harnesses[0])
	}
	return 0
}

// diskChecks measures the drives init fills: the state folder (snapshots and
// backups) and Docker's own data folder, when it is in its usual place. Two
// on the same Windows drive are shown once.
func diskChecks(state, docker string) []setup.Disk {
	places := [][2]string{{"the state folder (snapshots)", state}}
	if docker != "" {
		places = append(places, [2]string{"Docker's images", docker})
	}
	var out []setup.Disk
	for _, p := range places {
		vol := strings.ToLower(filepath.VolumeName(p[1]))
		merged := false
		for i := range out {
			if vol != "" && strings.ToLower(filepath.VolumeName(out[i].Path)) == vol {
				out[i].What += " and " + p[0]
				merged = true
			}
		}
		if !merged {
			n, err := disk.Free(p[1])
			out = append(out, setup.Disk{What: p[0], Path: p[1], Free: n, Err: err})
		}
	}
	return out
}

// dockerDataDir is where Docker keeps images on this computer, in its usual
// place, or "" when it isn't there (Docker Desktop lets you move it).
func dockerDataDir() string {
	var dir string
	switch runtime.GOOS {
	case "windows":
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			dir = filepath.Join(l, "Docker")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "Containers", "com.docker.docker")
		}
	case "linux":
		dir = "/var/lib/docker"
	}
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// preflight checks the system and waits for Docker if it isn't running.
func preflight(ctx context.Context, w io.Writer) error {
	ok := map[string]bool{"windows": true, "linux": true, "darwin": true}
	if !ok[runtime.GOOS] || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("ezbx doesn't support %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("the docker command isn't installed. Install Docker Desktop (https://docs.docker.com/desktop/), then run ezbx init again. ezbx doesn't install it for you")
	}
	r := &box.Runner{Docker: dockerIn}
	err := r.CheckDocker(ctx)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "too old") {
		return err
	}
	// Never start Docker: say so and wait (decided in docs/init-flow.md).
	fmt.Fprintln(w, "Docker isn't answering. Start Docker Desktop; this waits up to 5 minutes (Ctrl+C to stop).")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		if err = r.CheckDocker(ctx); err == nil {
			fmt.Fprintln(w, "Docker is answering.")
			return nil
		}
		if strings.Contains(err.Error(), "too old") {
			return err
		}
	}
	return err
}

// gatherFacts finds everything the questions need.
func gatherFacts(ctx context.Context, existing *config.Config, o setup.Options, dirs paths.Dirs) (setup.Facts, error) {
	f, err := baseFacts(ctx, existing, dirs)
	if err != nil {
		return f, err
	}

	// The scan offers every kind; --api only narrows what is listed.
	adapters, err := apiKinds(config.DefaultAPI)
	if err != nil {
		return f, err
	}
	if o.API != "" {
		if _, err := apiKinds(o.API); err != nil {
			return f, usageError("--api" + strings.TrimPrefix(err.Error(), "engine.api"))
		}
	}
	var urls []string
	keys := map[string]string{} // each key goes only to its server's url
	if o.URL != "" {
		urls = append(urls, o.URL)
		if o.KeyEnv != "" {
			keys[o.URL] = os.Getenv(o.KeyEnv)
		}
	}
	if existing != nil && existing.Engine.URL != "" {
		u := existing.Engine.URL
		urls = append(urls, u)
		if _, given := keys[u]; !given && existing.Engine.KeyEnv != "" {
			keys[u] = os.Getenv(existing.Engine.KeyEnv)
		}
	}
	sort.Strings(urls)
	res := (&detect.Detector{Adapters: adapters, Keys: keys}).Detect(ctx, urls)
	f.Engines = res.Engines
	f.Refused = res.Refused
	return f, nil
}

// baseFacts is everything the questions need except the model servers, so
// ezbx create can use it without contacting any.
func baseFacts(ctx context.Context, existing *config.Config, dirs paths.Dirs) (setup.Facts, error) {
	home, _ := os.UserHomeDir()
	f := setup.Facts{Home: home, Own: []string{dirs.Config, dirs.State}, System: systemDirs(), Existing: existing, DockerMemory: dockerMemory(ctx)}

	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return f, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	for _, m := range all {
		if m.Verified.Status == "verified" {
			f.Harnesses = append(f.Harnesses, m)
		}
	}
	sort.SliceStable(f.Harnesses, func(i, j int) bool {
		return f.Harnesses[i].Kind == "coding" && f.Harnesses[j].Kind != "coding"
	})
	if f.Catalog, err = tools.Load(catalog.FS); err != nil {
		return f, fmt.Errorf("internal error loading the tool catalogue: %w", err)
	}

	return f, nil
}

// imagesPresent reports whether each image is built from these inputs.
// This only asks the local Docker engine, so --plan stays off the network.
func imagesPresent(ctx context.Context, specs ...images.Spec) []setup.Image {
	ir := imageRunner(io.Discard)
	var out []setup.Image
	for _, s := range specs {
		out = append(out, setup.Image{Tag: s.Tag(), Present: ir.Check(ctx, s) == nil})
	}
	return out
}

// boxExists reports whether either of the box's containers exists.
func boxExists(ctx context.Context, name string) bool {
	n := box.NamesFor(name)
	for _, c := range []string{n.Gateway, n.Box} {
		if out, err := dockerIn(ctx, nil, "container", "ls", "-a", "--filter", "name="+c, "--format", "{{.Names}}"); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(line) == c {
					return true
				}
			}
		}
	}
	return false
}

// writeConfig writes the config file, readable only by this user. An
// existing file is kept as FILE.previous first, so nothing is lost.
func writeConfig(file, text string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(file); err == nil {
		if err := os.WriteFile(file+".previous", old, 0o600); err != nil {
			return fmt.Errorf("keeping the current config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func envName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
