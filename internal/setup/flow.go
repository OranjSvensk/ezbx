package setup

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/tools"
)

// Facts are what the command found before asking anything.
type Facts struct {
	Engines      []detect.Engine    // model servers answering now
	Refused      []detect.Refusal   // addresses that answered 401 or 403
	Harnesses    []harness.Manifest // verified ones, offered
	Catalog      *tools.Catalog     // the tool catalogue
	DockerMemory int64              // bytes; 0 if unknown
	Home         string
	Own          []string // ezbx's config and state folders
	System       []string // folders that may never be the workspace
	Existing     *config.Config
	known        bool // Existing has the box being set up (set by Gather)
}

// Options are the answers given as flags. Empty means "ask".
type Options struct {
	Box       string // the box to set up; "" means the config's only box, else main
	Profile   string // starter profile id; empty prompts for a new box
	API       string // an API kind or "auto"; "" keeps what the config has
	URL       string // the server's address, if the scan doesn't find it
	Later     bool   // no server now: write the config without choosing one
	KeyEnv    string
	Reasoning bool // mark every model of the chosen engine as reasoning
	Harnesses []string
	Tools     []string // nil: ask; empty: none
	Workspace string
	ReadOnly  []string // read-only folders; nil: ask
	Network   string
	Memory    string
}

// Result is what Gather decided.
type Result struct {
	Answers  Answers
	Engine   *detect.Engine // nil when no server was chosen ("later")
	ReadOnly []ReadOnlySummary
}

// Gather asks every question in order, using flags first, then the
// existing config, then what was detected, as defaults.
func Gather(f Facts, o Options, p *Prompter) (Result, error) {
	name := o.Box
	if name == "" && f.Existing != nil {
		n, err := f.Existing.Default()
		if err != nil {
			return Result{}, err
		}
		name = n
	}
	if name == "" {
		name = "main"
	}
	if !config.BoxName(name) {
		return Result{}, fmt.Errorf("box name %q: use up to 32 lowercase letters, digits and dashes", name)
	}
	a := Answers{Box: name, Network: "off", Engine: config.Engine{API: config.DefaultAPI}, Models: map[string]config.Model{}}
	if f.Existing != nil {
		a, f.known = FromConfigBox(f.Existing, name)
		if a.Models == nil {
			a.Models = map[string]config.Model{}
		}
	}
	var res Result

	// 1. Engine.
	eng, err := chooseEngine(f, o, &a, p)
	if err != nil {
		return res, err
	}
	res.Engine = eng
	if eng != nil {
		if err := askReasoning(*eng, o, &a, p); err != nil {
			return res, err
		}
	}

	// 1b. Starter profile (new configs, or when --profile is given).
	fromProfile, err := askProfile(f, o, &a, p)
	if err != nil {
		return res, err
	}

	// 2. Harnesses, then the tools built in beside them.
	if err := askHarnesses(f, o, &a, p); err != nil {
		return res, err
	}
	if err := askTools(f, o, &a, p); err != nil {
		return res, err
	}

	// 3. Workspace.
	def := o.Workspace
	if def == "" {
		def = a.Workspace
	}
	if def == "" {
		def = filepath.Join(f.Home, "ezbx", "workspace")
	}
	fmt.Fprintln(p.Out, "\nThe workspace is the only folder the agent can change. It is created if missing.")
	ws, err := p.Ask("Workspace folder", def, func(s string) error {
		c, err := cleanWorkspace(s, f)
		if err != nil {
			return err
		}
		return otherWorkspace(c, a.Others, f.Home)
	})
	if err != nil {
		return res, err
	}
	a.Workspace, _ = cleanWorkspace(ws, f)

	// 3b. Read-only folders, checked against the workspace.
	if err := askReadOnly(f, o, &a, p, &res); err != nil {
		return res, err
	}

	// 4. Network.
	if err := askNetwork(f, o, &a, p, fromProfile); err != nil {
		return res, err
	}

	// 5. Limits.
	def = o.Memory
	if def == "" {
		def = a.Memory
	}
	if def == "" {
		def = MemoryFor(f.DockerMemory)
	}
	fmt.Fprintf(p.Out, "\nThe memory limit keeps the box from taking over this computer. The default is half of Docker's memory%s, from 2g to 8g.\n", gbText(f.DockerMemory))
	mem, err := p.Ask("Memory limit", def, func(s string) error {
		if !config.MemoryValue(s) {
			return errors.New("Use a whole number and k, m or g, such as 4g.")
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	a.Memory = strings.ToLower(mem)
	res.Answers = a
	return res, nil
}

func gbText(b int64) string {
	if b <= 0 {
		return ""
	}
	return fmt.Sprintf(" (Docker has %.1f GB)", float64(b)/(1<<30))
}

// cleanWorkspace expands ~, makes the path full and checks it.
func cleanWorkspace(s string, f Facts) (string, error) {
	ws := paths.ExpandHome(strings.TrimSpace(s), f.Home)
	if !filepath.IsAbs(ws) {
		return "", fmt.Errorf("Give a full path, or one starting with ~.")
	}
	ws = filepath.Clean(ws)
	if err := box.CheckWorkspace(ws, f.Home, f.Own, f.System); err != nil {
		return "", err
	}
	return ws, nil
}

// otherWorkspace refuses a workspace that is, holds or sits inside another
// box's: each box snapshots and rewinds its own workspace.
func otherWorkspace(ws string, others map[string]config.Box, home string) error {
	for _, n := range sortedKeys(others) {
		o := filepath.Clean(paths.ExpandHome(others[n].Workspace, home))
		if paths.Within(ws, o) || paths.Within(o, ws) {
			return fmt.Errorf("%s overlaps box %s's workspace %s; give each box its own folder", ws, n, o)
		}
	}
	return nil
}

// speaks reports whether e answers as the API kind api: its best match, or
// one it also matched (a server with LM Studio's API also speaks OpenAI's).
func speaks(e detect.Engine, api string) bool {
	return api == "" || api == config.DefaultAPI || e.ID == api || slices.Contains(e.AlsoMatched, api)
}

// sameURL compares addresses the way detection does: without trailing slashes.
func sameURL(x, y string) bool { return strings.TrimRight(x, "/") == strings.TrimRight(y, "/") }

func chooseEngine(f Facts, o Options, a *Answers, p *Prompter) (*detect.Engine, error) {
	// Flags first: they apply whether or not a server is chosen now.
	setEngine := func(url string) {
		if url != "" && !sameURL(url, a.Engine.URL) {
			// key_env and host belonged to the old address. A key is only
			// ever sent to its own url, so never carry it to a new one.
			a.Engine = config.Engine{API: a.Engine.API, URL: url}
		}
		if o.API != "" {
			a.Engine.API = o.API
		}
		if o.KeyEnv != "" {
			a.Engine.KeyEnv = o.KeyEnv
		}
	}

	var offered []detect.Engine
	for _, e := range f.Engines {
		if speaks(e, o.API) {
			offered = append(offered, e)
		}
	}
	fmt.Fprintln(p.Out, "Model servers found on this computer (any OpenAI- or Anthropic-compatible API works)")
	if len(offered) == 0 {
		fmt.Fprintln(p.Out, "  none. Start your model server and load a model, then run ezbx init again,")
		fmt.Fprintln(p.Out, "  or carry on: the config is written, and ezbx up works once the server runs.")
	}
	for _, r := range f.Refused {
		if r.KeySent {
			fmt.Fprintf(p.Out, "  (The server at %s refused the key it was sent. Check the key.)\n", r.URL)
		} else {
			fmt.Fprintf(p.Out, "  (A server at %s asks for a key. To use it: ezbx init --url %s --key-env NAME)\n", r.URL, r.URL)
		}
	}
	for i, e := range offered {
		fmt.Fprintf(p.Out, "  %d. %-24s %s, %d model(s)\n", i+1, e.Name, e.URL, len(e.Models))
	}
	if len(offered) > 0 {
		fmt.Fprintln(p.Out, "  L. I'll start one later")
		fmt.Fprintln(p.Out, "  Not listed? Run ezbx init --url http://127.0.0.1:PORT")
	}

	// The default: --url, then the configured address if it answers, then
	// the first server found.
	def := ""
	pick := func(url string) {
		for i, e := range offered {
			if def == "" && sameURL(e.URL, url) {
				def = strconv.Itoa(i + 1)
			}
		}
	}
	switch {
	case o.Later:
		def = "L"
	case o.URL != "":
		pick(o.URL)
		if def == "" {
			// Written anyway, so init can come before the server starts;
			// the plan then says the box won't be started yet.
			fmt.Fprintf(p.Out, "  (Nothing answers at %s now.)\n", o.URL)
			setEngine(o.URL)
			return nil, nil
		}
	case a.Engine.URL != "":
		pick(a.Engine.URL)
	}
	if def == "" && len(offered) > 0 {
		def = "1"
	}
	if len(offered) == 0 {
		setEngine(o.URL)
		return nil, nil
	}
	ans, err := p.Ask("Use which?", def, func(s string) error {
		if strings.EqualFold(s, "l") {
			return nil
		}
		if n, err := strconv.Atoi(s); err != nil || n < 1 || n > len(offered) {
			return fmt.Errorf("Type a number from 1 to %d, or L.", len(offered))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(ans, "l") {
		setEngine(o.URL)
		return nil, nil
	}
	n, _ := strconv.Atoi(ans)
	e := offered[n-1]
	// ezbx up contacts only this address, so it is always written.
	setEngine(e.URL)
	if o.API == "" {
		a.Engine.API = e.ID
	}
	return &e, nil
}

func askReasoning(e detect.Engine, o Options, a *Answers, p *Prompter) error {
	if len(e.Models) == 0 {
		fmt.Fprintf(p.Out, "\n%s lists no models yet. Load one before ezbx up.\n", e.Name)
		return nil
	}
	fmt.Fprintln(p.Out, "\nServers don't say whether a model is a reasoning model (one that thinks before it")
	fmt.Fprintln(p.Out, "answers). A reasoning model needs this set, or its answers can be cut off.")
	for _, m := range e.Models {
		cur := a.Models[m.ID]
		def := o.Reasoning || (cur.Reasoning != nil && *cur.Reasoning)
		yes, err := p.YesNo("Is "+m.ID+" a reasoning model?", def)
		if err != nil {
			return err
		}
		if yes || cur.Reasoning != nil {
			v := yes
			cur.Reasoning = &v
		}
		if cur == (config.Model{}) {
			delete(a.Models, m.ID)
		} else {
			a.Models[m.ID] = cur
		}
	}
	return nil
}

// FormatProfiles formats the starter profiles in cat for ezbx init --list-profiles.
func FormatProfiles(cat *tools.Catalog) string {
	if cat == nil || len(cat.Profiles) == 0 {
		return "No starter profiles found.\n"
	}
	var b strings.Builder
	b.WriteString("Starter profiles (sets defaults you can still change during setup):\n")
	for _, id := range cat.ProfileIDs() {
		pr := cat.Profiles[id]
		fmt.Fprintf(&b, "  %-10s %s — %s\n", pr.ID, pr.Name, pr.Description)
		fmt.Fprintf(&b, "  %-10s harnesses: %s | tools: %s | %s\n", "",
			strings.Join(pr.Harnesses, ", "), toolsText(pr.Tools), profileNetText(pr))
	}
	return b.String()
}

func profileNetText(pr tools.Profile) string {
	if pr.Network == "allow" && len(pr.Allow) > 0 {
		return "network: allow (" + strings.Join(pr.Allow, ", ") + ")"
	}
	return "network: " + pr.Network
}

func askProfile(f Facts, o Options, a *Answers, p *Prompter) (bool, error) {
	if f.Catalog == nil || len(f.Catalog.Profiles) == 0 {
		if o.Profile != "" {
			return false, fmt.Errorf("unknown profile %q (no starter profiles available)", o.Profile)
		}
		return false, nil
	}
	if o.Profile != "" {
		pr, ok := f.Catalog.Profiles[o.Profile]
		if !ok {
			return false, fmt.Errorf("unknown profile %q (available: %s)", o.Profile, strings.Join(f.Catalog.ProfileIDs(), ", "))
		}
		if err := applyProfile(f, pr, a); err != nil {
			return false, err
		}
		return true, nil
	}
	if f.known {
		return false, nil
	}
	ids := f.Catalog.ProfileIDs()
	fmt.Fprintln(p.Out, "\nStarter profiles (sets harnesses, tools and network defaults; each step below can still change them)")
	for i, id := range ids {
		pr := f.Catalog.Profiles[id]
		fmt.Fprintf(p.Out, "  %d. %-10s %s — %s\n", i+1, pr.ID, pr.Name, pr.Description)
		fmt.Fprintf(p.Out, "     %-10s tools: %s; %s\n", "", toolsText(pr.Tools), profileNetText(pr))
	}
	ans, err := p.Ask("Starter profile", "1", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > len(ids) {
			return fmt.Errorf("Type a number from 1 to %d.", len(ids))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(ans)
	if err := applyProfile(f, f.Catalog.Profiles[ids[n-1]], a); err != nil {
		return false, err
	}
	return true, nil
}

func applyProfile(f Facts, pr tools.Profile, a *Answers) error {
	avail := map[string]bool{}
	for _, m := range f.Harnesses {
		avail[m.ID] = true
	}
	for _, id := range pr.Harnesses {
		if !avail[id] {
			return fmt.Errorf("profile %q names harness %q, which is not available", pr.ID, id)
		}
	}
	a.Harnesses = slices.Clone(pr.Harnesses)
	a.Tools = slices.Clone(pr.Tools)
	a.Network = pr.Network
	a.Allow = slices.Clone(pr.Allow)
	return nil
}

func askHarnesses(f Facts, o Options, a *Answers, p *Prompter) error {
	if len(f.Harnesses) == 0 {
		return errors.New("internal error: no verified harness to offer")
	}
	fmt.Fprintln(p.Out, "\nHarnesses (the agent tools installed in the box)")
	index := map[string]int{}
	for i, m := range f.Harnesses {
		index[m.ID] = i + 1
		fmt.Fprintf(p.Out, "  %d. %-12s %s, %s\n", i+1, m.Name, m.Install.Version, m.Verified.Status)
	}
	want := o.Harnesses
	if len(want) == 0 {
		want = a.Harnesses
	}
	var nums []string
	for _, id := range want {
		if n, ok := index[id]; ok {
			nums = append(nums, strconv.Itoa(n))
		} else if len(o.Harnesses) > 0 {
			return fmt.Errorf("harness %q is not available (verified ones: %s)", id, harnessIDs(f.Harnesses))
		}
	}
	if len(nums) == 0 {
		nums = []string{strconv.Itoa(index["pi"])}
		if index["pi"] == 0 {
			nums = []string{"1"}
		}
	}
	var chosen []string
	_, err := p.Ask("Which? (numbers, separated by commas)", strings.Join(nums, ","), func(s string) error {
		chosen = nil
		seen := map[int]bool{}
		for _, part := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(f.Harnesses) {
				return fmt.Errorf("Type numbers from 1 to %d, such as 1.", len(f.Harnesses))
			}
			if !seen[n] {
				seen[n] = true
				chosen = append(chosen, f.Harnesses[n-1].ID)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.Harnesses = chosen
	return nil
}

// DefaultTools is offered when the config doesn't choose tools yet.
var DefaultTools = []string{tools.PackPrefix + "coding"}

// askTools asks which tool packs and single tools go into the box image.
// "none" means only the tools the harnesses need themselves.
func askTools(f Facts, o Options, a *Answers, p *Prompter) error {
	if f.Catalog == nil {
		return errors.New("internal error: no tool catalogue")
	}
	fmt.Fprintln(p.Out, "\nTools built into the box (the agent can't install programs itself)")
	for _, id := range f.Catalog.PackIDs() {
		pk := f.Catalog.Packs[id]
		fmt.Fprintf(p.Out, "  %-14s %s\n  %-14s (%s)\n", tools.PackPrefix+id, pk.Description, "", strings.Join(pk.Tools, ", "))
	}
	// Tools a chosen harness needs are built in with it anyway, so they aren't offered.
	needed := map[string]bool{}
	for _, m := range f.Harnesses {
		if len(a.Harnesses) == 0 || slices.Contains(a.Harnesses, m.ID) {
			for _, t := range m.Tools {
				needed[t] = true
			}
		}
	}
	var single []string
	for _, id := range f.Catalog.ToolIDs() {
		if !needed[id] {
			single = append(single, id)
		}
	}
	fmt.Fprintf(p.Out, "  Single tools: %s\n", strings.Join(single, ", "))

	// A new config starts with the coding pack. An existing one keeps what it
	// has, so taking every default changes nothing (a config from before
	// tools existed has none, and keeps none until a tool is chosen).
	def := DefaultTools
	switch {
	case o.Tools != nil:
		def = o.Tools
	case a.Tools != nil:
		def = a.Tools
	case f.known:
		def = a.Tools
	}
	var chosen []string
	_, err := p.Ask("Which? (packs and tools, separated by commas, or none)", toolsText(def), func(s string) error {
		chosen = []string{}
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			return nil
		}
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" && !slices.Contains(chosen, part) {
				chosen = append(chosen, part)
			}
		}
		_, err := f.Catalog.Resolve(chosen, nil)
		return err
	})
	if err != nil {
		return err
	}
	if len(chosen) == 0 && a.Tools == nil && o.Tools == nil && f.known {
		return nil // still not set, as before
	}
	a.Tools = chosen
	return nil
}

func toolsText(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ",")
}

func harnessIDs(list []harness.Manifest) string {
	var ids []string
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

func askNetwork(f Facts, o Options, a *Answers, p *Prompter, fromProfile bool) error {
	fmt.Fprintln(p.Out, "\nNetwork for the agent:")
	fmt.Fprintln(p.Out, "  airgap  nothing at all, not even the model (for tests, builds and linters)")
	fmt.Fprintln(p.Out, "  off     the model only, no internet")
	fmt.Fprintln(p.Out, "  allow   the model and only the hosts you list, such as registry.npmjs.org")
	fmt.Fprintln(p.Out, "  on      the model and the public internet; local addresses stay blocked")
	fmt.Fprintln(p.Out, "  Change it any time with: ezbx net airgap|off|allow|on")
	def := o.Network
	if def == "" {
		def = a.Network
	}
	mode, err := p.Ask("Network", def, func(s string) error {
		if config.NetworkMode(s) {
			return nil
		}
		return errors.New("Type airgap, off, allow or on.")
	})
	if err != nil {
		return err
	}
	a.Network = mode
	if mode != "allow" {
		if fromProfile {
			a.Allow = nil
		}
		return nil
	}
	var theirs []string
	for _, m := range f.Harnesses {
		for _, id := range a.Harnesses {
			if m.ID == id {
				theirs = append(theirs, m.Network.Domains...)
			}
		}
	}
	if len(theirs) > 0 {
		sort.Strings(theirs)
		fmt.Fprintln(p.Out, "  Allowed for the chosen harnesses anyway:", strings.Join(theirs, ", "))
	}
	ans, err := p.Ask("More hosts to allow (separated by commas, blank for none)", strings.Join(a.Allow, ","), func(s string) error {
		for _, h := range splitList(s) {
			if !config.HostName(h) {
				return fmt.Errorf("%q is not a host name such as registry.npmjs.org or *.example.com.", h)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.Allow = splitList(ans)
	return nil
}

func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		h := strings.ToLower(strings.TrimSpace(part))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Changes lists what differs between the existing config and the answers,
// for the review screen.
func Changes(old, new Answers) []string {
	var out []string
	add := func(name, a, b string) {
		if a != b {
			if a == "" {
				a = "(not set)"
			}
			if b == "" {
				b = "(not set)"
			}
			out = append(out, fmt.Sprintf("%-10s %s -> %s", name, a, b))
		}
	}
	add("box", old.Box, new.Box)
	if old.Workspace == "" && new.Workspace != "" && old.Box == new.Box {
		out = append(out, fmt.Sprintf("%-10s %s (added beside the other boxes)", "new box", new.Box))
	}
	add("engine", old.Engine.URL, new.Engine.URL)
	add("api", old.Engine.API, new.Engine.API)
	add("key_env", old.Engine.KeyEnv, new.Engine.KeyEnv)
	add("host", old.Engine.Host, new.Engine.Host)
	add("harnesses", strings.Join(old.Harnesses, ","), strings.Join(new.Harnesses, ","))
	add("tools", toolsSetting(old.Tools), toolsSetting(new.Tools))
	add("workspace", old.Workspace, new.Workspace)
	add("readonly", readOnlySetting(old.ReadOnly), readOnlySetting(new.ReadOnly))
	add("network", old.Network, new.Network)
	add("allow", strings.Join(old.Allow, ","), strings.Join(new.Allow, ","))
	add("memory", old.Memory, new.Memory)
	for _, id := range sortedKeys(mergedKeys(old.Models, new.Models)) {
		add(id, modelText(old.Models[id]), modelText(new.Models[id]))
	}
	return out
}

// toolsSetting tells "not set" (no tools key) from an empty list.
func toolsSetting(list []string) string {
	if list == nil {
		return ""
	}
	return toolsText(list)
}

func mergedKeys[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range maps.Keys(a) {
		out[k] = true
	}
	for k := range maps.Keys(b) {
		out[k] = true
	}
	return out
}

func modelText(m config.Model) string {
	var parts []string
	if m.Reasoning != nil {
		parts = append(parts, "reasoning "+strconv.FormatBool(*m.Reasoning))
	}
	if m.MaxTokens > 0 {
		parts = append(parts, "max_tokens "+strconv.Itoa(m.MaxTokens))
	}
	if m.ContextWindow > 0 {
		parts = append(parts, "context "+strconv.Itoa(m.ContextWindow))
	}
	if m.Name != "" {
		parts = append(parts, "name "+m.Name)
	}
	return strings.Join(parts, ", ")
}
