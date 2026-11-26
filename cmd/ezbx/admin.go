package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/images"
	"github.com/OranjSvensk/ezbx/internal/paths"
	"github.com/OranjSvensk/ezbx/internal/setup"
	"github.com/OranjSvensk/ezbx/internal/snapshot"
)

// runAdminShell opens a root shell for the human in a temporary container
// beside the box. See docs/admin.md.
func runAdminShell(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	netFlag := fs.Bool("net", false, "connect to the internet through the gateway's outside network (for downloads)")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx admin shell [--net] [--box NAME] [-- COMMAND...]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	b, err := t.needBox()
	if err != nil {
		return report(stderr, err)
	}
	root, err := workspaceOf(b)
	if err != nil {
		return report(stderr, err)
	}
	_, bx, err := imageSpecs(b.Tools, b.Harnesses)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := imageRunner(io.Discard).Check(ctx, bx); err != nil {
		return report(stderr, err)
	}
	r := &box.Runner{Docker: dockerIn}
	n := box.NamesFor(t.name)
	// The home volume must be ezbx's own: docker run would quietly make an
	// unlabelled one otherwise.
	if ok, err := r.Exists(ctx, "volume", n.Home); err != nil {
		return report(stderr, err)
	} else if !ok {
		return report(stderr, box.ErrNotUp{Box: t.name, What: "its home volume doesn't exist yet"})
	}
	if *netFlag {
		if ok, err := r.Exists(ctx, "network", n.Outside); err != nil {
			return report(stderr, err)
		} else if !ok {
			return report(stderr, box.ErrNotUp{Box: t.name, What: "--net needs the gateway's outside network"})
		}
	}
	if ok, err := r.Exists(ctx, "container", box.AdminName(t.name)); err != nil {
		return report(stderr, err)
	} else if ok {
		return report(stderr, errors.New(adminShellOpen(t.name, adminRunning(ctx, t.name))))
	}
	cancel()

	net := "none"
	if *netFlag {
		net = "the internet, through the gateway's outside network (not the box's network switch)"
	}
	fmt.Fprintf(stderr, "Admin shell: root, in a temporary container from %s, removed when you exit.\n", bx.Tag())
	fmt.Fprintf(stderr, "  /workspace and the agent's home (/home/agent) are the box's own; changes there last.\n")
	fmt.Fprintf(stderr, "  Everything else is gone when you exit. To give the agent a program: tools in ezbx.toml, then ezbx build.\n")
	fmt.Fprintf(stderr, "  Network: %s.\n", net)

	a := box.AdminShell{Box: t.name, Workspace: root, Image: bx.Tag(), Net: *netFlag, TTY: isTerminal(os.Stdin), Command: rest}
	cmd := exec.Command("docker", a.Args()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return report(stderr, err)
	}
	return 0
}

// activity lists what could be writing to the box's workspace or home: a
// session in the box, or an open admin shell.
func activity(ctx context.Context, name string) ([]string, error) {
	procs, err := sessionRunning(ctx, name)
	if err != nil {
		return nil, err
	}
	if ok, err := (&box.Runner{Docker: dockerIn}).Exists(ctx, "container", box.AdminName(name)); err != nil {
		return nil, err
	} else if ok {
		procs = append(procs, "an admin shell; if no terminal has it open any more, end it with: docker stop "+box.AdminName(name))
	}
	return procs, nil
}

// adminRunning reports whether the box's admin shell container is running.
// If Docker can't say, it is taken as running, the more careful answer.
func adminRunning(ctx context.Context, name string) bool {
	out, err := dockerIn(ctx, nil, "container", "inspect", "--format", "{{.State.Running}}", box.AdminName(name))
	return err != nil || strings.TrimSpace(string(out)) != "false"
}

// adminShellOpen explains an admin shell container that is still there.
// Closing a terminal window doesn't always end the shell in it, and then
// nothing removes the container, so say how.
func adminShellOpen(name string, running bool) string {
	c := box.AdminName(name)
	if !running {
		return fmt.Sprintf("an admin shell container for box %q (%s) is left over but not running. Remove it with: docker rm %s", name, c, c)
	}
	return fmt.Sprintf("an admin shell for box %q is already open (%s). Use that one, or if no terminal has it open any more (a closed window can leave it running), end it with: docker stop %s", name, c, c)
}

// imagesUsedByOthers lists the images (by name and by ID) that containers
// other than this box's own use.
func imagesUsedByOthers(ctx context.Context, n box.Names, name string) (map[string]bool, error) {
	out, err := dockerIn(ctx, nil, "container", "ls", "--all", "--format", "{{.Names}}|{{.Image}}")
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	return othersImages(string(out), n.Box, n.Gateway, box.AdminName(name)), nil
}

// othersImages reads docker container ls output, leaving out the given
// containers. A container's image shows as its name, or as Docker's short
// ID (the same one docker image ls shows) once that name has moved to a
// newer image.
func othersImages(out string, own ...string) map[string]bool {
	mine := map[string]bool{}
	for _, o := range own {
		mine[o] = true
	}
	used := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		name, image, ok := strings.Cut(strings.TrimSpace(line), "|")
		if ok && !mine[name] && image != "" {
			used[image] = true
		}
	}
	return used
}

// dirSize adds up the files in a folder.
func dirSize(dir string) (size int64, files int) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
				files++
			}
		}
		return nil
	})
	return size, files
}

func countGlob(pattern string) int {
	m, _ := filepath.Glob(pattern)
	return len(m)
}

// stateDirFor is the box's state folder, checked to lie inside the state
// root, and not to hold or sit inside the workspace, before anything there
// is removed.
func stateDirFor(t target) (string, error) {
	dir := t.dirs.Box(t.name)
	if !paths.Within(dir, t.dirs.State) || filepath.Clean(dir) == filepath.Clean(t.dirs.State) {
		return "", fmt.Errorf("internal error: the state folder %s is not inside %s", dir, t.dirs.State)
	}
	if b := t.box(); b != nil {
		if ws, err := workspaceOf(b); err == nil && (paths.Within(ws, dir) || paths.Within(dir, ws)) {
			return "", fmt.Errorf("the workspace %s and the state folder %s overlap, so ezbx won't remove the state folder. Move one of them", ws, dir)
		}
	}
	return dir, nil
}

// runDestroy removes everything ezbx made for a box, never the workspace.
func runDestroy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	alsoConfig := fs.Bool("remove-config", false, "also remove the box from ezbx.toml (the whole file, and ezbx.toml.previous, when it is the only box)")
	yes := fs.Bool("yes", false, "don't ask (refused while a session is running)")
	allImages := fs.Bool("images", false, "also remove every other ezbx image no container uses, and the build record")
	asJSON := fs.Bool("json", false, "print one JSON result (needs --yes); the list goes to stderr")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	// With --json, standard output holds only the result.
	out := stdout
	if *asJSON {
		out = stderr
	}
	refuse := func(err error) int {
		if *asJSON {
			return writeResult(stdout, *boxFlag, nil, err)
		}
		return report(stderr, err)
	}
	if len(words) != 0 {
		return refuse(usageError("Usage: ezbx destroy [--box NAME] [--remove-config] [--images] [--yes] [--json]"))
	}
	if *asJSON && !*yes {
		return refuse(usageError("ezbx destroy --json needs --yes: it can't ask for the box name"))
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return refuse(err)
	}
	cfgFile := t.dirs.ConfigFile(*cfgFlag, os.Getenv)
	stateDir, err := stateDirFor(t)
	if err != nil {
		return refuse(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: out}
	if err := r.CheckDocker(ctx); err != nil {
		return refuse(err)
	}
	busyNow, err := activity(ctx, t.name)
	if err != nil {
		return refuse(err)
	}
	if len(busyNow) > 0 && *yes {
		return refuse(fmt.Errorf("something is running in box %q (%s). End it first, or run ezbx destroy without --yes to be asked", t.name, strings.Join(busyNow, "; ")))
	}

	// What exists, to show before anything is removed.
	n := box.NamesFor(t.name)
	var list []string
	for _, item := range []struct{ kind, name, what string }{
		{"container", n.Box, "the box"}, {"container", n.Gateway, "the gateway"}, {"container", box.AdminName(t.name), "an open admin shell"},
		{"network", n.Inside, "the inside network"}, {"network", n.Outside, "the outside network"},
		{"volume", n.Home, "the agent's home (harness settings, sessions, history)"},
	} {
		ok, err := r.Exists(ctx, item.kind, item.name)
		if err != nil {
			return refuse(err)
		}
		if ok {
			list = append(list, fmt.Sprintf("%-9s %s: %s", item.kind, item.name, item.what))
		}
	}
	if st, err := os.Stat(stateDir); err == nil && st.IsDir() {
		size, files := dirSize(stateDir)
		snaps := countGlob(filepath.Join(stateDir, "snapshots", "snaps", "*.json"))
		backups := countGlob(filepath.Join(stateDir, "backups", "*"))
		list = append(list, fmt.Sprintf("%-9s %s: %d files, %s, with %d snapshot(s) (the workspace's undo history) and %d backup(s)",
			"folder", stateDir, files, snapshot.Bytes(size), snaps, backups))
	}
	imgs, err := r.Images(ctx)
	if err != nil {
		return refuse(err)
	}
	// Only this box's images go, unless --images: another box in ezbx.toml
	// may need the same ones, and any that a container uses are kept.
	usedByOthers, err := imagesUsedByOthers(ctx, n, t.name)
	if err != nil {
		return refuse(err)
	}
	own, others := imageOwners(t.cfg, t.name)
	removeImgs, keptImages := destroyImages(imgs, usedByOthers, own, others, *allImages)
	for _, img := range removeImgs {
		list = append(list, fmt.Sprintf("%-9s %s (%s)", "image", img.Name, img.Size))
	}
	record := images.RecordFile(t.dirs.Config)
	if _, err := os.Stat(record); err == nil && *allImages {
		list = append(list, fmt.Sprintf("%-9s %s", "file", record))
	}
	// --remove-config takes only this box's section when other boxes stay.
	var cfgFiles []string
	cfgText, keepOthers := "", false
	if *alsoConfig && t.cfg != nil {
		cfgText, keepOthers = configWithout(t.cfg, t.name, time.Now())
	}
	switch {
	case *alsoConfig && keepOthers && t.box() != nil:
		list = append(list, fmt.Sprintf("%-9s [box.%s] in %s (the other boxes stay; the old file is kept as .previous)", "setting", t.name, cfgFile))
	case *alsoConfig && !keepOthers:
		for _, f := range []string{cfgFile, cfgFile + ".previous"} {
			if _, err := os.Stat(f); err == nil {
				cfgFiles = append(cfgFiles, f)
				list = append(list, fmt.Sprintf("%-9s %s", "file", f))
			}
		}
	}
	if len(list) == 0 {
		fmt.Fprintf(out, "ezbx has nothing for box %q on this computer. Nothing to remove.\n", t.name)
		if *asJSON {
			return writeResult(stdout, t.name, map[string]any{"removed": []string{}, "kept": keptText(keptImages)}, nil)
		}
		return 0
	}

	fmt.Fprintf(out, "ezbx destroy removes, for box %q", t.name)
	fmt.Fprintln(out, ":")
	for _, l := range list {
		fmt.Fprintln(out, "  "+l)
	}
	fmt.Fprintln(out, "Kept:")
	if b := t.box(); b != nil {
		if ws, err := workspaceOf(b); err == nil {
			fmt.Fprintf(out, "  the workspace %s (never touched)\n", ws)
		}
	} else {
		fmt.Fprintln(out, "  the workspace (never touched)")
	}
	if !*alsoConfig {
		fmt.Fprintf(out, "  %s (--remove-config removes it too)\n", cfgFile)
	}
	for _, k := range keptImages {
		fmt.Fprintf(out, "  the image %s, because %s\n", k.Name, k.Why)
	}
	if !*allImages {
		fmt.Fprintln(out, "  other ezbx images: other boxes' and older builds (ezbx build --prune, or ezbx destroy --images, removes unused ones)")
	}
	fmt.Fprintln(out, "  base images Docker downloaded to build ezbx's (node, golang); they aren't ezbx's own. Remove them with docker image rm if you like.")
	if len(busyNow) > 0 {
		fmt.Fprintf(out, "Running now, and ended by this: %s\n", strings.Join(busyNow, "; "))
	}
	if !*yes {
		fmt.Fprintln(out, "\nThis can't be undone.")
		p := &setup.Prompter{In: bufio.NewReader(stdin), Out: out}
		typed, err := p.Ask(fmt.Sprintf("Type the box name (%s) to remove all of the above, or anything else to stop", t.name), "", nil)
		if err != nil {
			return refuse(err)
		}
		if strings.TrimSpace(typed) != t.name {
			fmt.Fprintln(out, "Nothing was removed.")
			return 0
		}
	}

	failed := 0
	fail := func(err error) {
		fmt.Fprintln(stderr, "not removed:", firstLine(err))
		failed++
	}
	if ok, _ := r.Exists(ctx, "container", box.AdminName(t.name)); ok {
		if _, err := dockerIn(ctx, nil, "container", "rm", "--force", box.AdminName(t.name)); err != nil {
			fail(err)
		}
	}
	if err := r.Down(ctx, t.name, t.dirs.Box(t.name)); err != nil {
		fail(err)
	}
	if _, err := r.RemoveVolume(ctx, n.Home); err != nil {
		fail(err)
	}
	for _, img := range removeImgs {
		if err := r.RemoveImage(ctx, img); err != nil {
			fail(err)
		}
	}
	if _, err := os.Stat(stateDir); err == nil {
		if err := os.RemoveAll(stateDir); err != nil {
			fail(err)
		} else {
			fmt.Fprintln(out, "removed folder", stateDir)
		}
	}
	if *alsoConfig && keepOthers && t.box() != nil {
		if err := writeConfig(cfgFile, cfgText); err != nil {
			fail(err)
		} else {
			fmt.Fprintf(out, "removed [box.%s] from %s\n", t.name, cfgFile)
		}
	}
	files := cfgFiles
	if *allImages {
		files = append([]string{record}, files...)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(err)
		} else if err == nil {
			fmt.Fprintln(out, "removed file", f)
		}
	}
	if failed == 0 {
		recordEvent(t, "box.destroyed", map[string]any{}, 0, stderr)
	}
	if *asJSON {
		var err error
		if failed > 0 {
			err = fmt.Errorf("%d item(s) weren't removed; the list is on stderr", failed)
		}
		return writeResultData(stdout, t.name, map[string]any{"removed": list, "kept": keptText(keptImages)}, err)
	}
	if failed > 0 {
		return 1
	}
	fmt.Fprintf(stdout, "Box %q is destroyed. The workspace is untouched. To start again: ezbx init\n", t.name)
	return 0
}

func keptText(k []keptImage) []string {
	out := []string{}
	for _, i := range k {
		out = append(out, i.Name+": "+i.Why)
	}
	return out
}

// Backups -----------------------------------------------------------------

var backupID = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)

const keepBackups = 5

func backupsDir(t target) string { return filepath.Join(t.dirs.Box(t.name), "backups") }

// listBackups returns the backup ids, oldest first.
func listBackups(t target) []string {
	entries, _ := os.ReadDir(backupsDir(t))
	var ids []string
	for _, e := range entries {
		if e.IsDir() && backupID.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// backupBox saves the config, the approved hosts and the agent's home volume
// (as home.tar) before reset changes anything. Any failure removes the
// partial backup and is returned, and then reset changes nothing.
func backupBox(ctx context.Context, t target, cfgFile, image string, now time.Time) (string, error) {
	base := now.UTC().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(backupsDir(t), id)); errors.Is(err, os.ErrNotExist) {
			break
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	dir := filepath.Join(backupsDir(t), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	copyFile := func(src, name string) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
	if err := copyFile(cfgFile, "ezbx.toml"); err != nil {
		return "", fmt.Errorf("copying the config: %w", err)
	}
	if hosts := box.ApprovedFileFor(t.dirs.Box(t.name)); fileExists(hosts) {
		if err := copyFile(hosts, "approved-hosts"); err != nil {
			return "", fmt.Errorf("copying the approved hosts: %w", err)
		}
	}
	if has, err := (&box.Runner{Docker: dockerIn}).Exists(ctx, "volume", box.NamesFor(t.name).Home); err != nil {
		return "", err
	} else if has {
		file := filepath.Join(dir, "home.tar")
		if err := dockerToFile(ctx, file, box.HomeBackupArgs(t.name, image)...); err != nil {
			return "", fmt.Errorf("copying the agent's home: %w", err)
		}
		if err := checkTar(file); err != nil {
			return "", fmt.Errorf("the copy of the agent's home is damaged: %w", err)
		}
	}
	ok = true
	return id, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// checkTar reads a tar file to the end, every file's content included, and
// checks that it closes with tar's end marker (two blocks of zeros), so a
// copy cut off anywhere is caught: Go's reader alone takes a cut between
// two files for the end.
func checkTar(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	n := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err == nil {
			_, err = io.Copy(io.Discard, tr)
		}
		if err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("it holds nothing")
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size()%512 != 0 || st.Size() < 1024 {
		return errors.New("it doesn't end on a whole block")
	}
	tail := make([]byte, 1024)
	if _, err := f.ReadAt(tail, st.Size()-1024); err != nil {
		return err
	}
	if !bytes.Equal(tail, make([]byte, 1024)) {
		return errors.New("it has no end marker, so it was cut off")
	}
	return nil
}

// dockerToFile runs docker with its standard output going to a file.
func dockerToFile(ctx context.Context, file string, args ...string) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = f, &errOut
	err = cmd.Run()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// dockerFromFile runs docker with a file on its standard input.
func dockerFromFile(ctx context.Context, file string, args ...string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stderr = f, &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// pruneBackups keeps the newest keepBackups backups and any named in keep.
func pruneBackups(t target, keep ...string) {
	ids := listBackups(t)
	protect := map[string]bool{}
	for _, k := range keep {
		protect[k] = true
	}
	for i := 0; i < len(ids)-keepBackups; i++ {
		if !protect[ids[i]] {
			os.RemoveAll(filepath.Join(backupsDir(t), ids[i]))
		}
	}
}

// runReset gives the box a fresh agent home and fresh containers, after
// backing up. The workspace, snapshots, approved hosts, config and images
// are kept.
func runReset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	rebuild := fs.Bool("rebuild", false, "also build the images again, without Docker's cache")
	restore := fs.String("restore", "", "put the agent's home back from this backup (see --list)")
	list := fs.Bool("list", false, "list the backups")
	yes := fs.Bool("yes", false, "don't ask")
	noDoctor := fs.Bool("no-doctor", false, "skip the doctor's checks after starting the box again")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx reset [--rebuild] [--restore ID] [--yes] [--no-doctor] | ezbx reset --list")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	if *list {
		ids := listBackups(t)
		if len(ids) == 0 {
			fmt.Fprintln(stdout, "No backups yet. ezbx reset makes one before it changes anything.")
			return 0
		}
		fmt.Fprintf(stdout, "Backups of box %q, in %s (newest last):\n", t.name, backupsDir(t))
		for _, id := range ids {
			size, _ := dirSize(filepath.Join(backupsDir(t), id))
			home := "agent's home included"
			if !fileExists(filepath.Join(backupsDir(t), id, "home.tar")) {
				home = "no agent home (the box had none)"
			}
			fmt.Fprintf(stdout, "  %-18s %10s  %s\n", id, snapshot.Bytes(size), home)
		}
		return 0
	}
	b, err := t.needBox()
	if err != nil {
		return report(stderr, err)
	}
	cfgFile := t.dirs.ConfigFile(*cfgFlag, os.Getenv)
	if *restore != "" {
		if !backupID.MatchString(*restore) || !fileExists(filepath.Join(backupsDir(t), *restore, "home.tar")) {
			return report(stderr, fmt.Errorf("no backup %q with the agent's home; ezbx reset --list shows them", *restore))
		}
	}
	gw, bx, err := imageSpecs(b.Tools, b.Harnesses)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.CheckDocker(ctx); err != nil {
		return report(stderr, err)
	}
	if busyNow, err := activity(ctx, t.name); err != nil {
		return report(stderr, err)
	} else if len(busyNow) > 0 {
		return report(stderr, fmt.Errorf("something is running in box %q (%s). End it, then reset", t.name, strings.Join(busyNow, "; ")))
	}

	fmt.Fprintf(stdout, "ezbx reset, for box %q", t.name)
	fmt.Fprintln(stdout, ":")
	fmt.Fprintf(stdout, "  1. back up the config, the approved hosts and the agent's home to %s\n", backupsDir(t))
	fmt.Fprintln(stdout, "     (if the backup fails, nothing is changed)")
	fmt.Fprintf(stdout, "  2. remove the box's containers, networks and the agent's home volume %s\n", box.NamesFor(t.name).Home)
	step := 3
	if *rebuild {
		fmt.Fprintf(stdout, "  %d. build both images again, without Docker's cache\n", step)
		step++
	}
	if *restore != "" {
		fmt.Fprintf(stdout, "  %d. put the agent's home back from backup %s\n", step, *restore)
		step++
	}
	fmt.Fprintf(stdout, "  %d. start the box again (ezbx up)\n", step)
	fmt.Fprintln(stdout, "Kept: the workspace, its snapshots, the approved hosts, the config and the images.")
	if !*yes {
		p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout}
		ok, err := p.YesNo("Reset?", false)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "Nothing was changed.")
			return 0
		}
	}

	// The backup container needs the box image.
	if err := buildImages(ctx, t.dirs, stdout, gw, bx); err != nil {
		return report(stderr, fmt.Errorf("%w\nNothing was changed", err))
	}
	id, err := backupBox(ctx, t, cfgFile, bx.Tag(), time.Now())
	if err != nil {
		return report(stderr, fmt.Errorf("the backup failed, so nothing was changed: %w", err))
	}
	fmt.Fprintf(stdout, "backed up to %s\n", filepath.Join(backupsDir(t), id))
	if err := r.Down(ctx, t.name, t.dirs.Box(t.name)); err != nil {
		return report(stderr, err)
	}
	if _, err := r.RemoveVolume(ctx, box.NamesFor(t.name).Home); err != nil {
		return report(stderr, err)
	}
	if *rebuild {
		if err := rebuildImages(ctx, t.dirs, stdout, gw, bx); err != nil {
			return report(stderr, fmt.Errorf("%w\nThe box is down; ezbx up starts it again (the backup is %s)", err, id))
		}
	}
	if *restore != "" {
		if _, err := dockerIn(ctx, nil, box.HomeVolumeArgs(t.name)...); err != nil {
			return report(stderr, fmt.Errorf("creating the home volume: %w", err))
		}
		if err := dockerFromFile(ctx, filepath.Join(backupsDir(t), *restore, "home.tar"), box.HomeRestoreArgs(t.name, bx.Tag())...); err != nil {
			return report(stderr, fmt.Errorf("putting the agent's home back failed: %w\nThe box is down; ezbx reset --restore %s tries again", err, *restore))
		}
		fmt.Fprintf(stdout, "put the agent's home back from %s\n", *restore)
	}
	pruneBackups(t, id, *restore)
	fmt.Fprintln(stdout)
	upArgs := []string{"--config", cfgFile, "--box", t.name}
	if *noDoctor {
		upArgs = append(upArgs, "--no-doctor")
	}
	if code := runUp(upArgs, stdout, stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "\nReset done. The agent's home before it is in backup %s: ezbx reset --restore %s puts it back.\n", id, id)
	return 0
}
