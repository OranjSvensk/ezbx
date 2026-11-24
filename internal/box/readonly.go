package box

import (
	"fmt"
	"path/filepath"

	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/readonly"
)

// ReadOnlyEntry is one [[box.<name>.readonly]] from the config.
type ReadOnlyEntry struct {
	Path, Name   string
	Mask, Unmask []string
}

// ReadOnlyFolder is a read-only folder after its checks: a clean full path,
// its name under /readonly, and what to mask in it.
type ReadOnlyFolder struct {
	Path    string
	Name    string
	Matcher readonly.Matcher
}

// ResolveReadOnly expands and checks each folder (docs/readonly.md, R2, R3,
// R6). ws must already have passed CheckWorkspace.
func ResolveReadOnly(entries []ReadOnlyEntry, ws, home string, own, system []string) ([]ReadOnlyFolder, error) {
	var out []ReadOnlyFolder
	names := map[string]string{}
	for _, e := range entries {
		p := paths.ExpandHome(e.Path, home)
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("read-only folder %q must be a full path, or start with ~", e.Path)
		}
		p = filepath.Clean(p)
		if err := checkReadOnly(p, ws, home, own, system); err != nil {
			return nil, err
		}
		for _, f := range out {
			if paths.Within(p, f.Path) || paths.Within(f.Path, p) {
				return nil, fmt.Errorf("read-only folders %s and %s overlap; keep one of them", f.Path, p)
			}
		}
		name := e.Name
		if name == "" {
			name = readonly.DefaultName(p)
			if err := readonly.CheckName(name); err != nil {
				return nil, fmt.Errorf("read-only folder %s: its %v; set name for it in ezbx.toml", p, err)
			}
		}
		if prev, ok := names[name]; ok {
			return nil, fmt.Errorf("read-only folders %s and %s would both be /readonly/%s; set name on one of them", prev, p, name)
		}
		names[name] = p
		m, err := readonly.NewMatcher(e.Mask, e.Unmask)
		if err != nil {
			return nil, fmt.Errorf("read-only folder %s: %v", p, err)
		}
		out = append(out, ReadOnlyFolder{Path: p, Name: name, Matcher: m})
	}
	return out, nil
}

// checkReadOnly refuses what the workspace refuses, an overlap with the
// workspace, and a folder that is itself a default folder mask (~/.ssh).
func checkReadOnly(p, ws, home string, own, system []string) error {
	if filepath.Dir(p) == p {
		return fmt.Errorf("read-only folder %s is the root of a drive; choose a folder inside it", p)
	}
	if home != "" && paths.Within(home, p) {
		return fmt.Errorf("read-only folder %s is your home folder or holds it; choose a folder inside it", p)
	}
	for _, d := range own {
		if d != "" && (paths.Within(p, d) || paths.Within(d, p)) {
			return fmt.Errorf("read-only folder %s overlaps ezbx's own folder %s, which holds the admin token; choose another folder", p, d)
		}
	}
	for _, d := range system {
		if d != "" && (paths.Within(p, d) || paths.Within(d, p)) {
			return fmt.Errorf("read-only folder %s is a system folder or holds one (%s); choose a folder in your home", p, d)
		}
	}
	if paths.Within(p, ws) || paths.Within(ws, p) {
		return fmt.Errorf("read-only folder %s overlaps the workspace %s; the agent can already read the workspace", p, ws)
	}
	defaults, _ := readonly.NewMatcher(nil, nil)
	if pat, ok := defaults.Match(filepath.ToSlash(p), true); ok {
		return fmt.Errorf("read-only folder %s would be masked whole by the default %q; it holds credentials, so it can't be shown to the agent", p, pat)
	}
	return nil
}
