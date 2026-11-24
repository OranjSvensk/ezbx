package tools

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/config"
)

// Profile is a named starter preset of harnesses, tools and network rules
// that ezbx init offers for a new box.
type Profile struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Harnesses   []string `json:"harnesses"`
	Tools       []string `json:"tools"`
	Network     string   `json:"network"`
	Allow       []string `json:"allow"`
}

func (c *Catalog) loadProfiles(fsys fs.FS) error {
	profileFiles, err := fs.Glob(fsys, "profiles/*.json")
	if err != nil {
		return err
	}
	for _, f := range profileFiles {
		var p Profile
		if err := decode(fsys, f, &p); err != nil {
			return err
		}
		if err := p.check(strings.TrimSuffix(path.Base(f), ".json"), c); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		c.Profiles[p.ID] = p
	}
	return nil
}

func (p Profile) check(file string, c *Catalog) error {
	switch {
	case p.Schema != 1:
		return fmt.Errorf("schema must be 1")
	case !idPattern.MatchString(p.ID):
		return fmt.Errorf("id %q must be lowercase letters, digits and dashes", p.ID)
	case p.ID != file:
		return fmt.Errorf("id %q must match the file name", p.ID)
	case p.Name == "" || p.Description == "":
		return fmt.Errorf("name and description are required")
	case len(p.Harnesses) == 0:
		return fmt.Errorf("a profile needs at least one harness")
	case p.Tools == nil:
		return fmt.Errorf("tools list is required (use [] for none)")
	}
	seenHarness := map[string]bool{}
	for _, h := range p.Harnesses {
		if !idPattern.MatchString(h) {
			return fmt.Errorf("harness %q must be lowercase letters, digits and dashes", h)
		}
		if seenHarness[h] {
			return fmt.Errorf("harness %q is listed twice", h)
		}
		seenHarness[h] = true
	}
	if _, err := c.Resolve(p.Tools, nil); err != nil {
		return err
	}
	switch p.Network {
	case "off", "allow", "on":
	default:
		return fmt.Errorf("network %q: use off, allow or on", p.Network)
	}
	if p.Network != "allow" && len(p.Allow) > 0 {
		return fmt.Errorf("network %q cannot have an allow list", p.Network)
	}
	seenAllow := map[string]bool{}
	for _, h := range p.Allow {
		if strings.ToLower(h) != h {
			return fmt.Errorf("allow host %q must be lowercase", h)
		}
		if !config.HostName(h) {
			return fmt.Errorf("allow host %q is not a host name such as registry.npmjs.org or *.example.com", h)
		}
		if seenAllow[h] {
			return fmt.Errorf("allow host %q is listed twice", h)
		}
		seenAllow[h] = true
	}
	return nil
}

// ProfileIDs lists every starter profile id, keeping offline first as the
// default and sorting the rest alphabetically.
func (c *Catalog) ProfileIDs() []string {
	ids := sortedKeys(c.Profiles)
	var out []string
	hasOffline := false
	for _, id := range ids {
		if id == "offline" {
			hasOffline = true
			continue
		}
		out = append(out, id)
	}
	if hasOffline {
		return append([]string{"offline"}, out...)
	}
	return out
}
