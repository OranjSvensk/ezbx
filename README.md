<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark-light.svg" alt="ezbx" width="260" height="64">
  </picture>
</h1>

**Boxed-up local AI agents.**

ezbx is a one-command setup for running AI coding and agent tools inside a sandboxed container, while the model itself keeps running on your own machine.

> **Status: early release (`v0.6.1`).** All seven harnesses (Pi, OpenCode, oh-my-pi, Codex CLI, Claude Code, Hermes Agent and OpenClaw) are verified end to end on Windows 11 with Docker Desktop and a local model server. The native Linux pilot covers Ubuntu 24.04 `amd64` with local Docker Engine, Pi, OpenCode, and a fake model server. Other programs can drive ezbx through a JSON contract and follow box activity with live events and recent replay. See the [runtime contract](docs/runtime-contract.md) and [isolation results](docs/isolation-test.md).

```text
# Scoop (Windows):
scoop bucket add ezbx https://github.com/OranjSvensk/scoop-bucket
scoop install ezbx

# Or install with Go 1.27+ (or download a binary from Releases):
go install github.com/OranjSvensk/ezbx/cmd/ezbx@v0.6.1
ezbx --version
ezbx init                 # asks the questions, shows the plan, builds and starts the box
ezbx shell pi             # opens Pi in the box (or opencode, omp, codex, claude, hermes, openclaw)
```

New here? Read the step-by-step guide in [docs/getting-started.md](docs/getting-started.md). Other commands: `ezbx up` and `ezbx down` start and remove a box described in `ezbx.toml` (it can hold several; give `--box NAME`) (see [ezbx.toml.example](ezbx.toml.example)), `ezbx build` builds the images, `ezbx review` and `ezbx rewind` show and undo what changed in the workspace since a snapshot, `ezbx net` shows and switches the network and approves hosts, `ezbx engine detect` and `ezbx engine use` find a model server and point the box at it, `ezbx harness add|remove|list` changes a box's harnesses, and `ezbx doctor` checks the whole setup and explains any fix. For programs that drive ezbx, `ezbx create`, `start`, `exec`, `status`, `stop` and `destroy` take `--json` ([docs/runtime-contract.md](docs/runtime-contract.md)).

## The idea

- **The model stays on your machine.** Whatever program serves your model keeps full access to your GPU and models. ezbx works with any server that offers an OpenAI- or Anthropic-compatible API (with better model facts from LM Studio's own API), so there is no list of supported engines.
- **The work stays in the box.** Agents run in a container and can only write to folders you choose.
- **One stable address between them.** A small gateway lets the box talk to your model server, so moving to another server or port doesn't mean reconfiguring every tool.
- **You choose the harnesses.** Pick from Pi, OpenCode, oh-my-pi, Codex CLI, Claude Code, Hermes Agent and OpenClaw, in any combination. Each one's install method is verified and pinned in `ezbx.lock`.
- **Network on or off.** A switch controls whether the agent can reach the internet: `airgap` (not even the model), `off` (the model only), `allow` (listed hosts) or `on`.
- **Root stays with you.** The agent runs as an unprivileged user. Admin access is reserved for the human.

## What works today

| Area | What you get |
|---|---|
| Setup | `ezbx init` finds the model server, asks the questions (or takes a starter profile, [docs/profiles.md](docs/profiles.md)), writes `ezbx.toml` and shows the plan first |
| Images | `ezbx build` builds from pinned versions in `ezbx.lock`, with the tool packs you chose ([docs/build.md](docs/build.md)) |
| The box | `ezbx up` / `down`, `ezbx shell [harness]`; new in 0.4: `ezbx harness add\|remove\|list` |
| Model servers | Any server with an OpenAI- or Anthropic-compatible API, or LM Studio's own (`openai`, `anthropic`, `lmstudio`; [docs/engine-adapters.md](docs/engine-adapters.md)); `ezbx engine detect` / `use`. All seven harnesses [select the `default` alias](docs/getting-started.md#choosing-the-default-model) when a default model is configured. |
| Network | `ezbx net off\|allow\|on`, `ezbx net allow HOST`; the box's only way out is the gateway; new in 0.4: `airgap` |
| Read-only folders | Host folders the agent may read at `/readonly/<name>`, with secret files masked ([docs/readonly.md](docs/readonly.md)) |
| Undo | `ezbx snapshot` / `review` / `rewind`; `ezbx shell` takes a snapshot first ([docs/snapshots.md](docs/snapshots.md)) |
| For the human | `ezbx admin shell` (a separate root container), `ezbx reset` (with a backup), `ezbx destroy` (never the workspace), `ezbx doctor` ([docs/admin.md](docs/admin.md)) |
| Several boxes (new in 0.4) | `[box.<name>]` sections in one `ezbx.toml`, sharing the model server; `--box NAME` |
| For other programs | `ezbx version --json`, `create`, `start`, `exec`, `status`, `stop`, `destroy --json`, and `ezbx events --jsonl` for live events and recent replay. ([docs/runtime-contract.md](docs/runtime-contract.md)) |
| Logging | The gateway logs every request without its content ([docs/gateway.md](docs/gateway.md)); new in 0.4: token counts |

### Where it runs

| Platform | State |
|---|---|
| Windows 11 + Docker Desktop (Engine 28+) | Verified: every command, all seven harnesses, the isolation tests |
| Ubuntu 24.04 `amd64` + local Docker Engine | CI pilot with Pi, OpenCode, and a fake model server ([setup and limits](docs/getting-started.md#native-linux-pilot)) |
| Other Linux setups, macOS | Experimental builds ([threat model](docs/threat-model.md)) |

## What's next

Before `1.0.0`, the focus is the ezbx runtime, platform support, and reliability for the seven established harnesses. New built-in harnesses and support for registering custom harnesses are planned for after `1.0.0`.

- **v0.6.2** improves recovery from an incomplete box, fixes intermittent gateway event reader exits on Windows, and tests all seven harnesses with a fake model server on Ubuntu 24.04 and 26.04 `amd64`. The Pi fixture checks event reconnect and retention recovery from a caller's perspective.
- **v0.7.0** is planned to expand testing to macOS.
- Cloud API keys through the gateway, kept out of the box like local keys.
- Winget (the package is waiting for review).

## Releases

`v0.1.0`/`v0.1.1` (setup, the gateway and network switch, snapshots, the harnesses), `v0.2.0` (`allow` mode, more model servers, `reset` with backup), `v0.3.0`/`v0.3.1` (read-only folders with secret masking, model servers by API kind, starter profiles), `v0.4.0` (several boxes, `airgap`, the runtime contract), `v0.5.0` (runtime events, replay, and harness selection of the `default` alias), `v0.5.1` (engine-free airgap startup), `v0.6.0` (native Linux pilot), `v0.6.1` (first-run recovery, gateway events, and OpenCode in the Ubuntu pilot). The milestones are in [docs/design.md](docs/design.md), section 12. See [Releases](https://github.com/OranjSvensk/ezbx/releases) for notes and downloads.

## Safety note

A container is a strong boundary but not a perfect one. It shares a kernel with its host, and anything you mount into it can be changed by the agent. What ezbx protects, against whom, and the risks it leaves to you are in the [threat model](docs/threat-model.md). The most important one: check what the agent wrote (`ezbx review --diff`) before you run it outside the box.

## Contributing and security

Contributions will open once the design settles. To report a security problem, see [SECURITY.md](SECURITY.md).

## License

MIT. Copyright (c) 2026 Ryan Ven (OranjSvensk https://github.com/OranjSvensk).
