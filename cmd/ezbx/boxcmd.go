package main

import (
	"errors"
	"flag"
	"os"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/paths"
)

// target is the box a command acts on.
type target struct {
	name string
	cfg  *config.Config // nil when there is no config file
	dirs paths.Dirs
}

// box is this box's settings, or nil when the config doesn't have it.
func (t target) box() *config.Box {
	if t.cfg == nil {
		return nil
	}
	b, _ := t.cfg.Get(t.name)
	return b
}

// needBox returns the box's settings, or says why there are none.
func (t target) needBox() (*config.Box, error) {
	if t.cfg == nil {
		return nil, errors.New("no ezbx.toml was found, so the workspace isn't known. Run ezbx init, or pass --config FILE")
	}
	b := t.box()
	if b == nil {
		return nil, errNoBox(t.name, t.cfg.Names())
	}
	return b, nil
}

// resolveBox finds the box: --box if given, else the one box in the config,
// else "main". A config named with --config must load. A config with
// several boxes needs --box.
func resolveBox(cfgFlag, boxFlag string) (target, error) {
	dirs, err := paths.Default()
	if err != nil {
		return target{}, err
	}
	t := target{dirs: dirs, name: boxFlag}
	cfg, err := config.Load(dirs.ConfigFile(cfgFlag, os.Getenv))
	if err != nil && cfgFlag != "" {
		return target{}, err
	}
	if err == nil {
		t.cfg = cfg
		if t.name == "" {
			if t.name, err = cfg.Default(); err != nil {
				return target{}, usageError(err.Error())
			}
		}
	}
	if t.name == "" {
		t.name = "main"
	}
	if !config.BoxName(t.name) {
		return target{}, usageError("A box name is short, lowercase letters, digits and dashes.")
	}
	return t, nil
}

// usageError is a mistake in how a command was typed (exit code 2).
type usageError string

func (e usageError) Error() string { return string(e) }

// parseInterleaved parses flags that may come before, between or after the
// other words, which Go's flag package alone doesn't allow. Everything after
// the first "--" is not parsed and comes back as rest, for passing on to a
// harness.
func parseInterleaved(fs *flag.FlagSet, args []string) (words, rest []string, err error) {
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], append([]string{}, args[i+1:]...)
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return words, rest, nil
		}
		words = append(words, args[0])
		args = args[1:]
	}
}
