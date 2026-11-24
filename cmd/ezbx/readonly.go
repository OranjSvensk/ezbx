package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/doctor"
	"github.com/OranjSvensk/ezbx/internal/readonly"
	"github.com/OranjSvensk/ezbx/internal/setup"
)

// readOnlyEntries hands the config's read-only folders to the box package.
func readOnlyEntries(b *config.Box) []box.ReadOnlyEntry {
	var out []box.ReadOnlyEntry
	for _, r := range b.ReadOnly {
		out = append(out, box.ReadOnlyEntry{Path: r.Path, Name: r.Name, Mask: r.Mask, Unmask: r.Unmask})
	}
	return out
}

// readOnlyPlan checks and walks the box's read-only folders and works out
// their masks. It warns about files that look like secrets but aren't
// masked, defaults shown with unmask, links and folders it couldn't list.
// It never prints what a file holds.
func readOnlyPlan(b *config.Box, ws, home string, own []string, stdout io.Writer) ([]box.ReadOnlyMount, error) {
	if len(b.ReadOnly) == 0 {
		return nil, nil
	}
	folders, err := box.ResolveReadOnly(readOnlyEntries(b), ws, home, own, systemDirs())
	if err != nil {
		return nil, err
	}
	var out []box.ReadOnlyMount
	var all [][]readonly.Mask
	for _, f := range folders {
		if st, err := os.Stat(f.Path); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("read-only folder %s doesn't exist or isn't a folder", f.Path)
		}
		w, err := readonly.Walk(f.Path, f.Matcher)
		if err != nil {
			if errors.Is(err, readonly.ErrTooBig) {
				return nil, err
			}
			return nil, fmt.Errorf("read-only folder %s: %w", f.Path, err)
		}
		all = append(all, w.Masks)
		m := box.ReadOnlyMount{Name: f.Name, Path: f.Path, Mask: f.Mask, Unmask: f.Unmask, Masks: w.Masks}
		out = append(out, m)

		inside := m.Inside()
		for _, u := range f.Unmask {
			fmt.Fprintf(stdout, "note: %s shows files matching %q, a default mask, because of unmask\n", inside, u)
		}
		for _, mk := range w.Masks {
			if mk.Link {
				fmt.Fprintf(stdout, "note: %s is a link; in the box it points inside the box, never to this computer, so it isn't masked\n", path.Join(inside, mk.Rel))
			}
		}
		for _, rel := range w.Unreadable {
			fmt.Fprintf(stdout, "note: couldn't list %s, so the box can't read it either\n", path.Join(inside, rel))
		}
		for _, fd := range readonly.Scan(f.Path, w.Scan) {
			fmt.Fprintf(stdout, "warning: %s looks like it holds %s, and the agent can read it. Add it to mask for this folder in ezbx.toml, or move it\n", path.Join(inside, fd.Rel), fd.Kind)
		}
		fmt.Fprintf(stdout, "read-only: %s at %s, %d masked\n", f.Path, inside, readonly.Covered(w.Masks))
	}
	if err := readonly.CheckMaskCount(all...); err != nil {
		return nil, err
	}
	return out, nil
}

// checkMasks refuses a session when a read-only folder now holds a file
// matching a mask that wasn't there when the box started: the box can read
// it until ezbx down and ezbx up (docs/readonly.md, R5).
func checkMasks(stateDir string) error {
	ms, ok, err := box.ReadMasks(stateDir)
	if err != nil || !ok {
		return err
	}
	var found []string
	for _, m := range ms {
		names, _, err := box.Unmasked(m)
		if err != nil {
			return fmt.Errorf("checking read-only folder %s: %w", m.Path, err)
		}
		for _, n := range names {
			found = append(found, path.Join(m.Inside(), n))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("these files match a mask but appeared after the box started, so the agent can read them:\n  %s\nThe session wasn't started. Run ezbx down, then ezbx up, to mask them", strings.Join(found, "\n  "))
}

// doctorReadOnly gives the doctor the box's recorded read-only folders, each
// walked again for new secret files and secret-looking contents.
func doctorReadOnly(stateDir string) ([]doctor.ReadOnlyFolder, string) {
	ms, ok, err := box.ReadMasks(stateDir)
	if err != nil {
		return nil, err.Error()
	}
	if !ok {
		return nil, ""
	}
	var out []doctor.ReadOnlyFolder
	for _, m := range ms {
		inside := m.Inside()
		f := doctor.ReadOnlyFolder{Inside: inside, Unmask: m.Unmask}
		for _, mk := range m.Masks {
			switch {
			case mk.Link:
			case mk.Dir:
				f.Dirs = append(f.Dirs, path.Join(inside, mk.Rel))
			default:
				f.Files = append(f.Files, path.Join(inside, mk.Rel))
			}
		}
		names, w, err := box.Unmasked(m)
		if err != nil {
			f.Error = err.Error()
		} else {
			for _, n := range names {
				f.New = append(f.New, path.Join(inside, n))
			}
			for _, fd := range readonly.Scan(m.Path, w.Scan) {
				f.Findings = append(f.Findings, path.Join(inside, fd.Rel)+" looks like it holds "+fd.Kind)
			}
		}
		out = append(out, f)
	}
	return out, ""
}

// readOnlyNetWarning warns, after the network was opened, that the agent
// could send what it reads in the running box's read-only folders out
// (docs/readonly.md, R7). Nothing is refused.
func readOnlyNetWarning(t target, mode string, w io.Writer) {
	ms, _, err := box.ReadMasks(t.dirs.Box(t.name))
	if err != nil {
		return
	}
	if msg := setup.ReadOnlyNetWarning(mode, len(ms)); msg != "" {
		var at []string
		for _, m := range ms {
			at = append(at, m.Inside())
		}
		fmt.Fprintf(w, "warning: %s (%s).\n", msg, strings.Join(at, ", "))
	}
}
