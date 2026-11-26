package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/config"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/internal/doctor"
	"github.com/OranjSvensk/ezbx/internal/gateway"
)

// runDoctor checks a box from the host and from inside its network.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	return runDoctorChecks(args, stdout, stderr, false)
}

func runDoctorChecks(args []string, stdout, stderr io.Writer, skipEngine bool) int {
	if len(args) > 0 && args[0] == "inside" {
		return runDoctorInside(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file; default EZBX_CONFIG, then ezbx.toml in the config folder")
	boxFlag := fs.String("box", "", "box name; its containers are ezbx-<box>-gateway and so on (default: the config's box, else main)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if _, ok := err.(usageError); ok {
			return 2
		}
		return 1
	}

	if b := t.box(); b != nil && b.Network == gateway.ModeAirgap {
		skipEngine = true
	}
	cfg := doctor.HostConfig{
		Box:        t.name,
		Docker:     dockerCLI,
		SkipEngine: skipEngine,
		LinuxHost:  runtime.GOOS == "linux",
		LocalAddrs: localAddrs,
	}
	if !skipEngine {
		cfg.Detect, err = doctorDetect(t.cfg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	cfg.Images, cfg.ImagesNote = expectedImages(t)
	cfg.ReadOnly, cfg.ReadOnlyNote = doctorReadOnly(t.dirs.Box(t.name))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep := doctor.Run(ctx, cfg)
	if *asJSON {
		rep.PrintJSON(stdout)
	} else {
		fmt.Fprintf(stdout, "ezbx doctor, box %q\n", t.name)
		rep.Print(stdout)
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

// doctorDetect is how the doctor finds the model server: it checks only the
// configured address and the one the gateway forwards to (extra). It never
// scans this computer for others, and sends no key. c may be nil.
func doctorDetect(c *config.Config) (func(context.Context, []string) detect.Result, error) {
	kinds, err := apiKinds(config.DefaultAPI)
	if err != nil {
		return nil, err
	}
	var configured []string
	if c != nil && c.Engine.URL != "" {
		configured = append(configured, c.Engine.URL)
	}
	return func(ctx context.Context, extra []string) detect.Result {
		d := &detect.Detector{Adapters: kinds, SkipHintPorts: true}
		return d.Detect(ctx, append(slices.Clone(configured), extra...))
	}, nil
}

// expectedImages works out the images the box's config needs now, the same
// way ezbx up does, so the doctor can tell a box started from older ones.
// With no config for the box, it says why instead.
func expectedImages(t target) (map[string]doctor.Image, string) {
	if t.cfg == nil {
		return nil, "no ezbx.toml to compare with"
	}
	b, ok := t.cfg.Box[t.name]
	if !ok {
		return nil, fmt.Sprintf("ezbx.toml has no box %q to compare with", t.name)
	}
	gw, bx, err := imageSpecs(b.Tools, b.Harnesses)
	if err != nil {
		return nil, "can't work out the images from ezbx.toml: " + err.Error()
	}
	return map[string]doctor.Image{
		"gateway": {Tag: gw.Tag(), Inputs: gw.Inputs},
		"box":     {Tag: bx.Tag(), Inputs: bx.Inputs},
	}, ""
}

// runDoctorInside runs in the check container on the inside network and
// prints its report as JSON for the host side to read.
func runDoctorInside(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor inside", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := fs.String("config", "", "what to expect, as JSON (written by ezbx doctor)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var cfg doctor.InsideConfig
	dec := json.NewDecoder(strings.NewReader(*spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fmt.Fprintln(stderr, "doctor inside: bad --config:", err)
		return 2
	}
	doctor.RunInside(context.Background(), cfg).PrintJSON(stdout)
	return 0
}

// dockerCLI runs the docker command. What it prints on standard error comes
// back in the error, so failures can be explained.
func dockerCLI(ctx context.Context, args ...string) ([]byte, error) {
	return dockerIn(ctx, nil, args...)
}

// localAddrs lists this computer's own addresses (not loopback or
// link-local), so the doctor can check the box can't reach any of them.
// They are only passed to the check container, never written anywhere.
func localAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.To4() == nil {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
