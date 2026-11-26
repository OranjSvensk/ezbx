# ezbx design (draft 0.1)

Status: `v0.5.0` early release with live runtime events and recent replay (section 12). Verified on Windows 11 with Docker Desktop and a local model server across all seven harnesses: `pi`, `opencode`, `omp`, `codex`, `claude`, `hermes`, `openclaw`. Built: read-only folders with secret masking ([readonly.md](readonly.md)), starter profiles ([profiles.md](profiles.md)), model server detection by API kind (`openai`, `lmstudio`, `anthropic`; decision 22), the gateway (model route, OpenAI Responses and Anthropic Messages translation, egress proxy, admin API), config rendering, `ezbx doctor`, `ezbx up`/`down`, `ezbx shell`, `ezbx snapshot`/`review`/`rewind`, `ezbx admin shell`, `ezbx reset`/`destroy`, `ezbx net`, `ezbx engine use`, `ezbx init`, and `ezbx build` with `ezbx.lock` and tool packs ([build.md](build.md)).

## 1. Goal

Make it easy to run AI agent tools in a sandbox while the model itself keeps running on your own machine.

- **Power on the host.** Your model server, whatever program runs it, uses your GPU and models directly.
- **Work in the box.** Agents run in a container and write only to folders you choose.
- **Easy.** One setup flow, a small command-line tool, and a switch for network access.

### Non-goals (for now)

- Running the model inside the container.
- Being a hardened, multi-tenant security product. The box reduces risk. It doesn't remove it. See the threat model.
- Supporting every agent tool. Each harness is added only after its install method is verified.

## 2. Architecture

```text
  Host (your PC)                           Docker
 ┌─────────────────────────┐   ┌────────────────────────────────────────────┐
 │ Model server            │   │  "inside" network  (no internet route)     │
 │  OpenAI- or Anthropic-  │   │  ┌──────────┐        ┌──────────────────┐  │
 │  compatible API         │◄──┼──┤ gateway  │◄───────┤ box (harnesses)  │  │
 │                         │   │  └────┬─────┘        └──────────────────┘  │
 └─────────────────────────┘   │       │ "outside" network (internet)       │
                               └───────┼────────────────────────────────────┘
                                       ▼
                                   the internet (only through the gateway)
```

Three parts:

1. **API kinds** (host side): recognise the model server by the API it speaks and read its models.
2. **Gateway** (container): the only way out of the box. It carries model traffic and, depending on the network mode, internet traffic.
3. **Box** (container): where harnesses run, as an unprivileged user.

### Why the network is enforced by topology

The box sits only on an internal Docker network, which has no route to the internet or to the host. The gateway sits on that network and on a normal one. If the agent ignores proxy settings, it still has no other route. Environment variables alone could be changed by the agent. Network layout can't.

The inside network must be created with `--internal` **and** the bridge option `gateway_mode_ipv4=isolated` (Docker Engine 28 or later), with IPv6 off. A plain internal network still gives its bridge an address that belongs to Docker Desktop's VM, and the box could reach services listening there. With the option, a test probe on Docker Desktop reached the gateway's model port and nothing else: no internet, host, LAN, Docker Desktop addresses, other containers or outside DNS. The same held from inside the first box image, where Pi reached the model through the gateway. The method, results and limits are in [isolation-test.md](isolation-test.md). Note that the box can reach every port the gateway container listens on, so anything that must stay private to the host cannot listen on the gateway's network interfaces. The admin API therefore listens only on the gateway container's own loopback and is reached with `docker exec` (decision 7; see [gateway.md](gateway.md), section 2).

## 3. The gateway

One small service that does four jobs:

| Job | Detail |
|---|---|
| **Model route** | Exposes one stable OpenAI-compatible address inside the box, for example `http://gateway:8000/v1`, and forwards to the model server in `ezbx.toml`. Moving to another server changes one setting and no harness config. |
| **Key injection** | Keys stay on the host and the agent never sees a real one. Built for the model server's key on the model route. Cloud API keys will use one reverse route per provider on the gateway (decision 8, not built yet), because the egress proxy never decrypts HTTPS and so cannot add a key to a tunnelled request. |
| **Egress control** | A forward proxy that applies the current network mode (section 5). |
| **Request log** | Records model, latency, how each request ended, the token counts a reply reports, and blocked proxy requests. |

Decided: the gateway is one small Go binary, shipped in a minimal image and sharing code with the CLI. The full design is in [gateway.md](gateway.md).

## 4. Model servers and API kinds

ezbx works with any model server by the API it speaks, not by the program that runs it (decision 22). Each API kind is a small data file, `providers/<id>/adapter.json`: `openai` (any OpenAI-compatible server), `lmstudio` (the same, plus LM Studio's own model list for context sizes and leaving out embedding models), and `anthropic` (any Anthropic-compatible server: `/v1/messages` passes through and `/v1/chat/completions` and `/v1/responses` are translated to it). The format is described in [engine-adapters.md](engine-adapters.md).

- **Where:** `url` under `[engine]` in `ezbx.toml` is the one address `ezbx up` and `ezbx doctor` contact. `ezbx init` and `ezbx engine detect` look at a few common local ports to offer one.
- **Which API:** `api = "auto"` uses the most specific kind the server answers; naming a kind makes `up` refuse a server that doesn't speak it.
- **Discover:** read the model list, so no model name is ever hardcoded.
- ezbx doesn't start, stop or unload models: that stays with the server and you.

### Host reachability

Containers reach the host through a host alias. On Docker Desktop this works for a server bound to loopback (tested, [isolation-test.md](isolation-test.md)). On other setups it may not work. The gateway owns this detail, and `ezbx doctor` tests it.

## 5. Network modes

| Mode | Model route | Internet |
|---|---|---|
| `airgap` | no | none |
| `off` | yes | none |
| `allow` | yes | only domains on the allowlist (package registries, the code host, and any API a chosen cloud harness needs) |
| `on` | yes | the public internet (local networks and the host stay blocked) |

- Switching modes only changes the gateway's rules, so running sessions continue.
- A mode switched with `ezbx net` lasts until the next `ezbx up` or gateway restart, which go back to the mode in `ezbx.toml`, so a forgotten `on` never outlives its session. Hosts approved with `ezbx net allow` are saved in the box's state folder and kept (decision 11).
- Each harness manifest lists the domains it needs, and `allow` merges the lists of the installed harnesses.
- Cloud-backed harnesses won't work in `off`. `ezbx doctor` says so.
- `airgap` closes the model route too, for commands that need no model at all, such as tests, builds and linters. Switching a running box to it ends model requests in flight (decision 24).
- The default allowlist is tiny: package registries and the code host. Everything else is added on demand. The gateway logs blocked requests, and `ezbx net allow <domain>` approves one. Allowlists are plain data files, so people can contribute to them.

## 6. The box

- **User:** an unprivileged `agent` user. No `sudo`, no setuid tricks, `no-new-privileges` on, all Linux capabilities dropped.
- **Writable:** only the workspace folder you choose (`/workspace`) plus the agent's own home volume.
- **Optional read-only folders:** you may expose extra host folders read-only at `/readonly/<name>`. Common secret files are masked with empty overlays, and ezbx warns about other secret-like files in anything exposed. See [readonly.md](readonly.md).
- **No Docker socket**, ever.
- **Resource limits:** memory, CPU and process limits with sensible defaults, adjustable.
- **Admin access:** reserved for the human. `ezbx admin shell` starts a separate, short-lived admin container from the same image, because root in the main box has no capabilities. It mounts the workspace and the agent's home, has no network unless asked, and nothing else in it lasts: programs for the agent come from the tool catalogue and `ezbx build`, so images stay pinned. See [admin.md](admin.md).

Future option: a read-only root filesystem with tmpfs for scratch space.

## 7. Harnesses

Each harness is a folder, `harnesses/<id>/`, with a `manifest.json` and an install script. The format is described in [harness-manifest.md](harness-manifest.md), and `harnesses/pi/manifest.json` is a complete example.

- **Verified:** nothing ships without a recorded install check.
- **Pinned:** versions are pinned and recorded in `ezbx.lock`, so a rebuild gives the same box (see [build.md](build.md)).
- **Tools:** programs such as ripgrep, Chromium or FFmpeg come from a catalogue of verified tools and tool packs, chosen in `ezbx.toml` and built into the image. The agent can't install programs itself.
- **Config generation:** the harness template is filled from the model server's model list (in JSON, TOML or YAML), so each harness points at the gateway automatically.
- **Included harnesses:** Pi, OpenCode, oh-my-pi (a separate project from Pi), Codex CLI, Claude Code, Hermes Agent and OpenClaw.
- **Autonomy flag:** each harness's "skip approvals" mode is set only inside the box and is recorded in its manifest.

## 8. Snapshots and rewind

Before each session, ezbx snapshots the workspace into a store in the box's state folder, outside the box's mounts, so the agent can't edit its own history. `ezbx rewind` restores a snapshot (the newest by default) after taking one of the current state, so a rewind can be undone too. `ezbx review` shows what changed since a snapshot. Built; see [snapshots.md](snapshots.md).

## 9. Command line

```text
ezbx init               guided setup: engine, harnesses, folders
ezbx create             add a box to ezbx.toml without asking anything (for programs)
ezbx up | down          start or stop the gateway and box (start | stop for programs)
ezbx shell [harness]    open a terminal in the box, or launch a harness
ezbx exec -- COMMAND    run one command in the box without a terminal
ezbx status             missing, stopped, running or unhealthy
ezbx events --jsonl     follow the box's runtime activity, with optional replay
ezbx net airgap|off|allow|on   change the network mode
ezbx engine detect|use
ezbx harness add|remove|list
ezbx snapshot | rewind | review
ezbx admin shell        human-only root shell in a temporary admin container
ezbx doctor             check Docker, engine, gateway, mounts and network
ezbx reset              a fresh agent home and containers; backs up first and refuses to continue if the backup fails
ezbx destroy            remove everything ezbx made for a box, never the workspace
```

Programs that drive ezbx use `--json` on these commands; the contract is in [runtime-contract.md](runtime-contract.md).

Language: Go, built as one cross-platform binary. The first milestone targets Windows with Docker Desktop, and the code avoids Windows-only assumptions so Linux can follow.

## 10. Configuration and privacy

- `ezbx.toml.example`: committed, placeholders only.
- `ezbx.toml`: your real config, in the per-user config folder (decision 9), outside the repo. Unknown settings are refused (decision 10).
- `ezbx.local.toml`: gitignored, for a config kept inside a clone of the repo (`--here`, later).
- `ezbx.lock`: committed. Pinned harness and image versions.
- Secrets live in environment variables on the host in 0.1 (the host's credential store later), never in the repo or the image.
- The repo has a pre-commit and CI privacy scan. Docs use placeholder paths such as `C:\Users\Example`.

## 11. Threat model

The full threat model is in [threat-model.md](threat-model.md): what is protected, against whom, each boundary with its controls and tests, and the risks that remain. In short, the box protects against agent changes outside the workspace, unrequested network access, and the agent holding the engine's key. It doesn't fully protect against a container escape, against the agent sending what it can read to a host the network mode allows, or against you running, outside the box, something the agent wrote in the workspace.

## 12. Milestones

1. **0.1 (done, `v0.1.0`):** `init`, engine detection for Strata and one other engine, the gateway with the model route and `off`/`on`, one harness, snapshots.
2. **0.2 (done, `v0.2.0`):** `allow` mode, more engines and harnesses, `doctor`, `reset` with backup and restore. The remaining harnesses and `review`, first planned for 0.3, came early.
3. **0.3 (done, `v0.3.1`):** read-only folders with secret masking and the full threat model, model servers by API kind (decision 22: `openai`, `lmstudio`, `anthropic`), starter profiles (`offline`, `open`, `python-ml`, `research`, `web-dev` in [profiles.md](profiles.md)).
4. **0.4 (done, `v0.4.0`):** the runtime contract's phase 1 for programs that drive ezbx ([runtime-contract.md](runtime-contract.md): `version --json`, `status`, `create`, `start`, `exec`, `stop`, `destroy --json`), several boxes in one `ezbx.toml` (decision 23), the `airgap` mode (decision 24), token counts in the gateway's log (decision 25), the `default` model alias (decision 26), `ezbx harness add|remove|list`, and `destroy` limited to its box (decision 27).
5. **0.5 (done, `v0.5.0`):** `ezbx events --jsonl` with bounded replay and a destroy receipt (decision 28), harness selection of the `default` alias, and repeatable Windows Docker acceptance testing with the fake engine.
6. **Later:** cloud API keys (decision 8), Linux and macOS, tray or GUI front end, other container runtimes, a tool-calling self-test for models.

**Before `1.0.0`**, keep the seven established harnesses and prioritize improvements to the ezbx runtime, platform support, and reliability. Maintain and verify the existing integrations as the runtime changes. Individual release scope is decided separately.

**After `1.0.0`**, revisit adding more built-in harnesses and letting people register custom harnesses.

## 13. Decisions

| # | Question | Decision |
|---|---|---|
| 1 | Language | Go, for the CLI and the gateway. One static binary, easy cross-compiling, and a standard library that covers the proxy work. |
| 2 | Allowlist | Tiny default list plus each harness's domains. Blocked requests are logged and approved with `ezbx net allow`. Lists are data files. |
| 3 | Platforms | Windows with Docker Desktop first. No Windows-only assumptions in the code. Linux next, macOS later. |
| 4 | Engines bound to loopback outside Docker Desktop | Version 0.1 supports Docker Desktop only. `ezbx doctor` detects the problem and explains the fix. A proper helper comes later. |
| 5 | Boxes | One box in 0.1 holding many projects. The config format allowed several named boxes from the start; 0.4 runs them (decision 23). |
| 6 | Second harness | Pi first, then OpenCode 2, pinned at an exact version. It supports a custom OpenAI-compatible provider, which points at the gateway. Its npm launcher needs its install script, so the lockfile pins its prebuilt Linux x64 baseline program instead ([build.md](build.md)). |
| 7 | Reaching the admin API | It listens only on the gateway container's own loopback, with no published port. The CLI calls it through `docker exec`, and a token is still required. The box has no network path to it. See [gateway.md](gateway.md), section 2. |
| 8 | Cloud API keys | One reverse route per cloud provider on the gateway, built like the model route. The harness points its base address at the gateway, and the gateway adds the key and forwards to that provider's fixed address. Each provider gets a small adapter for its key style. Only API-key providers are covered; account logins that refresh their own tokens are a separate case for later. Built after the isolation proof, since 0.1's first harness uses a local model. |
| 9 | Where config and state live | Windows: `%LOCALAPPDATA%\ezbx` holds `ezbx.toml`, and `%LOCALAPPDATA%\ezbx\state\<box>` holds the admin token and logs (snapshots later). Not the Roaming folder, which a domain profile copies between machines; the token, the logs and the machine's own paths should stay on this one. macOS: `~/Library/Application Support/ezbx`, the same layout. Linux: `$XDG_CONFIG_HOME/ezbx` and `$XDG_STATE_HOME/ezbx` (`~/.config/ezbx` and `~/.local/state/ezbx`). `EZBX_HOME` moves both, for tests. The state folder is never mounted into the box, and the workspace may not overlap it. On Windows the token's file mode means little; what protects it is that the user's profile folder is private to that user and to administrators. |
| 10 | Config format | TOML, read with [BurntSushi/toml](https://github.com/BurntSushi/toml) (MIT, no dependencies of its own), ezbx's first outside module, pinned in `go.sum`. Unknown settings are refused, so a typo is an error. Sections: `[box.<name>]`, `[engine]` with `[engine.<id>]` overrides, `[models."<id>"]` for facts engines don't report, and `[gateway]`. A parser of our own could replace the module later if we want no dependencies. |
| 11 | Changes to a running box | `ezbx net off\|allow\|on` and `ezbx engine use` change the running gateway only. The next `ezbx up`, or a restart of the gateway (Docker Desktop restarting, say), goes back to `ezbx.toml`: the safe default wins, so a forgotten `net on` doesn't survive. Hosts approved with `ezbx net allow HOST` are kept: they are saved in `approved-hosts` in the box's state folder, which is mounted read-only into the gateway (never the box) and read with `--allow-file` each time it starts. `ezbx net remove HOST` forgets one. |
| 12 | Where the image build files come from | Carried inside the `ezbx` program: the Dockerfiles, harness install files, the tool catalogue and the gateway's Go source. `ezbx build` needs no copy of the repository. See [build.md](build.md). |
| 13 | `ezbx.lock` and what `up` accepts | `ezbx.lock` is committed and generated from the files: base image digests, harness versions and lockfile hashes, tools. What was built on a computer goes in `images.json` next to `ezbx.toml`. `up` refuses images not built from the current inputs. |
| 14 | Image names and old images | Each image is named after a hash of its inputs (`ezbx-box:<12 hex digits>`). Rebuilds keep the old images; `ezbx build --prune` removes older ones after asking, keeping the current images and the ones they replaced. |
| 15 | Snapshots | A store of ezbx's own in the state folder (content kept once, compressed, by hash; a list per snapshot), not git, which keeps a nested git repository only as a pointer. Taken at every `ezbx shell` and by hand. Rebuildable folders and files over 100 MB left out; the last 20 automatic ones kept. `rewind` snapshots first, then matches the workspace exactly, and refuses while a session runs. See [snapshots.md](snapshots.md). |
| 16 | Admin shell | A temporary root container from the box image, with Docker's normal capabilities (never privileged), no Docker socket, the workspace and agent home mounted, home `/root`, no network unless `--net`. Nothing outside the mounts lasts; programs come from tools and `ezbx build`. See [admin.md](admin.md). |
| 17 | `reset` and `destroy` | `reset` backs up the config, approved hosts and the agent's home (refusing if that fails), then gives a fresh home and containers; workspace, snapshots, approved hosts, config and images are kept; `--restore ID` undoes it. `destroy` removes the containers, networks, home volume, state folder (snapshots too) and ezbx's images, never the workspace, keeps `ezbx.toml` unless `--remove-config`, and asks you to type the box name. See [admin.md](admin.md). |
| 18 | The engine's key | `key_env` in `ezbx.toml` stays the only way to give it. `ezbx up` writes it to `engine-key` in the state folder, mounted read-only into the gateway only, which reads it with `--key-file`; `ezbx down` removes it. Nothing in `docker inspect` shows it. `ezbx engine use` keeps refusing keyed engines. See [gateway.md](gateway.md), section 3. |
| 19 | Detection and the engine's key | Detection sends a key only to the exact `url` configured for that engine, never to hint ports, status-command addresses or other engines, and never through a redirect, so `key_env` without `url` is refused. A 401 or 403 from an address that matches no adapter is reported as asking for a key (or refusing the one sent), not as nothing found. The doctor sends no key; the gateway's `engine ready` check covers such an engine. Chosen over sending no key ever (an engine that guards its model list could then never be used) and over sending it to every candidate (any local program on a hint port would get it). |
| 20 | Which harnesses a box image holds | Only the box's own (`harnesses` in `ezbx.toml`), so each box carries what it uses; the list is part of the image's name. The box Dockerfile has one harness stage that runs the install script of every harness folder in the build files, so a new harness needs no Dockerfile change. A harness's `env` is set on the box container by `ezbx up`. Chosen over building every verified harness into every box (OpenCode alone adds about 250 MB). See [build.md](build.md). |
| 21 | Read-only folders and secret masking | Built. A read-only bind mount at `/readonly/<name>`, with an empty read-only overlay over each file or folder that matches a mask; a built-in mask list that `mask` adds to and only `unmask` can lift; content warnings rather than refusals; `ezbx shell` refuses a session when a new secret file isn't masked yet; the workspace's folder refusals plus no overlaps; network warnings, not refusals. See [readonly.md](readonly.md). |
| 22 | Which model servers ezbx supports | Any server, by the API it speaks: an API kind (`openai`, `lmstudio`, `anthropic`) is a data file of probes and model-fact paths, with no product names, status commands or start, stop and unload commands. `[engine]` holds `url`, `api` (`auto` by default), `key_env` and `host`; the 0.2 form (`engine.use`, `[engine.<id>]` tables) is refused with a message showing the new one. `ezbx up`, `ezbx engine use` and `ezbx doctor` contact only the configured address and the gateway's upstream; only `ezbx init` and `ezbx engine detect` look at common local ports. A new address drops the old `key_env` and `host`. Chosen over adapters per engine program: ezbx would have to keep verifying each program one by one, while the model server is yours to choose. The `anthropic` kind (passing `/v1/messages` through and translating `/v1/chat/completions` and `/v1/responses` to it) is built. |
| 23 | Several boxes | `ezbx.toml` may hold several `[box.<name>]` sections, sharing one `[engine]`. Each has its own containers, networks, home volume, state folder and image. A command acts on the only box, or on the one `--box` names; with several, `--box` is required rather than guessed. Two boxes may not have the same workspace or one inside the other, because each snapshots and rewinds its own. `ezbx init --box NAME` and `ezbx create` add one and keep the others; `ezbx build` builds every box's image (or `--box`'s), and `--prune` keeps every box's current one. Chosen over one config file per box (the engine settings would be copied into each) and over keeping one box (a program running several agents needs several). |
| 24 | A mode without the model | `airgap` joins `off`, `allow` and `on`: the gateway refuses the model route (403, `network_airgap`) as well as everything through the proxy, and a switch to it ends model requests in flight. `off` keeps its meaning (the model only), so no existing config changes. The box still reaches the gateway's ports, which refuse it; taking the gateway off the box's network would be stronger, but switching modes while the box runs would then mean reconnecting networks. Chosen over renaming `off` to `local-model-only` and using `off` for this mode, which would have given one word two meanings. |
| 25 | Token counts in the log | The gateway reads `usage` from what it sends the box (JSON, or a stream's events, in OpenAI's or Anthropic's shape) and logs `tokens_in` and `tokens_out`, never the content. When it translates a stream, it asks the server for usage (`stream_options.include_usage`, part of OpenAI's API) and passes the real numbers on; a server that refuses the option with 400 is asked once more without it. |
| 26 | A `default` model | `default_model` under `[engine]` names a model; a request for the model `default` gets it, on every route, and the log and `X-Ezbx-Model` name the real one. It belongs to the server, so a new address drops it, like `key_env`. Without it, `default` reaches the engine as it is. |
| 27 | What `destroy` removes | Everything for its box, and only the images that box needs that no other box in `ezbx.toml` needs and no container uses; `--images` removes every unused ezbx image, as 0.3 did. `--remove-config` takes out the box's section, or the whole file when it was the only box. Chosen so destroying one agent's box never breaks another's. |
| 28 | Runtime event source and retention | The ezbx CLI writes a bounded host-side journal per box. Explicit records cover lifecycle actions, exec attempts, network mode changes, and snapshots. A successful destroy replaces the prior journal with one removal receipt eligible for 24 hours of replay. See [runtime-contract.md](runtime-contract.md). |

## 14. Open questions

1. ~~Exact format of `ezbx.toml`.~~ Decided: decision 10, and [ezbx.toml.example](../ezbx.toml.example).
2. ~~How `ezbx init` presents the choices.~~ Decided in [init-flow.md](init-flow.md): plain prompts first, every question also a flag.
3. ~~Where snapshots and the gateway log live on disk by default.~~ Decided in [init-flow.md](init-flow.md): per-user config and state folders, never inside the workspace.
4. How the gateway proves which engine and model it is forwarding to, so `doctor` can confirm the path end to end. (The `X-Ezbx-*` reply headers are a first answer; see [gateway.md](gateway.md).)
5. ~~How cloud API keys are injected.~~ Decided: a reverse route per cloud provider on the gateway (decision 8).
