// Package setup is the logic of ezbx init: the questions, their defaults,
// the plan shown before anything changes, and the config file it writes.
// Everything with side effects (Docker, files, detection) stays in the
// command, so the decisions here can be tested on their own.
package setup

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/config"
)

// Answers are the settings init writes to ezbx.toml.
type Answers struct {
	Box       string
	Workspace string // full path
	Memory    string
	CPUs      float64
	PIDs      int
	Network   string
	Allow     []string
	Harnesses []string
	Tools     []string          // tool ids and "pack:" names; nil when not set
	ReadOnly  []config.ReadOnly // read-only folders, as in ezbx.toml
	Snapshots config.Snapshots  // kept as it is; init doesn't ask about it
	Engine    config.Engine     // api, url, key_env, host
	Models    map[string]config.Model
	Log       *bool
	Others    map[string]config.Box // the config's other boxes, written back unchanged
}

// FromConfig turns an existing config with one box into answers, so running
// init again starts from what is there and keeps what init doesn't ask about.
func FromConfig(c *config.Config) Answers {
	a, _ := FromConfigBox(c, c.Names()[0])
	return a
}

// FromConfigBox is FromConfig for the named box. When the config doesn't
// have that box yet, the answers start a new one that shares the config's
// engine and model settings, and found is false. Every other box is kept in
// Others, so writing the answers back never drops one.
func FromConfigBox(c *config.Config, name string) (a Answers, found bool) {
	a = Answers{Box: name, Network: config.DefaultNetwork, Engine: c.Engine, Models: maps.Clone(c.Models), Log: c.Gateway.Log}
	for _, n := range c.Names() {
		if n == name {
			continue
		}
		if a.Others == nil {
			a.Others = map[string]config.Box{}
		}
		o, _ := c.Get(n)
		a.Others[n] = cloneBox(*o)
	}
	b, found := c.Get(name)
	if !found {
		return a, false
	}
	a.Workspace, a.Memory, a.CPUs, a.PIDs = b.Workspace, b.Memory, b.CPUs, b.PIDs
	a.Network, a.Allow, a.Harnesses = b.Network, slices.Clone(b.Allow), slices.Clone(b.Harnesses)
	a.Tools, a.ReadOnly, a.Snapshots = slices.Clone(b.Tools), cloneReadOnly(b.ReadOnly), cloneSnapshots(b.Snapshots)
	if a.PIDs == config.DefaultPIDs {
		a.PIDs = 0 // the default, so not written
	}
	return a, true
}

// box is the answers' own box, in the config's form.
func (a Answers) box() config.Box {
	return config.Box{
		Workspace: a.Workspace, Memory: a.Memory, CPUs: a.CPUs, PIDs: a.PIDs, Network: a.Network,
		Allow: a.Allow, Harnesses: a.Harnesses, Tools: a.Tools, ReadOnly: a.ReadOnly, Snapshots: a.Snapshots,
	}
}

func cloneBox(b config.Box) config.Box {
	b.Allow, b.Harnesses, b.Tools = slices.Clone(b.Allow), slices.Clone(b.Harnesses), slices.Clone(b.Tools)
	b.ReadOnly, b.Snapshots = cloneReadOnly(b.ReadOnly), cloneSnapshots(b.Snapshots)
	return b
}

func cloneSnapshots(s config.Snapshots) config.Snapshots {
	s.Skip = slices.Clone(s.Skip)
	return s
}

// MemoryFor is the default memory limit: half of what Docker has, rounded
// down to whole gigabytes, from 2 to 8. On Docker Desktop the box runs in
// Docker's own virtual machine, so its memory is what counts, not the
// computer's. dockerBytes is 0 when it isn't known.
func MemoryFor(dockerBytes int64) string {
	gb := dockerBytes / 2 / (1 << 30)
	if gb < 2 {
		gb = 2
	}
	if gb > 8 {
		gb = 8
	}
	return strconv.FormatInt(gb, 10) + "g"
}

// ConfigText writes the answers as ezbx.toml, with comments that explain
// each setting. Settings left at their defaults are left out.
func ConfigText(a Answers, now time.Time) string {
	var b strings.Builder
	w := func(format string, v ...any) { fmt.Fprintf(&b, format, v...) }
	w("# ezbx config, written by ezbx init on %s.\n", now.Format("2006-01-02"))
	w("# Edit it by hand, or run ezbx init again. Unknown settings are refused.\n")
	w("version = 1\n\n")

	boxes := maps.Clone(a.Others)
	if boxes == nil {
		boxes = map[string]config.Box{}
	}
	boxes[a.Box] = a.box() // the answers win over any old copy

	for _, n := range sortedKeys(boxes) {
		b.WriteString(boxText(n, boxes[n]))
		w("\n")
	}

	e := a.Engine
	if e.API == "" {
		e.API = config.DefaultAPI
	}
	w("# The model server on this computer. Any OpenAI- or Anthropic-compatible\n")
	w("# API works; api says which kind (auto picks the most specific one it answers).\n")
	w("[engine]\n")
	if e.URL != "" {
		w("url     = %s\n", quote(e.URL))
	} else {
		w("# url   = \"http://127.0.0.1:PORT\"   # needed before ezbx up; see ezbx engine detect\n")
	}
	w("api     = %s   # auto | openai | lmstudio | anthropic\n", quote(e.API))
	if e.KeyEnv != "" {
		w("key_env = %s   # the NAME of an environment variable, never the key\n", quote(e.KeyEnv))
	}
	if e.Host != "" {
		w("host    = %s\n", quote(e.Host))
	}
	if e.DefaultModel != "" {
		w("default_model = %s   # used for requests that ask for the model \"default\"\n", quote(e.DefaultModel))
	}
	for _, id := range sortedKeys(a.Models) {
		m := a.Models[id]
		if m == (config.Model{}) {
			continue
		}
		w("\n[models.%s]\n", quote(id))
		if m.Name != "" {
			w("name       = %s\n", quote(m.Name))
		}
		if m.Reasoning != nil {
			w("reasoning  = %t\n", *m.Reasoning)
		}
		if m.MaxTokens > 0 {
			w("max_tokens = %d\n", m.MaxTokens)
		}
		if m.ContextWindow > 0 {
			w("context    = %d\n", m.ContextWindow)
		}
	}
	log := a.Log == nil || *a.Log
	w("\n[gateway]\nlog = %t   # keep a request log in the state folder\n", log)
	return b.String()
}

// boxText writes one [box.NAME] section with its read-only folders and
// snapshot settings.
func boxText(name string, x config.Box) string {
	var b strings.Builder
	w := func(format string, v ...any) { fmt.Fprintf(&b, format, v...) }
	w("[box.%s]\n", name)
	w("workspace = %s   # the ONLY folder the agent can write to\n", quote(x.Workspace))
	if x.Memory != "" {
		w("memory    = %s\n", quote(x.Memory))
	}
	if x.CPUs > 0 {
		w("cpus      = %s\n", strconv.FormatFloat(x.CPUs, 'f', -1, 64))
	}
	if x.PIDs > 0 && x.PIDs != config.DefaultPIDs {
		w("pids      = %d\n", x.PIDs)
	}
	w("network   = %s   # airgap | off | allow | on\n", quote(x.Network))
	if len(x.Allow) > 0 {
		w("allow     = %s   # hosts for allow mode, besides the harnesses' own\n", list(x.Allow))
	}
	w("harnesses = %s\n", list(x.Harnesses))
	if x.Tools != nil {
		w("tools     = %s   # tool packs (pack:NAME) and single tools; after a change, ezbx build\n", list(x.Tools))
	}
	b.WriteString(readOnlyText(name, x.ReadOnly))
	if s := x.Snapshots; s.Skip != nil || s.MaxFile != "" || s.Keep > 0 {
		w("\n[box.%s.snapshots]\n", name)
		if s.Skip != nil {
			w("skip     = %s\n", list(s.Skip))
		}
		if s.MaxFile != "" {
			w("max_file = %s\n", quote(s.MaxFile))
		}
		if s.Keep > 0 {
			w("keep     = %d\n", s.Keep)
		}
	}
	return b.String()
}

// quote writes a TOML basic string. Only the escapes TOML knows are used.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func list(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
