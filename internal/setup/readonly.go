package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/readonly"
)

// ReadOnlySummary is one read-only folder as the review screen shows it.
type ReadOnlySummary struct {
	Path, Inside string
	Masked       int
	Unmask       []string
}

// askReadOnly asks for read-only folders after the workspace: none by
// default. Each is checked as ezbx up will check it, walked, and its files
// that look like secrets are offered for masking, one by one (yes by
// default, so --yes masks them). Settings init doesn't ask about (name,
// unmask, earlier masks) are kept for folders that stay.
func askReadOnly(f Facts, o Options, a *Answers, p *Prompter, res *Result) error {
	var list []config.ReadOnly
	if o.ReadOnly != nil {
		for _, s := range o.ReadOnly {
			list = append(list, keepEntry(a.ReadOnly, s, f.Home))
		}
	} else {
		fmt.Fprintln(p.Out, "\nRead-only folders are folders the agent may read but never change, at /readonly/<name> (none by default). Common secret files in them, such as .env files and keys, are hidden.")
		for _, r := range a.ReadOnly {
			keep, err := p.YesNo("Keep read-only folder "+r.Path+"?", true)
			if err != nil {
				return err
			}
			if keep {
				list = append(list, r)
			}
		}
		for {
			s, err := p.Ask("Add a read-only folder (Enter for none)", "", func(s string) error {
				if strings.TrimSpace(s) == "" {
					return nil
				}
				_, err := resolve(append(slices.Clone(list), config.ReadOnly{Path: s}), a.Workspace, f)
				return err
			})
			if err != nil {
				return err
			}
			if strings.TrimSpace(s) == "" {
				break
			}
			list = append(list, keepEntry(nil, strings.TrimSpace(s), f.Home))
		}
	}
	folders, err := resolve(list, a.Workspace, f)
	if err != nil {
		return err
	}
	var all [][]readonly.Mask
	res.ReadOnly = nil
	for i, fo := range folders {
		w, err := readonly.Walk(fo.Path, fo.Matcher)
		if err != nil {
			return fmt.Errorf("read-only folder %s: %w", fo.Path, err)
		}
		masked := readonly.Covered(w.Masks)
		// A file masked already isn't scanned, so nothing is asked twice.
		for _, fd := range readonly.Scan(fo.Path, w.Scan) {
			pat := "./" + fd.Rel
			yes, err := p.YesNo(fmt.Sprintf("%s looks like it holds %s. Hide it from the agent?", filepath.Join(fo.Path, filepath.FromSlash(fd.Rel)), fd.Kind), true)
			if err != nil {
				return err
			}
			if yes {
				list[i].Mask = append(list[i].Mask, pat)
				masked++
			}
		}
		all = append(all, w.Masks)
		res.ReadOnly = append(res.ReadOnly, ReadOnlySummary{Path: fo.Path, Inside: "/readonly/" + fo.Name, Masked: masked, Unmask: fo.Unmask})
	}
	if err := readonly.CheckMaskCount(all...); err != nil {
		return err
	}
	a.ReadOnly = list
	return nil
}

// keepEntry is the existing entry for path, if there is one, else a new one.
// Paths are written as full paths.
func keepEntry(had []config.ReadOnly, s, home string) config.ReadOnly {
	full := filepath.Clean(paths.ExpandHome(s, home))
	for _, r := range had {
		if filepath.Clean(paths.ExpandHome(r.Path, home)) == full {
			return r
		}
	}
	return config.ReadOnly{Path: full}
}

// resolve checks the folders the way ezbx up does, and that each exists.
func resolve(list []config.ReadOnly, ws string, f Facts) ([]box.ReadOnlyFolder, error) {
	var entries []box.ReadOnlyEntry
	for _, r := range list {
		entries = append(entries, box.ReadOnlyEntry{Path: strings.TrimSpace(r.Path), Name: r.Name, Mask: r.Mask, Unmask: r.Unmask})
	}
	folders, err := box.ResolveReadOnly(entries, ws, f.Home, f.Own, f.System)
	if err != nil {
		return nil, err
	}
	for _, fo := range folders {
		if st, err := os.Stat(fo.Path); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("read-only folder %s doesn't exist or isn't a folder", fo.Path)
		}
	}
	return folders, nil
}

// readOnlyText writes the read-only folders as ezbx.toml tables.
func readOnlyText(box string, ro []config.ReadOnly) string {
	var b strings.Builder
	for _, r := range ro {
		fmt.Fprintf(&b, "\n[[box.%s.readonly]]   # the agent may read this folder, never change it\n", box)
		fmt.Fprintf(&b, "path   = %s\n", quote(r.Path))
		if r.Name != "" {
			fmt.Fprintf(&b, "name   = %s\n", quote(r.Name))
		}
		if len(r.Mask) > 0 {
			fmt.Fprintf(&b, "mask   = %s   # hidden, on top of the default masks\n", list(r.Mask))
		}
		if len(r.Unmask) > 0 {
			fmt.Fprintf(&b, "unmask = %s   # default masks shown after all\n", list(r.Unmask))
		}
	}
	return b.String()
}

// readOnlySetting is the folders in one line, for the list of changes.
func readOnlySetting(list []config.ReadOnly) string {
	var parts []string
	for _, r := range list {
		s := r.Path
		if r.Name != "" {
			s += " as " + r.Name
		}
		if len(r.Mask) > 0 {
			s += " mask " + strings.Join(r.Mask, ",")
		}
		if len(r.Unmask) > 0 {
			s += " unmask " + strings.Join(r.Unmask, ",")
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// cloneReadOnly copies the entries, so changing the answers doesn't change
// the config they came from.
func cloneReadOnly(in []config.ReadOnly) []config.ReadOnly {
	var out []config.ReadOnly
	for _, r := range in {
		out = append(out, config.ReadOnly{Path: r.Path, Name: r.Name, Mask: slices.Clone(r.Mask), Unmask: slices.Clone(r.Unmask)})
	}
	return out
}

// ReadOnlyNetWarning says, for a network mode other than off, that what the
// agent reads in read-only folders could be sent out (docs/readonly.md, R7).
// It is "" when there is nothing to warn about.
func ReadOnlyNetWarning(mode string, folders int) string {
	if folders == 0 || mode == "off" || mode == "" {
		return ""
	}
	what := "the public internet"
	if mode == "allow" {
		what = "an allowed host"
	}
	return fmt.Sprintf("with the network %s, the agent could send what it reads in the read-only folders to %s", mode, what)
}
