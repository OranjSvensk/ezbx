package main

// ezbx harness list|add|remove: which harnesses a box has, changed in
// ezbx.toml. The box image holds only its own harnesses, so a change takes
// effect after ezbx build, ezbx down and ezbx up.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/harnesses"
	"github.com/OranjSvensk/ezbx/internal/harness"
	"github.com/OranjSvensk/ezbx/internal/setup"
)

const harnessUsage = `Usage:
  ezbx harness list [--box NAME] [--json]   the verified harnesses, and which the box has
  ezbx harness add ID... [--box NAME]        add harnesses to the box in ezbx.toml
  ezbx harness remove ID... [--box NAME]     take harnesses out (at least one stays)
Then: ezbx build --box NAME, ezbx down and ezbx up.`

// verifiedHarnesses are the harnesses a box may have, coding ones first.
func verifiedHarnesses() ([]harness.Manifest, error) {
	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return nil, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	return onlyVerified(all), nil
}

// onlyVerified keeps the verified harnesses, coding ones first.
func onlyVerified(all []harness.Manifest) []harness.Manifest {
	var out []harness.Manifest
	for _, m := range all {
		if m.Verified.Status == "verified" {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b harness.Manifest) int {
		switch {
		case a.Kind == "coding" && b.Kind != "coding":
			return -1
		case a.Kind != "coding" && b.Kind == "coding":
			return 1
		}
		return 0
	})
	return out
}

// changeHarnesses adds or removes harness ids, keeping the list's order and
// refusing unknown ids and an empty result. changed is false when nothing
// would change.
func changeHarnesses(have []string, ids []string, add bool, known []harness.Manifest) (out []string, changed bool, err error) {
	if len(ids) == 0 {
		return nil, false, usageError("Name at least one harness, such as: ezbx harness add opencode")
	}
	out = slices.Clone(have)
	for _, id := range ids {
		if !slices.ContainsFunc(known, func(m harness.Manifest) bool { return m.ID == id }) {
			var names []string
			for _, m := range known {
				names = append(names, m.ID)
			}
			return nil, false, usageError(fmt.Sprintf("unknown harness %q (verified: %s)", id, strings.Join(names, ", ")))
		}
		i := slices.Index(out, id)
		switch {
		case add && i < 0:
			out, changed = append(out, id), true
		case !add && i >= 0:
			out, changed = slices.Delete(out, i, i+1), true
		}
	}
	if len(out) == 0 {
		return nil, false, usageError("a box needs at least one harness; add another before removing the last")
	}
	return out, changed, nil
}

func runHarness(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, harnessUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("harness "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name; needed when ezbx.toml has several")
	asJSON := fs.Bool("json", false, "list: print JSON")
	words, rest, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return 2
	}
	if len(rest) > 0 || (verb == "list" && len(words) > 0) || (verb != "list" && verb != "add" && verb != "remove") {
		fmt.Fprintln(stderr, harnessUsage)
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	known, err := verifiedHarnesses()
	if err != nil {
		return report(stderr, err)
	}
	b, err := t.needBox()
	if err != nil {
		return report(stderr, err)
	}

	if verb == "list" {
		if *asJSON {
			type row struct {
				ID, Name, Kind string
				InBox          bool `json:"inBox"`
			}
			var rows []row
			for _, m := range known {
				rows = append(rows, row{m.ID, m.Name, m.Kind, slices.Contains(b.Harnesses, m.ID)})
			}
			out, _ := json.Marshal(rows)
			fmt.Fprintln(stdout, string(out))
			return 0
		}
		fmt.Fprintf(stdout, "Harnesses for box %q (* = in the box):\n", t.name)
		for _, m := range known {
			mark := " "
			if slices.Contains(b.Harnesses, m.ID) {
				mark = "*"
			}
			fmt.Fprintf(stdout, "  %s %-9s %-14s %s\n", mark, m.ID, m.Name, m.Kind)
		}
		return 0
	}

	list, changed, err := changeHarnesses(b.Harnesses, words, verb == "add", known)
	if err != nil {
		return report(stderr, err)
	}
	if !changed {
		fmt.Fprintf(stdout, "Nothing to change: box %q has harnesses %s.\n", t.name, strings.Join(b.Harnesses, ", "))
		return 0
	}
	a, _ := setup.FromConfigBox(t.cfg, t.name)
	a.Harnesses = list
	cfgFile := t.dirs.ConfigFile(*cfgFlag, os.Getenv)
	if err := writeConfig(cfgFile, setup.ConfigText(a, time.Now())); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Box %q now has harnesses %s (in %s; the old file is kept as .previous).\n", t.name, strings.Join(list, ", "), cfgFile)
	fmt.Fprintf(stdout, "To use them: ezbx build --box %s, then ezbx down --box %s and ezbx up --box %s\n", t.name, t.name, t.name)
	return 0
}
