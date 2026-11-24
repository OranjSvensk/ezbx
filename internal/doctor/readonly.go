package doctor

import (
	"context"
	"fmt"
	"strings"
)

// ReadOnlyFolder is one read-only folder of a running box, as recorded by
// ezbx up, with what the host found in it now.
type ReadOnlyFolder struct {
	Inside   string   // /readonly/<name>
	Files    []string // masked files, full paths in the box
	Dirs     []string // masked folders, full paths in the box
	Unmask   []string // defaults shown on purpose
	Findings []string // "path: kind" for files that look like secrets
	New      []string // match a mask but appeared after ezbx up
	Error    string   // the host couldn't walk the folder again
}

// readOnlyScript runs in the box as the agent. EZBX_RO holds one line per
// check, "R:", "F:" or "D:" and a path: a folder that must refuse writes, a
// file mask (a character device: /dev/null), a folder mask (an empty
// tmpfs that refuses writes). The paths come in the environment, never in
// the script, so a file name can't run as a command. It prints one line per
// problem, then "done".
const readOnlyScript = `probe() { if touch "$1/.ezbx-doctor-probe" 2>/dev/null; then rm -f "$1/.ezbx-doctor-probe"; echo "writable $1"; fi; }
printf '%s\n' "$EZBX_RO" | while IFS= read -r l; do
  [ -n "$l" ] || continue
  k=${l%%:*}; p=${l#*:}
  case $k in
    R) probe "$p" ;;
    F) [ -c "$p" ] || echo "unmasked $p" ;;
    D) if [ "$(stat -f -c %T "$p" 2>/dev/null)" != tmpfs ] || [ -n "$(ls -A "$p" 2>/dev/null)" ]; then echo "unmasked $p"; fi; probe "$p" ;;
  esac
done
echo done`

// readOnlyChecks adds the read-only folder checks for a box container.
func readOnlyChecks(ctx context.Context, r *Report, cfg HostConfig, boxName string, box containerInfo) {
	const g = "Read-only folders"
	if cfg.ReadOnlyNote != "" {
		r.add(g, "record", Warn, cfg.ReadOnlyNote, "")
	}
	mounts := map[string]bool{} // destination -> writable
	for _, m := range box.Mounts {
		mounts[m.Destination] = m.RW
	}
	var lines []string
	for _, f := range cfg.ReadOnly {
		rw, ok := mounts[f.Inside]
		switch {
		case !ok:
			r.add(g, f.Inside, Fail, "not mounted, though ezbx up recorded it", "Run ezbx down, then ezbx up.")
		case rw:
			r.add(g, f.Inside, Fail, "mounted writable, so the agent can change your files", "Run ezbx down, then ezbx up: ezbx mounts it read-only.")
		default:
			r.add(g, f.Inside, Pass, fmt.Sprintf("mounted read-only, %d masked", len(f.Files)+len(f.Dirs)), "")
		}
		lines = append(lines, "R:"+f.Inside)
		for _, p := range f.Files {
			lines = append(lines, "F:"+p)
		}
		for _, p := range f.Dirs {
			lines = append(lines, "D:"+p)
		}
		for _, u := range f.Unmask {
			r.add(g, f.Inside+" unmask", Warn, fmt.Sprintf("shows files matching %q, a default mask", u), "Remove it from unmask in ezbx.toml unless the agent needs those files.")
		}
		for _, fd := range f.Findings {
			r.add(g, f.Inside+" contents", Warn, fd, "Add the file to mask for this folder in ezbx.toml, or move it. Then ezbx down and ezbx up.")
		}
		if len(f.New) > 0 {
			r.add(g, f.Inside+" new files", Fail, "match a mask but appeared after ezbx up, so the agent can read them: "+strings.Join(f.New, ", "), "Run ezbx down, then ezbx up, to mask them.")
		}
		if f.Error != "" {
			r.add(g, f.Inside+" new files", Warn, "couldn't check the folder again: "+f.Error, "")
		}
	}
	for dst, rw := range mounts {
		if rw && (dst == "/readonly" || strings.HasPrefix(dst, "/readonly/")) {
			r.add(g, dst, Fail, "a writable mount under /readonly", "Run ezbx down, then ezbx up.")
		}
	}
	if len(cfg.ReadOnly) == 0 || !box.State.Running {
		return
	}
	out, err := cfg.Docker(ctx, "exec", "-e", "EZBX_RO="+strings.Join(lines, "\n"), boxName, "sh", "-c", readOnlyScript)
	got := strings.TrimSpace(string(out))
	if err != nil {
		r.add(g, "seen from the box", Warn, "couldn't ask the box: "+shortErr(err), "")
		return
	}
	if !strings.HasSuffix(got, "done") {
		r.add(g, "seen from the box", Warn, "couldn't ask the box: its answer stopped early", "")
		return
	}
	var problems []string
	for _, l := range strings.Split(strings.TrimSuffix(got, "done"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			problems = append(problems, l)
		}
	}
	if len(problems) == 0 {
		r.add(g, "seen from the box", Pass, "the agent can't write the folders, and every mask hides its file or folder", "")
		return
	}
	r.add(g, "seen from the box", Fail, strings.Join(problems, "; "), "Run ezbx down, then ezbx up. If it stays, report it as a security bug (SECURITY.md).")
}
