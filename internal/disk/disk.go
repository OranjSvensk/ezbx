// Package disk reports free disk space, for the warning ezbx init shows
// before it builds images. See docs/init-flow.md, step 0.
package disk

import (
	"errors"
	"os"
	"path/filepath"
)

// Free is the space this user may still write on the drive that holds path.
// A path that doesn't exist yet (a folder init will create) is measured at
// the nearest folder above it that does.
func Free(path string) (uint64, error) {
	p := filepath.Clean(path)
	for {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return free(p)
		}
		up := filepath.Dir(p)
		if up == p {
			return 0, errors.New("no existing folder above " + path)
		}
		p = up
	}
}
