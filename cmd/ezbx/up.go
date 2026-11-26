package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/harnesses"
	"github.com/OranjSvensk/ezbx/internal/adapter"
	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/gateway"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/setup"
	"github.com/OranjSvensk/ezbx/providers"
)

// runUp starts the box described in ezbx.toml, then runs the doctor.
func runUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file; default EZBX_CONFIG, then ezbx.toml in the config folder")
	boxFlag := fs.String("box", "", "box to start; needed when ezbx.toml has several")
	noDoctor := fs.Bool("no-doctor", false, "skip the doctor's checks at the end")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx up [--box NAME] [--config FILE] [--no-doctor]")
		return 2
	}
	plan, err := startBox(*cfgFlag, *boxFlag, !*noDoctor, stdout, stderr)
	switch {
	case errors.Is(err, errDoctorFound):
		fmt.Fprintf(stderr, "Box %q is up, but the doctor found problems (above).\n", plan.Box)
		return 1
	case err != nil:
		return report(stderr, err)
	case *noDoctor:
		fmt.Fprintf(stdout, "Box %q is up.\n", plan.Box)
	default:
		fmt.Fprintf(stdout, "\nBox %q is up. Open it with: docker exec -it %s bash\n", plan.Box, plan.Names.Box)
	}
	return 0
}

// errDoctorFound means the box started but the doctor's checks failed.
var errDoctorFound = errors.New("the box is up, but the doctor found problems")

// startBox starts a box from ezbx.toml (the only one, when name is empty)
// and, with doctorToo, runs the doctor. It never builds or pulls an image.
func startBox(cfgFlag, name string, doctorToo bool, stdout, stderr io.Writer) (box.Plan, error) {
	dirs, err := paths.Default()
	if err != nil {
		return box.Plan{}, err
	}
	cfgFile := dirs.ConfigFile(cfgFlag, os.Getenv)
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return box.Plan{}, err
	}
	if name == "" {
		if name, err = cfg.Default(); err != nil {
			return box.Plan{}, usageError(err.Error())
		}
	}
	fmt.Fprintln(stdout, "config:", cfgFile)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	plan, err := makePlan(ctx, cfg, name, dirs, stdout)
	if err != nil {
		return plan, err
	}
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.Up(ctx, plan); err != nil {
		return plan, err
	}
	recordEvent(target{dirs: dirs, name: plan.Box}, "box.started", map[string]any{"networkMode": plan.Network}, 0, stderr)
	if !doctorToo {
		return plan, nil
	}
	fmt.Fprintln(stdout)
	if code := runDoctorChecks([]string{"--config", cfgFile, "--box", plan.Box}, stdout, stderr, plan.Network == gateway.ModeAirgap); code != 0 {
		return plan, errDoctorFound
	}
	return plan, nil
}

// runDown removes a box's containers and networks. It never removes the
// workspace, the state folder or the agent's home volume.
func runDown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box name")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx down [--box NAME] [--config FILE]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	if err := stopBox(t, stdout, stderr); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Kept: the workspace, the state folder (token, log, approved hosts) and the volume %s.\n", box.NamesFor(t.name).Home)
	return 0
}

// stopBox removes a box's containers and networks, and its engine key file.
func stopBox(t target, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.Down(ctx, t.name, t.dirs.Box(t.name)); err != nil {
		return err
	}
	if t.box() != nil {
		recordEvent(t, "box.stopped", map[string]any{}, 0, stderr)
	}
	return nil
}

// makePlan turns the config into a plan: it checks the workspace, finds the
// engine and renders each harness's config. Nothing is changed yet.
func makePlan(ctx context.Context, cfg *config.Config, name string, dirs paths.Dirs, stdout io.Writer) (box.Plan, error) {
	b, ok := cfg.Get(name)
	if !ok {
		return box.Plan{}, errNoBox(name, cfg.Names())
	}
	home, _ := os.UserHomeDir()

	ws := paths.ExpandHome(b.Workspace, home)
	if !filepath.IsAbs(ws) {
		return box.Plan{}, fmt.Errorf("workspace %q must be a full path, or start with ~", b.Workspace)
	}
	ws = filepath.Clean(ws)
	if err := box.CheckWorkspace(ws, home, []string{dirs.Config, dirs.State}, systemDirs()); err != nil {
		return box.Plan{}, err
	}
	// Read-only folders are checked and walked before any engine is asked.
	ro, err := readOnlyPlan(b, ws, home, []string{dirs.Config, dirs.State}, stdout)
	if err != nil {
		return box.Plan{}, err
	}

	chosen, err := boxHarnesses(b)
	if err != nil {
		return box.Plan{}, err
	}
	gw, bx, err := imageSpecs(b.Tools, b.Harnesses)
	if err != nil {
		return box.Plan{}, err
	}
	env, err := harnessEnv(chosen)
	if err != nil {
		return box.Plan{}, err
	}
	plan := box.Plan{
		Box: name, Names: box.NamesFor(name), Workspace: ws, StateDir: dirs.Box(name), LogOn: cfg.LogOn(),
		Network: b.Network, Allow: box.AllowList(b.Allow, chosen),
		Memory: memoryLimit(ctx, b.Memory), CPUs: b.CPUsFlag(), PIDs: b.PIDs,
		Harnesses: b.Harnesses, Env: env,
		ReadOnly:     ro,
		GatewayImage: planImage(gw), BoxImage: planImage(bx),
	}
	if b.Network == gateway.ModeAirgap {
		fmt.Fprintln(stdout, "network: airgap (model server skipped)")
		return plan, nil
	}
	eng, err := chooseEngine(ctx, cfg)
	if err != nil {
		return box.Plan{}, err
	}
	upstream, err := box.GatewayUpstream(eng.URL)
	if err != nil {
		return box.Plan{}, err
	}
	fmt.Fprintf(stdout, "engine: %s at %s, %d model(s)\n", eng.Name, eng.URL, len(eng.Models))
	if w := detect.FileNamedWarning(eng); w != "" {
		fmt.Fprintln(stdout, "warning:", w+".")
	}
	files, err := renderHome(cfg, chosen, eng)
	if err != nil {
		return box.Plan{}, err
	}
	plan.Upstream, plan.EngineID, plan.EngineName = upstream, eng.ID, eng.Name
	plan.UpstreamHost, plan.DefaultModel, plan.KeyEnv = cfg.Engine.Host, cfg.Engine.DefaultModel, cfg.Engine.KeyEnv
	plan.HomeFiles = files
	return plan, nil
}

// memoryLimit is the configured limit, or, when there is none, half of
// Docker's memory from 2g to 8g (the same rule as ezbx init).
func memoryLimit(ctx context.Context, configured string) string {
	if configured != "" {
		return configured
	}
	return setup.MemoryFor(dockerMemory(ctx))
}

// dockerMemory is the memory Docker has, in bytes, or 0 if Docker doesn't
// say. On Docker Desktop this is its virtual machine's memory.
func dockerMemory(ctx context.Context) int64 {
	out, err := dockerIn(ctx, nil, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// boxHarnesses loads the manifests of a box's harnesses. Only verified ones
// are in the box image.
func boxHarnesses(b *config.Box) ([]harness.Manifest, error) {
	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return nil, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	var chosen []harness.Manifest
	for _, id := range b.Harnesses {
		m, ok := harness.Find(all, id)
		if !ok {
			return nil, fmt.Errorf("unknown harness %q", id)
		}
		if m.Verified.Status != "verified" {
			return nil, fmt.Errorf("harness %s is not verified yet, so it isn't in the box image", m.Name)
		}
		chosen = append(chosen, m)
	}
	return chosen, nil
}

// harnessEnv merges the box's harnesses' environment settings. Two
// harnesses that want different values for one variable are refused, since
// the box has one environment.
func harnessEnv(chosen []harness.Manifest) (map[string]string, error) {
	env := map[string]string{}
	from := map[string]string{}
	for _, m := range chosen {
		for k, v := range m.Env {
			if old, dup := env[k]; dup && old != v {
				return nil, fmt.Errorf("harnesses %s and %s set %s differently (%q and %q)", from[k], m.ID, k, old, v)
			}
			env[k], from[k] = v, m.ID
		}
	}
	return env, nil
}

// renderHome fills in each harness's config from the engine's models and
// the config's model overrides.
func renderHome(cfg *config.Config, chosen []harness.Manifest, eng detect.Engine) ([]harness.File, error) {
	overrides := map[string]harness.Override{}
	for id, m := range cfg.Models {
		overrides[id] = harness.Override{Name: m.Name, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Reasoning: m.Reasoning}
	}
	models := harness.Models(eng.Models, harness.Defaults{}, overrides)
	if len(models) == 0 {
		return nil, fmt.Errorf("the server at %s lists no models. Load a model, then try again", eng.URL)
	}
	values := harness.Values{GatewayBaseURL: box.GatewayBaseURL, GatewayKey: gatewayKey, Models: models}
	if cfg.Engine.DefaultModel != "" && len(chosen) != 0 {
		var err error
		values.Models, err = modelsWithAlias(models, cfg.Engine.DefaultModel)
		if err != nil {
			return nil, err
		}
		values.Default = gateway.ModelAlias
	}
	var files []harness.File
	seen := map[string]string{}
	for _, m := range chosen {
		out, err := harness.Render(m, values)
		if err != nil {
			return nil, err
		}
		for _, f := range out {
			if other, dup := seen[f.Path]; dup {
				return nil, fmt.Errorf("harnesses %s and %s both write ~/%s", other, m.ID, f.Path)
			}
			seen[f.Path] = m.ID
		}
		files = append(files, out...)
	}
	return files, nil
}

func modelsWithAlias(models []harness.Model, target string) ([]harness.Model, error) {
	withAlias := make([]harness.Model, 0, len(models)+1)
	var alias harness.Model
	for _, model := range models {
		if model.ID == target {
			alias = model
		}
		if model.ID != gateway.ModelAlias {
			withAlias = append(withAlias, model)
		}
	}
	if alias.ID == "" {
		return nil, fmt.Errorf("default model %q is not one the engine reports. Update default_model in ezbx.toml or make that model available", target)
	}
	alias.ID, alias.Name = gateway.ModelAlias, gateway.ModelAlias
	return append(withAlias, alias), nil
}

// kindsFS holds the API kind files. Tests replace it.
var kindsFS fs.FS = providers.FS

// apiKinds loads the API kinds, keeping only the one api names, or all of
// them for auto.
func apiKinds(api string) ([]adapter.Adapter, error) {
	all, err := adapter.LoadFS(kindsFS)
	if err != nil {
		return nil, fmt.Errorf("internal error loading the API kinds: %w", err)
	}
	if api == config.DefaultAPI {
		return all, nil
	}
	var ids []string
	for _, a := range all {
		if a.ID == api {
			return []adapter.Adapter{a}, nil
		}
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("engine.api %q: use auto or one of %s", api, strings.Join(ids, ", "))
}

// chooseEngine checks the model server the config names. Only engine.url is
// contacted: ezbx up never scans this computer for other servers.
func chooseEngine(ctx context.Context, cfg *config.Config) (detect.Engine, error) {
	e := cfg.Engine
	kinds, err := apiKinds(e.API)
	if err != nil {
		return detect.Engine{}, err
	}
	if e.URL == "" {
		return detect.Engine{}, errors.New("no model server is set: put its address in url under [engine] in ezbx.toml, such as url = 'http://127.0.0.1:1234'. ezbx engine detect lists the servers it finds on this computer")
	}
	keys := map[string]string{}
	if e.KeyEnv != "" {
		// Checked before anything is contacted, so a missing key is never a
		// confusing "not found".
		k := os.Getenv(e.KeyEnv)
		if k == "" {
			return detect.Engine{}, fmt.Errorf("engine.key_env names %s, but that environment variable is empty or not set", e.KeyEnv)
		}
		keys[e.URL] = k // sent to this address only (see detect.Keys)
	}
	res := (&detect.Detector{Adapters: kinds, SkipHintPorts: true, Keys: keys}).Detect(ctx, []string{e.URL})
	if len(res.Engines) == 1 {
		return res.Engines[0], nil
	}
	what := "Nothing answered at " + e.URL + " with an OpenAI- or Anthropic-compatible API"
	if e.API != config.DefaultAPI {
		what = "Nothing answered at " + e.URL + " with the " + kinds[0].Name
	}
	if why := refusedHint(res.Refused, e.KeyEnv); why != "" {
		return detect.Engine{}, errors.New(what + ". " + why)
	}
	return detect.Engine{}, errors.New(what + ". Start the server (and load a model), then try again. If it moved, change url under [engine] in ezbx.toml, or use ezbx engine use --url ADDRESS on a running box")
}

// refusedHint explains an address that answered 401 or 403. keyEnv names
// the variable whose key was sent, if any. It is "" when nothing refused.
func refusedHint(refused []detect.Refusal, keyEnv string) string {
	var parts []string
	for _, r := range refused {
		if r.KeySent {
			parts = append(parts, fmt.Sprintf("The server at %s refused the key in %s: check that it is the right key", r.URL, keyEnv))
		} else {
			parts = append(parts, fmt.Sprintf("The server at %s asks for a key: put key_env = 'NAME' under [engine] in ezbx.toml, with the key in that environment variable", r.URL))
		}
	}
	return strings.Join(parts, ". ")
}

// systemDirs are folders that must never be a workspace.
func systemDirs() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for _, k := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if v := os.Getenv(k); v != "" {
				out = append(out, filepath.Clean(v))
			}
		}
		return out
	}
	return []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/sbin", "/sys", "/usr",
		"/System", "/Library", "/Applications", "/private/etc"}
}

// dockerIn runs the docker command, with stdin when given.
func dockerIn(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}
