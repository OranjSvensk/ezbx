package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OranjSvensk/ezbx/internal/readonly"
)

// readOnlyInside is where read-only folders appear in the box.
const readOnlyInside = "/readonly"

// ReadOnlyMount is one read-only folder as the box gets it: the folder, the
// patterns it was masked with (so ezbx shell can walk it again the same
// way), and what was masked.
type ReadOnlyMount struct {
	Name   string          `json:"name"`
	Path   string          `json:"path"`
	Mask   []string        `json:"mask,omitempty"`
	Unmask []string        `json:"unmask,omitempty"`
	Masks  []readonly.Mask `json:"masks"`
}

// Inside is the folder's path in the box.
func (m ReadOnlyMount) Inside() string { return readOnlyInside + "/" + m.Name }

// readOnlyArgs mounts each folder read-only, then covers each mask: a file
// with /dev/null (Docker's VM's own: reads give nothing, writes are thrown
// away), a folder with an empty read-only tmpfs. Links aren't covered: in
// the box a link points inside the box, never to the host, and whatever it
// names in the folder is masked by its own name (docs/readonly.md).
func (p Plan) readOnlyArgs() []string {
	var a []string
	for _, f := range p.ReadOnly {
		a = append(a, "--mount", mountSpec("type=bind", "src="+f.Path, "dst="+f.Inside(), "readonly"))
		for _, m := range f.Masks {
			dst := path.Join(f.Inside(), m.Rel)
			switch {
			case m.Link:
			case m.Dir:
				a = append(a, "--mount", mountSpec("type=tmpfs", "dst="+dst, "tmpfs-size=4k", "tmpfs-mode=0555", "readonly"))
			default:
				a = append(a, "--mount", mountSpec("type=bind", "src=/dev/null", "dst="+dst, "readonly"))
			}
		}
	}
	return a
}

// mountSpec joins --mount fields. Docker reads the value as one CSV line,
// so a field holding a comma or a quote is quoted, its quotes doubled.
func mountSpec(fields ...string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		if strings.ContainsAny(f, `,"`) {
			f = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		}
		out[i] = f
	}
	return strings.Join(out, ",")
}

// MasksFileIn is the record of what a running box's read-only folders mask.
func MasksFileIn(stateDir string) string { return filepath.Join(stateDir, "masks.json") }

// WriteMasks records the read-only folders of a box being started.
func WriteMasks(stateDir string, ms []ReadOnlyMount) error {
	data, err := json.MarshalIndent(ms, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(MasksFileIn(stateDir), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("recording the read-only folders: %w", err)
	}
	return nil
}

// ReadMasks reads the record. ok is false when there is none: the box has
// no read-only folders.
func ReadMasks(stateDir string) (ms []ReadOnlyMount, ok bool, err error) {
	data, err := os.ReadFile(MasksFileIn(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, false, fmt.Errorf("the record of read-only folders (%s) is broken: %v. Run ezbx down, then ezbx up", MasksFileIn(stateDir), err)
	}
	return ms, true, nil
}

// RemoveMasks removes the record, when the box goes or has no read-only
// folders.
func RemoveMasks(stateDir string) error {
	if err := os.Remove(MasksFileIn(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the record of read-only folders: %w", err)
	}
	return nil
}

// Unmasked walks a recorded folder again with the same patterns and lists
// what now matches a mask but wasn't masked when the box started: a secret
// file added since, which the box can read (docs/readonly.md, R5). The walk
// is returned too, for a content check.
func Unmasked(m ReadOnlyMount) ([]string, readonly.Walked, error) {
	matcher, err := readonly.NewMatcher(m.Mask, m.Unmask)
	if err != nil {
		return nil, readonly.Walked{}, err
	}
	w, err := readonly.Walk(m.Path, matcher)
	if err != nil {
		return nil, w, err
	}
	had := map[string]bool{}
	for _, mk := range m.Masks {
		had[strings.ToLower(mk.Rel)] = true
	}
	var out []string
	for _, mk := range w.Masks {
		if !had[strings.ToLower(mk.Rel)] {
			out = append(out, mk.Rel)
		}
	}
	sort.Strings(out)
	return out, w, nil
}
