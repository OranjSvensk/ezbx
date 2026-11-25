package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/box"
	"github.com/OranjSvensk/ezbx/internal/config"
)

// runEngineUse points a running box's gateway at a model server, checked
// again now, and refreshes the harness configs in the box from its model
// list. With no flags it uses the server in ezbx.toml (after it moved or
// loaded new models, say). The change lasts until the next ezbx up, which
// uses ezbx.toml.
func runEngineUse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	url := fs.String("url", "", "the server's address on this computer")
	api := fs.String("api", "", "the API kind: auto, openai, lmstudio or anthropic")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 0 || len(rest) > 0 {
		fmt.Fprintln(stderr, "Usage: ezbx engine use [--url http://127.0.0.1:PORT] [--api auto|openai|lmstudio|anthropic] [--box NAME]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	if t.cfg == nil {
		return report(stderr, fmt.Errorf("engine use needs the box's ezbx.toml, for its harnesses and model settings. See: ezbx up"))
	}
	b, err := t.needBox()
	if err != nil {
		return report(stderr, err)
	}
	cfg, err := engineChoice(t.cfg, *url, *api)
	if err != nil {
		return report(stderr, err)
	}
	if cfg.Engine.KeyEnv != "" {
		return report(stderr, fmt.Errorf("the server at %s needs a key (key_env %s), which the running gateway can't take on. Set it under [engine] in ezbx.toml, then ezbx down and ezbx up", cfg.Engine.URL, cfg.Engine.KeyEnv))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng, err := chooseEngine(ctx, cfg)
	if err != nil {
		return report(stderr, err)
	}
	upstream, err := box.GatewayUpstream(eng.URL)
	if err != nil {
		return report(stderr, err)
	}
	chosen, err := boxHarnesses(b)
	if err != nil {
		return report(stderr, err)
	}
	files, err := renderHome(cfg, chosen, eng)
	if err != nil {
		return report(stderr, err)
	}

	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if _, err := r.Admin(ctx, t.name, "engine", upstream, eng.ID); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "The gateway now forwards to %s at %s (%d model(s)).\n", eng.Name, eng.URL, len(eng.Models))
	p := box.Plan{Box: t.name, Names: box.NamesFor(t.name), HomeFiles: files}
	if err := r.CopyHome(ctx, p); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Updated the config of %s in the box. A harness that is already running keeps its old model list until you start it again.\n", strings.Join(b.Harnesses, ", "))
	if cfg.Engine != t.cfg.Engine {
		fmt.Fprintln(stdout, "This lasts until the next ezbx up, which uses the server in ezbx.toml.")
	}
	return 0
}

// engineChoice applies --url and --api to a copy of the config, leaving the
// loaded config unchanged. A new address drops key_env and host, which
// belonged to the old one: a key is only ever sent to its own url.
func engineChoice(c *config.Config, url, api string) (*config.Config, error) {
	cp := *c
	if url != "" {
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, usageError("--url must start with http:// or https://")
		}
		if strings.TrimRight(url, "/") != strings.TrimRight(cp.Engine.URL, "/") {
			cp.Engine = config.Engine{API: cp.Engine.API, URL: url}
		}
	}
	if api != "" {
		cp.Engine.API = api
	}
	return &cp, nil
}
