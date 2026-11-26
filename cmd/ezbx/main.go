// Command ezbx is the ezbx command line tool.
//
// It starts and stops a box (ezbx up, ezbx down), finds model servers by the
// API they speak (ezbx engine detect), runs the gateway (ezbx gateway serve)
// and more.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/OranjSvensk/ezbx/internal/adapter"
	"github.com/OranjSvensk/ezbx/internal/detect"
	"github.com/OranjSvensk/ezbx/providers"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

func resolveVersion() string {
	if v := strings.TrimSpace(version); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const usage = `ezbx - boxed-up local AI agents

Usage:
  ezbx [--version] [help]
  ezbx version [--json]
  ezbx status [--box NAME] [--config FILE] [--json]
  ezbx init [--plan] [--yes] [--no-start] [--box NAME] [--config FILE] [--profile NAME]
            [--list-profiles] [--url ADDRESS | --later]
            [--api auto|openai|lmstudio|anthropic] [--key-env NAME] [--reasoning]
            [--harness pi,opencode,omp,codex,claude,hermes,openclaw]
            [--tools pack:coding,ffmpeg|none] [--workspace PATH]
            [--network airgap|off|allow|on] [--memory 8g]
  ezbx create --box NAME --workspace FOLDER [--network MODE] [--allow HOST]...
              [--readonly FOLDER]... [--harness LIST] [--tools LIST]
              [--profile NAME] [--memory 8g] [--config FILE] [--json]
  ezbx build [--box NAME] [--config FILE] [--prune [--yes]]
  ezbx up [--box NAME] [--config FILE] [--no-doctor]
  ezbx down [--box NAME] [--config FILE]
  ezbx start [--box NAME] [--doctor] [--json] | ezbx stop [--box NAME] [--json]
  ezbx shell [HARNESS] [--box NAME] [--no-snapshot] [-- HARNESS ARGS...]
  ezbx exec [--box NAME] [--json] [--cwd DIR] [--timeout 10m] [--env NAME=VALUE]...
            [--snapshot] -- COMMAND [ARGS...]
  ezbx snapshot [-m NOTE] | ezbx snapshot list | ezbx snapshot remove ID [--yes]
  ezbx review [ID] [--diff]
  ezbx rewind [ID] [--yes]
  ezbx admin shell [--net] [--box NAME] [-- COMMAND...]
  ezbx reset [--rebuild] [--restore ID] [--yes] [--no-doctor] | ezbx reset --list
  ezbx destroy [--box NAME] [--remove-config] [--images] [--yes] [--json]
  ezbx net [airgap|off|allow|on] [--box NAME]
  ezbx net allow|remove HOST... [--box NAME]
  ezbx engine detect [--json] [--timeout 500ms] [--url ADDRESS]...
  ezbx engine use [--url ADDRESS] [--api auto|openai|lmstudio|anthropic] [--box NAME]
  ezbx gateway serve [--upstream ADDRESS] [--engine KIND] [--listen ADDRESS]
                     [--key-env NAME | --key-file FILE] [--log FILE]
                     [--proxy ADDRESS] [--network airgap|off|allow|on] [--allow HOST]...
                     [--allow-file FILE]
                     [--admin 127.0.0.1:PORT --admin-token-file FILE]
  ezbx gateway admin [--addr 127.0.0.1:PORT] [--token-file FILE] CALL
  ezbx gateway health [--addr 127.0.0.1:PORT]
  ezbx harness list [--box NAME] [--json] | ezbx harness add|remove ID... [--box NAME]
  ezbx harness render ID --out FOLDER [--url ADDRESS] [--default MODEL]
                     [--context N] [--max-tokens N] [--reasoning]
  ezbx doctor [--config FILE] [--box NAME] [--json]

Commands:
  init            Guided setup: finds the model server, asks the questions, shows
                  the plan, then writes ezbx.toml and starts the box.
                  Run it again to change the setup.
  create          Add a box to ezbx.toml without asking anything (for
                  programs; ezbx init asks). Builds and starts nothing.
  start | stop    ezbx up and ezbx down for one box. start runs no doctor
                  unless --doctor; with --json both print the box's state.
  build           Build the gateway and box images from the files inside
                  this program, with the tools chosen in ezbx.toml. Each
                  image is named after a hash of what goes into it, so an
                  image that is built already is skipped. --prune then
                  removes older ezbx images, after asking.
  up              Start the box described in ezbx.toml: its networks, the
                  gateway and the box, with the harness config filled in
                  from the model server's models. Then run the doctor.
  down            Remove the box's containers and networks. The workspace,
                  the state folder and the agent's home volume are kept.
  shell           Open bash in the box's workspace, or start a harness
                  there (ezbx shell pi), as the unprivileged agent user.
  exec            Run one command in the running box as the agent, without
                  a terminal, for scripts and programs. --json prints one
                  result with the exit code and output; --timeout ends the
                  command inside the box. No snapshot unless --snapshot.
  snapshot        Take an undo point of the workspace by hand (kept until
                  removed); list shows them all. ezbx shell takes one
                  before every session (the last 20 are kept).
  review          Show what changed in the workspace since a snapshot
                  (the newest by default); --diff shows the lines.
  rewind          Put the workspace back as it was in a snapshot (the
                  newest by default). Takes a snapshot first, so a rewind
                  can be undone too.
  admin shell     A root shell for you (never the agent) in a temporary
                  container beside the box, with the workspace and the
                  agent's home mounted. No network unless --net. Changes
                  outside those two folders are gone when it exits.
  reset           Back up, then give the box a fresh agent home and fresh
                  containers. Keeps the workspace, snapshots, approved hosts,
                  config and images. --restore ID puts a backed-up home back.
  destroy         Remove everything ezbx made for the box: containers,
                  networks, the agent's home, the state folder (snapshots
                  too) and the box's images that no other box needs.
                  --images removes every unused ezbx image too. Never the
                  workspace. Asks you to type the box name first.
  net             Show the network mode, allowed and recently blocked
                  hosts. net airgap|off|allow|on changes the mode until the next
                  up. net allow HOST approves a host and saves it, so it
                  stays approved; net remove HOST forgets it.
  engine detect   Find model servers on this computer's common ports (and
                  at --url), and the API each one speaks. Any server with
                  an OpenAI- or Anthropic-compatible API works, whatever
                  program runs it.
  engine use      Point the running gateway at the server in ezbx.toml (or
                  at --url), checked again now, and refresh the harness
                  config in the box. Lasts until the next up.
  gateway serve   Run the gateway: the model route (/v1/... to one server),
                  and optionally the egress proxy and the admin API.
                  Without --upstream it uses the one server it finds.
  gateway admin   Call the admin API. Meant to run inside the gateway
                  container through docker exec. CALL is one of:
                    status
                    ready              (does the server answer with models?)
                    engine UPSTREAM [KIND]
                    network airgap|off|allow|on [HOST]...
                    allow HOST
  gateway health  Exit 0 if the gateway answers /healthz, for Docker's
                  healthcheck.
  harness         list shows the verified harnesses and which the box has;
                  add and remove change them in ezbx.toml (then ezbx build,
                  ezbx down and ezbx up).
  harness render  Write a harness's config files, filled in from the
                  server's model list, laid out like the agent's home.
  doctor          Check Docker, the model server, the networks, the gateway and
                  the box, then check from inside the box's network that it
                  reaches the model and nothing else. Explains each fix.
  status          Whether a box is missing, stopped, running or unhealthy,
                  with its network mode and image. --json for programs.
  events          Watch runtime events with --jsonl. --since replays recent
                  events before following new ones. --gateway watches model
                  request outcomes and blocked hosts.
  version         Print the ezbx version (same as --version or -v). --json
                  adds the contract version and features, for programs that
                  run ezbx (docs/runtime-contract.md).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is separate from main so it can be tested. It returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		return runVersion(nil, stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "version" {
		return runVersion(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "status" {
		return runStatus(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "events" {
		return runEvents(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "exec" {
		return runExec(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "create" {
		return runCreate(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "start" {
		return runStart(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "stop" {
		return runStop(args[1:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "engine" && args[1] == "detect" {
		return runDetect(args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "init" {
		return runInit(args[1:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "engine" && args[1] == "use" {
		return runEngineUse(args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "net" {
		return runNet(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "shell" {
		return runShell(args[1:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "admin" && args[1] == "shell" {
		return runAdminShell(args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "reset" {
		return runReset(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "destroy" {
		return runDestroy(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "snapshot" {
		return runSnapshot(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "rewind" {
		return runRewind(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "review" {
		return runReview(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "build" {
		return runBuild(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "up" {
		return runUp(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "down" {
		return runDown(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "doctor" {
		return runDoctor(args[1:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "harness" && args[1] == "render" {
		return runHarnessRender(args[2:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "harness" {
		return runHarness(args[1:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "gateway" {
		switch args[1] {
		case "serve":
			return runGateway(args[2:], stdout, stderr)
		case "admin":
			return runAdmin(args[2:], stdout, stderr)
		case "health":
			return runHealth(args[2:], stdout, stderr)
		}
	}
	asked := len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h"
	if asked {
		showBanner(stderr)
	}
	fmt.Fprint(stderr, usage)
	if asked {
		return 0
	}
	return 2
}

// addresses lets --url be given more than once.
type addresses []string

func (a *addresses) String() string     { return strings.Join(*a, ",") }
func (a *addresses) Set(v string) error { *a = append(*a, v); return nil }

func runDetect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine detect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	timeout := fs.Duration("timeout", 500*time.Millisecond, "time to wait for each request")
	var urls addresses
	fs.Var(&urls, "url", "an address to check as well (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		fmt.Fprintln(stderr, "internal error loading the API kinds:", err)
		return 1
	}

	d := &detect.Detector{Adapters: adapters, Timeout: *timeout}
	res := d.Detect(context.Background(), urls)

	if *asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		printTable(stdout, res)
	}
	if len(res.Engines) == 0 {
		return 1
	}
	return 0
}

func printTable(w io.Writer, res detect.Result) {
	if len(res.Engines) == 0 {
		fmt.Fprintln(w, "No model server found on the common ports.")
		fmt.Fprintln(w, "Start one, or point at it with --url http://127.0.0.1:PORT. Any server with an OpenAI- or Anthropic-compatible API works.")
	} else {
		fmt.Fprintln(w, "Model servers found (by the API they speak)")
		for _, e := range res.Engines {
			fmt.Fprintf(w, "  %-10s %s   %d model(s)\n", e.ID, e.URL, len(e.Models))
			for _, m := range e.Models {
				fmt.Fprintf(w, "      %s%s\n", m.ID, describe(m))
			}
			if len(e.AlsoMatched) > 0 {
				fmt.Fprintf(w, "      (also speaks: %s; the most specific kind was chosen)\n", strings.Join(e.AlsoMatched, ", "))
			}
			if fw := detect.FileNamedWarning(e); fw != "" {
				fmt.Fprintf(w, "      warning: %s.\n", fw)
			}
		}
	}
	for _, r := range res.Refused {
		if r.KeySent {
			fmt.Fprintf(w, "note: %s refused the key it was sent\n", r.URL)
		} else {
			fmt.Fprintf(w, "note: %s asks for a key, so it wasn't checked. Set url and key_env under [engine] in ezbx.toml\n", r.URL)
		}
	}
}

func describe(m detect.Model) string {
	var parts []string
	if m.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%d ctx", m.ContextWindow))
	}
	if len(m.Input) > 0 {
		parts = append(parts, strings.Join(m.Input, "+"))
	}
	if m.Loaded != nil && *m.Loaded {
		parts = append(parts, "loaded")
	}
	if len(parts) == 0 {
		return ""
	}
	return "   (" + strings.Join(parts, ", ") + ")"
}
