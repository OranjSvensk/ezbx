// Package config reads and checks ezbx.toml.
//
// Unknown keys are refused, so a typo such as "netwrok" is an error rather
// than a setting that is quietly ignored.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/readonly"
)

// Config is the whole file.
type Config struct {
	Version int              `toml:"version"`
	Box     map[string]*Box  `toml:"box"`
	Engine  Engine           `toml:"engine"`
	Models  map[string]Model `toml:"models"`
	Gateway Gateway          `toml:"gateway"`
	names   []string         // box names, sorted
}

// Box is one sandbox: a gateway and a box container.
type Box struct {
	Workspace string     `toml:"workspace"` // the only folder the agent can write to
	Memory    string     `toml:"memory"`    // Docker's form, such as 4g or 512m
	CPUs      float64    `toml:"cpus"`      // 0 means no limit
	PIDs      int        `toml:"pids"`      // process limit
	Network   string     `toml:"network"`   // airgap | off | allow | on
	Allow     []string   `toml:"allow"`     // extra hosts for allow mode
	Harnesses []string   `toml:"harnesses"`
	Tools     []string   `toml:"tools"` // tool ids and "pack:" names; nil when the key is left out
	ReadOnly  []ReadOnly `toml:"readonly"`
	Snapshots Snapshots  `toml:"snapshots"`
}

// Snapshots says what snapshots of the workspace leave out and how many
// automatic ones are kept. Empty values mean the defaults.
type Snapshots struct {
	Skip    []string `toml:"skip"`     // names to leave out, at any depth; nil means the defaults
	MaxFile string   `toml:"max_file"` // larger files are left out, such as 100m
	Keep    int      `toml:"keep"`     // automatic snapshots kept
}

// ReadOnly is a folder the agent may read but not change, shown in the box
// as /readonly/<name>. See docs/readonly.md. ezbx up doesn't start a box
// with one yet.
type ReadOnly struct {
	Path   string   `toml:"path"`
	Name   string   `toml:"name"`   // under /readonly; the folder's own name, lowercased, if empty
	Mask   []string `toml:"mask"`   // masked on top of readonly.Defaults
	Unmask []string `toml:"unmask"` // defaults to show after all, written exactly
}

// Engine is the model server on this computer: where it is and which API it
// speaks. ezbx works with any server that offers one of the API kinds in
// providers/ (see docs/engine-adapters.md), whatever program runs it.
type Engine struct {
	API    string `toml:"api"`     // auto, or an API kind such as openai or lmstudio
	URL    string `toml:"url"`     // the server's address; the only one ezbx up contacts
	KeyEnv string `toml:"key_env"` // NAME of an environment variable, never the key
	Host   string `toml:"host"`    // Host header to send, like --upstream-host
}

// DefaultAPI means: use the most specific API kind the server answers.
const DefaultAPI = "auto"

// Model holds facts about one model that engines don't report.
type Model struct {
	Name          string `toml:"name"`
	ContextWindow int    `toml:"context"`
	MaxTokens     int    `toml:"max_tokens"`
	Reasoning     *bool  `toml:"reasoning"`
}

// Gateway settings.
type Gateway struct {
	Log *bool `toml:"log"` // keep a request log; on by default
}

// Defaults used when the file leaves a value out.
// (There is no fixed default memory limit: it comes from Docker's memory.)
const (
	DefaultPIDs    = 512
	DefaultNetwork = "off"
)

// oldEngine explains the engine settings of ezbx 0.2 and earlier, which named
// an engine (engine.use and [engine.<id>] tables) instead of an address.
const oldEngine = `the [engine] section changed: ezbx now works with any server by the API it speaks. Write it as
  [engine]
  url = "http://127.0.0.1:PORT"   # the server's address
  api = "auto"                    # or openai or lmstudio
with key_env and host there too if you had them (ezbx engine detect lists the servers it finds)`

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config file at %s. Copy ezbx.toml.example there and edit it", path)
		}
		return nil, err
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads and checks a config from text.
func Parse(text string) (*Config, error) {
	c := &Config{}
	md, err := toml.Decode(text, c)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		return nil, err
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		var keys []string
		for _, k := range bad {
			if len(k) >= 2 && k[0] == "engine" && (k[1] == "use" || len(k) > 2) {
				return nil, errors.New(oldEngine)
			}
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown setting: %s", strings.Join(keys, ", "))
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return c, nil
}

var (
	boxNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	memoryRE  = regexp.MustCompile(`^[1-9][0-9]*[kmg]$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	toolRE    = regexp.MustCompile(`^(pack:)?[a-z][a-z0-9-]{0,31}$`)
	hostRE    = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// HostName reports whether s is an allowlist entry: a host name such as
// registry.npmjs.org, or *.example.com for the names under it. A wildcard
// over a top-level domain (*.com) is refused, and so is an address such as
// 10.0.0.1: allowlists hold names, never addresses.
func HostName(s string) bool {
	if !hostRE.MatchString(s) {
		return false
	}
	// No top-level domain is all digits, so a last label of digits means
	// the text is an address (or a broken one).
	last := s[strings.LastIndexByte(s, '.')+1:]
	return strings.Trim(last, "0123456789") != ""
}

// NetworkModes are the network modes, from the most closed to the most open:
// airgap (no model route, no internet), off (the model route only), allow
// (the model route and listed hosts) and on (the public internet too).
var NetworkModes = []string{"airgap", "off", "allow", "on"}

// NetworkMode reports whether s is one of the network modes.
func NetworkMode(s string) bool { return slices.Contains(NetworkModes, s) }

// MemoryValue reports whether s is a memory limit in Docker's form, such as
// 4g or 512m.
func MemoryValue(s string) bool { return memoryRE.MatchString(strings.ToLower(s)) }

// BoxName reports whether s can be used in Docker resource names.
func BoxName(s string) bool { return boxNameRE.MatchString(s) }

func (c *Config) check() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1 (found %d)", c.Version)
	}
	if len(c.Box) == 0 {
		return errors.New("no box: add a [box.main] section")
	}
	for name, b := range c.Box {
		if !BoxName(name) {
			return fmt.Errorf("box name %q: use up to 32 lowercase letters, digits and dashes", name)
		}
		if b == nil {
			return fmt.Errorf("box.%s is empty", name)
		}
		if err := b.check(name); err != nil {
			return err
		}
		c.names = append(c.names, name)
	}
	sort.Strings(c.names)
	if err := c.checkWorkspaces(); err != nil {
		return err
	}
	e := &c.Engine
	if e.API == "" {
		e.API = DefaultAPI
	}
	// Which kinds exist is the adapters' business; the command checks api.
	if e.URL != "" && !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
		return errors.New("engine.url must start with http:// or https://")
	}
	if e.KeyEnv != "" && !envNameRE.MatchString(e.KeyEnv) {
		return errors.New("engine.key_env is the NAME of an environment variable, such as ENGINE_API_KEY, never the key itself")
	}
	if e.KeyEnv != "" && e.URL == "" {
		return errors.New("engine.key_env needs url too, such as url = 'http://127.0.0.1:PORT', so the key is only ever sent to that address")
	}
	for id, m := range c.Models {
		if m.ContextWindow < 0 || m.MaxTokens < 0 {
			return fmt.Errorf("models.%q: sizes can't be negative", id)
		}
	}
	return nil
}

func (b *Box) check(name string) error {
	p := "box." + name + "."
	if strings.TrimSpace(b.Workspace) == "" {
		return errors.New(p + "workspace is required: the one folder the agent can write to")
	}
	// An empty memory limit means: work it out from Docker's memory when the
	// box starts (see setup.MemoryFor).
	b.Memory = strings.ToLower(b.Memory)
	if b.Memory != "" && !MemoryValue(b.Memory) {
		return fmt.Errorf("%smemory %q: use a whole number and k, m or g, such as 4g", p, b.Memory)
	}
	if b.CPUs < 0 {
		return errors.New(p + "cpus can't be negative")
	}
	if b.PIDs == 0 {
		b.PIDs = DefaultPIDs
	}
	if b.PIDs < 64 {
		return errors.New(p + "pids must be at least 64")
	}
	if b.Network == "" {
		b.Network = DefaultNetwork
	}
	if !NetworkMode(b.Network) {
		return fmt.Errorf("%snetwork %q: use airgap, off, allow or on", p, b.Network)
	}
	for _, h := range b.Allow {
		if !HostName(h) {
			return fmt.Errorf("%sallow %q: use a host name such as registry.npmjs.org or *.example.com", p, h)
		}
	}
	if len(b.Harnesses) == 0 {
		return errors.New(p + `harnesses is empty: add at least one, such as ["pi"]`)
	}
	// Only the form is checked here; the catalogue decides whether a tool or
	// pack exists, when the image is planned.
	for _, t := range b.Tools {
		if !toolRE.MatchString(t) {
			return fmt.Errorf(`%stools %q: use a tool id such as "ffmpeg" or a pack such as "pack:coding"`, p, t)
		}
	}
	for _, s := range b.Snapshots.Skip {
		if s == "" || strings.ContainsAny(s, `/\`) || s == "." || s == ".." {
			return fmt.Errorf("%ssnapshots.skip %q: use a folder or file name such as node_modules or *.log, not a path", p, s)
		}
		if _, err := path.Match(s, ""); err != nil {
			return fmt.Errorf("%ssnapshots.skip %q: %v", p, s, err)
		}
	}
	b.Snapshots.MaxFile = strings.ToLower(b.Snapshots.MaxFile)
	if b.Snapshots.MaxFile != "" && !MemoryValue(b.Snapshots.MaxFile) {
		return fmt.Errorf("%ssnapshots.max_file %q: use a whole number and k, m or g, such as 100m", p, b.Snapshots.MaxFile)
	}
	if b.Snapshots.Keep < 0 {
		return errors.New(p + "snapshots.keep can't be negative")
	}
	names := map[string]bool{}
	for i, r := range b.ReadOnly {
		q := fmt.Sprintf("%sreadonly[%d].", p, i)
		if strings.TrimSpace(r.Path) == "" {
			return errors.New(q + "path is required: the folder the agent may read")
		}
		if r.Name != "" {
			if err := readonly.CheckName(r.Name); err != nil {
				return errors.New(q + err.Error())
			}
			if names[r.Name] {
				return fmt.Errorf("%sname %q is used twice; give each read-only folder its own name", q, r.Name)
			}
			names[r.Name] = true
		}
		for _, m := range r.Mask {
			if err := readonly.CheckPattern(m); err != nil {
				return fmt.Errorf("%smask %q %v", q, m, err)
			}
		}
		for _, u := range r.Unmask {
			if !readonly.IsDefault(u) {
				return fmt.Errorf("%sunmask %q isn't one of the default masks; write it exactly as listed in docs/readonly.md", q, u)
			}
		}
	}
	return nil
}

// checkWorkspaces refuses two boxes whose workspaces are the same folder or
// one inside the other: each box snapshots and rewinds its own workspace,
// so a shared one would let one box's rewind undo another agent's work.
func (c *Config) checkWorkspaces() error {
	home, _ := os.UserHomeDir()
	ws := func(n string) string { return filepath.Clean(paths.ExpandHome(c.Box[n].Workspace, home)) }
	for i, a := range c.names {
		for _, b := range c.names[i+1:] {
			wa, wb := ws(a), ws(b)
			if paths.Within(wa, wb) || paths.Within(wb, wa) {
				return fmt.Errorf("boxes %s and %s have overlapping workspaces (%s and %s); give each box its own folder", a, b, wa, wb)
			}
		}
	}
	return nil
}

// Names lists the boxes, sorted.
func (c *Config) Names() []string { return append([]string(nil), c.names...) }

// Get returns the box with this name, if the config has it.
func (c *Config) Get(name string) (*Box, bool) {
	b, ok := c.Box[name]
	return b, ok
}

// Default is the box a command acts on when none is named: the only one.
// With several boxes, the command must be told which.
func (c *Config) Default() (string, error) {
	if len(c.names) == 1 {
		return c.names[0], nil
	}
	return "", fmt.Errorf("ezbx.toml has %d boxes (%s); say which with --box NAME", len(c.names), strings.Join(c.names, ", "))
}

// LogOn reports whether the gateway keeps a request log.
func (c *Config) LogOn() bool { return c.Gateway.Log == nil || *c.Gateway.Log }

// CPUsFlag formats the CPU limit for docker, or "" for none.
func (b *Box) CPUsFlag() string {
	if b.CPUs == 0 {
		return ""
	}
	return strconv.FormatFloat(b.CPUs, 'f', -1, 64)
}
