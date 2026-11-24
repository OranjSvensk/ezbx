package readonly

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// Limits keep a mistaken large folder from stalling ezbx up. Variables only
// so tests can lower them.
var (
	MaxEntries = 200_000 // files and folders walked in one read-only folder
	MaxMasks   = 256     // masks in one box, all folders together
)

// MaxScanSize is the largest file checked for secret-looking contents.
const MaxScanSize = 1 << 20

// Mask is one name the box will cover.
type Mask struct {
	Rel     string `json:"rel"`            // inside the folder, with / between parts
	Dir     bool   `json:"dir,omitempty"`  // a folder (covered by an empty folder), else a file
	Link    bool   `json:"link,omitempty"` // a link or Windows junction, left as it is
	Pattern string `json:"pattern"`        // the pattern that matched
}

// Walked is what a walk of one folder found.
type Walked struct {
	Masks      []Mask
	Scan       []string // regular files, not masked, small enough to check
	Unreadable []string // folders the walk couldn't list
	Entries    int
}

// ErrTooBig is returned when a folder holds more than MaxEntries entries.
var ErrTooBig = errors.New("too many files")

// Walk lists what m masks in root. It never follows links or junctions, and
// doesn't go into masked folders: what is under them is hidden anyway.
func Walk(root string, m Matcher) (Walked, error) {
	var w Walked
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root {
			return err // the folder itself must be readable
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			// A folder that can't be listed here can't be read by the box
			// either (Docker reads it as you), but say so.
			w.Unreadable = append(w.Unreadable, rel)
			return fs.SkipDir
		}
		w.Entries++
		if w.Entries > MaxEntries {
			return fmt.Errorf("%s holds more than %d files and folders: %w; choose a narrower folder", root, MaxEntries, ErrTooBig)
		}
		link := d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0
		if link {
			// A link may stand for a file or a folder, so either kind of
			// pattern masks it.
			if pat, ok := m.Match(rel, true); ok {
				w.Masks = append(w.Masks, Mask{Rel: rel, Link: true, Pattern: pat})
			} else if pat, ok := m.Match(rel, false); ok {
				w.Masks = append(w.Masks, Mask{Rel: rel, Link: true, Pattern: pat})
			}
			return nil
		}
		if pat, ok := m.Match(rel, d.IsDir()); ok {
			w.Masks = append(w.Masks, Mask{Rel: rel, Dir: d.IsDir(), Pattern: pat})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil && info.Size() <= MaxScanSize {
				w.Scan = append(w.Scan, rel)
			}
		}
		return nil
	})
	sort.Slice(w.Masks, func(i, j int) bool { return w.Masks[i].Rel < w.Masks[j].Rel })
	return w, err
}

// CheckMaskCount refuses more than MaxMasks masks in all, naming the pattern
// with the most matches so its parent folder can be masked instead.
func CheckMaskCount(all ...[]Mask) error {
	n := 0
	count := map[string]int{}
	for _, ms := range all {
		n += len(ms)
		for _, mk := range ms {
			count[mk.Pattern]++
		}
	}
	if n <= MaxMasks {
		return nil
	}
	top, most := "", 0
	for p, c := range count {
		if c > most || (c == most && p < top) {
			top, most = p, c
		}
	}
	return fmt.Errorf("the read-only folders need %d masks, more than %d; %q matches %d of them, so mask the folder that holds those files instead", n, MaxMasks, top, most)
}

// Covered counts the masks that hide something: links are left as they are
// (docs/readonly.md, "Links are left as they are").
func Covered(ms []Mask) int {
	n := 0
	for _, m := range ms {
		if !m.Link {
			n++
		}
	}
	return n
}
