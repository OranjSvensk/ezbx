package setup

import (
	"fmt"
	"sort"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/harness"
)

// Plan is what the review screen shows and what apply does.
type Plan struct {
	Result     Result
	Old        *Answers // the existing config, if any
	ConfigFile string
	StateDir   string
	Images     []Image  // the gateway and box images this config needs
	Tools      []string // the tool ids chosen for the box (the harnesses' own are always added)
	Harnesses  []harness.Manifest
	NoStart    bool   // --no-start
	BoxUp      bool   // the box's containers exist already
	KeyEnvSet  bool   // the engine's key variable has a value
	Workspace  bool   // the workspace folder exists already
	Disks      []Disk // free space where the images and snapshots go
}

// Disk is the free space on one drive init will use.
type Disk struct {
	What string // what lives there, such as "the state folder"
	Path string
	Free uint64
	Err  error // the free space couldn't be read
}

// Sizes behind the disk estimate, from the image sizes in
// docs/isolation-test.md: the box image with Pi and pack:coding is about
// 726 MB, and each more harness adds about 600 MB (4.28 GB for all seven).
// A build needs about as much again for Docker's build cache, and snapshots
// need room of their own.
const (
	firstHarness = 750 << 20
	moreHarness  = 600 << 20
	headroom     = 1 << 30
)

// DiskNeed estimates the free space the plan needs on each drive: room to
// build the box image twice over when it isn't built yet, plus room for
// snapshots.
func (p Plan) DiskNeed() uint64 {
	need := uint64(headroom)
	if start, _ := p.Start(); start && len(p.MissingImages()) > 0 {
		n := uint64(len(p.Result.Answers.Harnesses))
		if n == 0 {
			n = 1
		}
		need += 2 * (firstHarness + (n-1)*moreHarness)
	}
	return need
}

// LowDisks lists the drives with less free space than DiskNeed.
func (p Plan) LowDisks() []Disk {
	var low []Disk
	for _, d := range p.Disks {
		if d.Err == nil && d.Free < p.DiskNeed() {
			low = append(low, d)
		}
	}
	return low
}

// Size writes a number of bytes the way people read them: 1.5 GB, 820 MB.
func Size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%d MB", n>>20)
	}
}

// Changed reports whether the config file would be written.
func (p Plan) Changed() bool { return p.Old == nil || len(Changes(*p.Old, p.Result.Answers)) > 0 }

// Image is one image the config needs, and whether it is built already.
type Image struct {
	Tag     string
	Present bool
}

// MissingImages lists the images that aren't built yet. Apply builds them
// before starting the box.
func (p Plan) MissingImages() []string {
	var out []string
	for _, img := range p.Images {
		if !img.Present {
			out = append(out, img.Tag)
		}
	}
	return out
}

// Start reports whether apply will run ezbx up, and if not, why.
func (p Plan) Start() (bool, string) {
	switch {
	case p.NoStart:
		return false, "--no-start was given"
	case p.Result.Engine == nil:
		return false, "no model server was chosen; start it, then run ezbx up"
	case len(p.Result.Engine.Models) == 0:
		return false, "the server lists no models; load one, then run ezbx up"
	case p.BoxUp && p.Changed():
		return false, "the box is up; to apply the changes, run ezbx down, then ezbx up"
	case p.BoxUp:
		return false, "the box is already up"
	case p.keyEnv() != "" && !p.KeyEnvSet:
		return false, "the key variable " + p.keyEnv() + " is empty; set it, then run ezbx up"
	}
	return true, ""
}

func (p Plan) keyEnv() string {
	if p.Result.Engine == nil {
		return ""
	}
	return p.Result.Answers.Engine.KeyEnv
}

// Review is the screen shown before anything changes.
func (p Plan) Review() string {
	a := p.Result.Answers
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "%-13s %s\n", k, v) }
	b.WriteString("\nezbx plan (nothing has been changed yet)\n\n")

	if e := p.Result.Engine; e != nil {
		use := e.Name + " at " + e.URL
		if a.Engine.API == config.DefaultAPI {
			use += ", API checked again at every ezbx up"
		}
		row("Engine", fmt.Sprintf("%s, %d model(s)", use, len(e.Models)))
		var models []string
		for _, m := range e.Models {
			t := m.ID
			if r := a.Models[m.ID].Reasoning; r != nil && *r {
				t += " (reasoning)"
			}
			models = append(models, t)
		}
		if len(models) > 0 {
			row("Models", strings.Join(models, ", "))
		}
		if k := p.keyEnv(); k != "" {
			state := "set"
			if !p.KeyEnvSet {
				state = "EMPTY"
			}
			row("Engine key", "from the environment variable "+k+" ("+state+")")
		}
	} else {
		switch {
		case a.Engine.URL == "":
			row("Engine", "none chosen yet (set url under [engine] before ezbx up)")
		default:
			row("Engine", a.Engine.URL+", "+a.Engine.API+" API (not answering now)")
		}
	}
	var hs []string
	for _, id := range a.Harnesses {
		for _, m := range p.Harnesses {
			if m.ID == id {
				hs = append(hs, fmt.Sprintf("%s %s (%s)", m.Name, m.Install.Version, m.Verified.Status))
			}
		}
	}
	row("Harnesses", strings.Join(hs, ", "))
	tl := "none"
	if len(p.Tools) > 0 {
		tl = strings.Join(p.Tools, ", ")
	}
	if len(a.Tools) > 0 {
		tl += "   (from " + strings.Join(a.Tools, ", ") + ")"
	}
	row("Tools", tl)
	ws := a.Workspace
	if !p.Workspace {
		ws += "   (will be created)"
	}
	row("Workspace", ws)
	if len(p.Result.ReadOnly) == 0 {
		row("Read-only", "none")
	}
	for i, r := range p.Result.ReadOnly {
		k := ""
		if i == 0 {
			k = "Read-only"
		}
		line := fmt.Sprintf("%s at %s, %d hidden", r.Path, r.Inside, r.Masked)
		if len(r.Unmask) > 0 {
			line += ", shown after all: " + strings.Join(r.Unmask, ", ")
		}
		row(k, line)
	}
	net := a.Network
	if a.Network == "allow" {
		allow := append([]string(nil), a.Allow...)
		sort.Strings(allow)
		net += ": " + strings.Join(append(allow, "(and the harnesses' own hosts)"), ", ")
	}
	row("Network", net)
	if w := ReadOnlyNetWarning(a.Network, len(p.Result.ReadOnly)); w != "" {
		row("", "warning: "+w)
	}
	row("Memory", a.Memory)
	for i, d := range p.Disks {
		k := ""
		if i == 0 {
			k = "Disk"
		}
		if d.Err != nil {
			row(k, fmt.Sprintf("free space unknown at %s, %s", d.Path, d.What))
		} else {
			row(k, fmt.Sprintf("%s free at %s, %s", Size(d.Free), d.Path, d.What))
		}
	}
	if low := p.LowDisks(); len(low) > 0 {
		fmt.Fprintf(&b, "\nWarning: about %s of free space is needed (images, Docker's build cache and snapshots), and %s has only %s.\n", Size(p.DiskNeed()), low[0].Path, Size(low[0].Free))
		b.WriteString("Free some space first. docker system df shows what Docker keeps, and ezbx build --prune removes ezbx's old images.\n")
	}
	if e := p.Result.Engine; e != nil {
		if w := detect.FileNamedWarning(*e); w != "" {
			fmt.Fprintf(&b, "\nWarning: %s, then run ezbx init again.\n", w)
		}
	}

	b.WriteString("\nWill create or change\n")
	switch {
	case p.Old == nil:
		fmt.Fprintf(&b, "  config file   %s (new)\n", p.ConfigFile)
	case p.Changed():
		fmt.Fprintf(&b, "  config file   %s (the current one is kept as %s.previous)\n", p.ConfigFile, p.ConfigFile)
		for _, c := range Changes(*p.Old, a) {
			fmt.Fprintf(&b, "                %s\n", c)
		}
	default:
		fmt.Fprintf(&b, "  config file   %s (no changes)\n", p.ConfigFile)
	}
	fmt.Fprintf(&b, "  state folder  %s (admin token, logs, approved hosts)\n", p.StateDir)
	ok, why := p.Start()
	var tags []string
	for _, img := range p.Images {
		tags = append(tags, img.Tag)
	}
	switch miss := p.MissingImages(); {
	case len(miss) == 0:
		fmt.Fprintf(&b, "  images        %s (built already)\n", strings.Join(tags, " and "))
	case ok:
		fmt.Fprintf(&b, "  images        ezbx build: %s (downloads from the internet once; the box image is several hundred MB)\n", strings.Join(miss, " and "))
	default:
		fmt.Fprintf(&b, "  images        not built yet: %s. ezbx build makes them (init does, when it starts the box)\n", strings.Join(miss, " and "))
	}
	if ok {
		b.WriteString("  then          ezbx up (2 networks, 1 volume, 2 containers), then ezbx doctor\n")
	} else {
		fmt.Fprintf(&b, "  then          nothing is started: %s\n", why)
	}
	return b.String()
}
