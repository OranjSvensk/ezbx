<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark-light.svg" alt="ezbx" width="260" height="64">
  </picture>
</h1>

**Boxed-up local AI agents.**

ezbx is a planned one-click setup for running AI coding and agent tools inside a sandboxed container, while the model itself keeps running on your own machine.

> **Status: early release (`v0.2.0`).** All seven harnesses — Pi, OpenCode, oh-my-pi, Codex CLI, Claude Code, Hermes Agent and OpenClaw — are built, pinned in `ezbx.lock`, and verified end to end on Windows 11 with Docker Desktop and a local model server (including file tools in `/workspace` with `network = "off"`). Every command on the road to 0.1 below is built. Since `v0.2.0`, `main` also has read-only folders with secret masking ([docs/readonly.md](docs/readonly.md)), tested end to end, and works with any model server by the API it speaks ([docs/engine-adapters.md](docs/engine-adapters.md)); the `[engine]` section of `ezbx.toml` changed with it. Linux, macOS and cloud keys come next. Test results are in [docs/isolation-test.md](docs/isolation-test.md), and designs are in [docs/](docs/).

```text
# Scoop (Windows):
scoop bucket add ezbx https://github.com/OranjSvensk/scoop-bucket
scoop install ezbx

# Or install with Go 1.27+ (or download a binary from Releases):
go install github.com/OranjSvensk/ezbx/cmd/ezbx@v0.2.0
ezbx --version
ezbx init                 # asks the questions, shows the plan, builds and starts the box
ezbx shell pi             # opens Pi in the box (or opencode, omp, codex, claude, hermes, openclaw)
```

New here? Read the step-by-step guide in [docs/getting-started.md](docs/getting-started.md). Other commands: `ezbx up` and `ezbx down` start and remove the box described in `ezbx.toml` (see [ezbx.toml.example](ezbx.toml.example)), `ezbx build` builds the images, `ezbx review` and `ezbx rewind` show and undo what changed in the workspace since a snapshot, `ezbx net` shows and switches the network and approves hosts, `ezbx engine detect` and `ezbx engine use` find a model server and point the box at it, and `ezbx doctor` checks the whole setup and explains any fix.

## The idea

- **The model stays on your machine.** Whatever program serves your model keeps full access to your GPU and models. ezbx works with any server that offers an OpenAI- or Anthropic-compatible API (with better model facts from LM Studio's own API), so there is no list of supported engines.
- **The work stays in the box.** Agents run in a container and can only write to folders you choose.
- **One stable address between them.** A small gateway lets the box talk to your model server, so moving to another server or port doesn't mean reconfiguring every tool.
- **You choose the harnesses.** Pick from Pi, OpenCode, oh-my-pi, Codex CLI, Claude Code, Hermes Agent and OpenClaw, in any combination. Each one's install method is verified and pinned in `ezbx.lock`.
- **Network on or off.** A switch controls whether the agent can reach the internet.
- **Root stays with you.** The agent runs as an unprivileged user. Admin access is reserved for the human.

## Planned first version

1. An `ezbx init` setup that finds your model server and asks which harnesses to install.
2. The gateway and the network switch.
3. Snapshots of the writable folder, so an agent session can be undone.

## Road to the first usable version

The goal: set up Pi with a local model, from nothing, using only `ezbx` commands, with every planned safety feature. Other harnesses and cloud keys come after.

| Command | What it does | Status |
|---|---|---|
| `ezbx init` | Finds the model server, asks the questions, writes `ezbx.toml`, shows the plan first | done |
| `ezbx build` | Builds the images from pinned versions listed in `ezbx.lock`, with the tool packs you chose ([docs/build.md](docs/build.md)) | done |
| `ezbx up` / `ezbx down` | Start and remove the box | done |
| `ezbx shell` / `ezbx shell pi` | A terminal in the box, or Pi | done |
| `ezbx net off\|allow\|on`, `ezbx net allow HOST` | The network switch and approvals | done |
| `ezbx engine use` | Point a running box at the model server again, for example after it moves to a new port | done |
| `ezbx snapshot` / `rewind` / `review` | Undo an agent's changes to the workspace; `ezbx shell` takes a snapshot first ([docs/snapshots.md](docs/snapshots.md)) | done |
| `ezbx admin shell` | A root shell for the human, in a separate, temporary container ([docs/admin.md](docs/admin.md)) | done |
| `ezbx reset` / `destroy` | A fresh agent home after a backup, or remove everything ezbx made (never the workspace) | done |
| `ezbx doctor` | Check the whole setup and explain fixes | done |

## Safety note

A container is a strong boundary but not a perfect one. It shares a kernel with its host, and anything you mount into it can be changed by the agent. What ezbx protects, against whom, and the risks it leaves to you are in the [threat model](docs/threat-model.md). The most important one: check what the agent wrote (`ezbx review --diff`) before you run it outside the box.

## Contributing and security

Contributions will open once the design settles. To report a security problem, see [SECURITY.md](SECURITY.md).

## License

MIT. Copyright (c) 2026 Ryan Ven (OranjSvensk https://github.com/OranjSvensk).
