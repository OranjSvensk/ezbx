# Getting started with ezbx

This guide walks through installing ezbx, setting up your first sandboxed box with `ezbx init`, running AI agent harnesses inside it, undoing workspace changes with snapshots, and keeping things healthy.

> **Current platform scope:** ezbx is built and verified on **Windows 11 with Docker Desktop** (Docker Engine 28 or later). Linux and macOS support is planned next.

---

## 1. Before you start

You need three things on your computer:

1. **Docker Desktop** (with Docker Engine 28 or later), running. ezbx never installs or starts Docker for you.
2. **Go 1.27 or later**, to build `ezbx` from source.
3. **A local inference engine** (such as Strata, LM Studio, Ollama or llama.cpp) running on your computer with at least one model loaded. You can still run setup without a running engine and start the model later.

---

## 2. Install ezbx

Clone the repository and install the `ezbx` command into your Go bin folder:

```text
git clone https://github.com/ryan-ven/ezbx.git
cd ezbx
go install ./cmd/ezbx
```

Check that `ezbx` is on your `PATH`:

```text
ezbx --help
```

---

## 3. Set up your first box (`ezbx init`)

Start Docker Desktop and your inference engine (with a model loaded), then run:

```text
ezbx init
```

To preview what `ezbx init` will ask and create without changing anything on disk, run `ezbx init --plan`.

`ezbx init` walks you through six steps and shows the full plan before touching anything:

1. **Inference engine:** `ezbx init` detects running engines on your computer and picks the first match by default. If your engine is on a custom port or requires an API key, pass `--engine`, `--url` and `--key-env`:
   ```text
   ezbx init --engine strata --url http://127.0.0.1:PORT --key-env STRATA_API_KEY
   ```
   The key itself is never written into `ezbx.toml` or container images — ezbx only reads the named environment variable when starting the gateway.
2. **Reasoning models:** Engines don't report whether a model thinks before answering, so `init` asks per model (`Is <model> a reasoning model?`). Answer `y` for reasoning models so their output isn't cut off mid-thought.
3. **Harnesses:** Choose one or more harnesses by number (coding tools first, assistant tools after):

   | ID | Name | Kind | Approval-skip flag (inside the box) |
   |---|---|---|---|
   | `pi` | Pi | coding | none needed |
   | `opencode` | OpenCode | coding | none needed |
   | `omp` | oh-my-pi | coding | none needed |
   | `codex` | Codex CLI | coding | `--dangerously-bypass-approvals-and-sandbox` |
   | `claude` | Claude Code | coding | `--dangerously-skip-permissions` |
   | `hermes` | Hermes Agent | assistant | `--yolo` |
   | `openclaw` | OpenClaw | assistant | configured in `~/.openclaw/openclaw.json` |

   Only the harnesses you choose are built into your box image. Picking fewer keeps the image smaller (Pi alone with `pack:coding` is ~726 MB; all seven harnesses without extra packs is ~4.28 GB).
4. **Tools and tool packs:** Built into the image at build time because the agent cannot install OS packages inside the box:
   - `pack:coding` (default for new configs): `curl`, `jq`, `less`, `make`, `python3`, `unzip`
   - `pack:web`: headless `chromium`
   - `pack:media`: `ffmpeg`, `imagemagick`
   - Or list single tools (`pack:coding,ffmpeg`) or `none`.
5. **Workspace folder:** Choose the folder that will be mounted at `/workspace` inside the container. This is the **only** host folder the agent can edit. ezbx refuses system folders, drive roots, your home folder itself, and its own config/state folders.
6. **Network mode and memory limit:**
   - `off` (default): no internet access; only the model route through the gateway works.
   - `allow`: only approved hostnames (such as `registry.npmjs.org`) are reachable.
   - `on`: public internet allowed; local/private/loopback/metadata addresses remain blocked.
   - Memory defaults to half of Docker's allocated memory (from `2g` to `8g`), and CPU/process limits protect the host.

At the review screen, answer `y` to write `ezbx.toml`, build the gateway and box images (`ezbx build`), start the containers (`ezbx up`), and run all isolation and connectivity checks (`ezbx doctor`).

---

## 4. Daily use: opening a harness and undoing changes

Start any installed harness inside the box as the unprivileged `agent` user:

```text
ezbx shell pi
ezbx shell opencode
ezbx shell codex -- --dangerously-bypass-approvals-and-sandbox
ezbx shell claude -- --dangerously-skip-permissions
ezbx shell hermes -- --yolo
ezbx shell omp
ezbx shell openclaw
```

Arguments after `--` are passed straight to the harness (for example `ezbx shell pi -- -p "explain main.go"`). Run `ezbx shell` with no harness name for a plain `bash` shell in `/workspace`.

### Automatic snapshots, `review`, and `rewind`

Every `ezbx shell` automatically snapshots your workspace before the session starts (keeping the last 20 automatic snapshots, excluding rebuildable folders like `node_modules` and `.venv` and files over 100 MB). Snapshots live in ezbx's state folder on your host — never inside the container — so the agent cannot alter its own history.

- **See what the agent changed:**
  ```text
  ezbx review
  ezbx review --diff
  ```
- **Undo the agent's changes:**
  ```text
  ezbx rewind
  ```
  `rewind` stops if a shell session is still running, takes a safety snapshot of the current workspace first (so a rewind can itself be undone), and puts the workspace back.
- **Manual undo points:**
  ```text
  ezbx snapshot -m "before big refactor"
  ezbx snapshot list
  ezbx snapshot remove <ID>
  ```

---

## 5. Controlling network access (`ezbx net`)

When `network = "off"` or `"allow"`, the gateway logs every blocked outbound connection. Inspect or change network rules while the box runs:

```text
ezbx net                          show current mode, allowed hosts, and blocked attempts
ezbx net allow registry.npmjs.org approve a host (saved across restarts)
ezbx net remove registry.npmjs.org
ezbx net allow                    temporarily switch to allow mode (until next ezbx up)
ezbx net on                       temporarily allow public internet (until next ezbx up)
ezbx net off                      return to off mode
```

Temporary mode switches reset to `ezbx.toml` on the next `ezbx up`, so a forgotten `ezbx net on` never persists.

---

## 6. Switching engines, updating tools, and troubleshooting

### Common lifecycle commands

| Task | Command |
|---|---|
| Stop containers (keeps workspace, snapshots, and home volume) | `ezbx down` |
| Start containers | `ezbx up` |
| Find running engines | `ezbx engine detect` |
| Point running box at another engine or port | `ezbx engine use <id>` (or `--url http://127.0.0.1:PORT`) |
| Run full security, isolation, and model checks | `ezbx doctor` |
| Open a temporary root shell (human-only, never the agent) | `ezbx admin shell` (add `--net` for temporary network) |
| Wipe the agent home after taking a full backup | `ezbx reset` (`--restore <ID>` to undo) |
| Remove all ezbx containers, volumes, state, and unused ezbx images | `ezbx destroy` (never touches your workspace) |

### Adding harnesses or tools later

Edit `harnesses` or `tools` in your `ezbx.toml` (or re-run `ezbx init`), then rebuild and recreate the box:

```text
ezbx build
ezbx down
ezbx up
```

To free disk space from older ezbx image builds afterwards:

```text
ezbx build --prune
```

### Where ezbx stores its files

| What | Windows default | macOS / Linux |
|---|---|---|
| Config (`ezbx.toml`) and build record (`images.json`) | `%LOCALAPPDATA%\ezbx\` | macOS: `~/Library/Application Support/ezbx/` · Linux: `~/.config/ezbx/` |
| State (gateway logs, snapshots, backups, approved hosts, admin token) | `%LOCALAPPDATA%\ezbx\state\<box>\` | macOS: `~/Library/Application Support/ezbx/state/<box>/` · Linux: `~/.local/state/ezbx/<box>/` |

Use `--config FILE` or `EZBX_CONFIG` to point at a custom config file, and see [ezbx.toml.example](../ezbx.toml.example) for all settings.

### Common issues

- **"Engine not found" during `init` or `up`:** Make sure your inference engine is running with a model loaded. If it runs on an unusual port, pass `--url http://127.0.0.1:PORT` (or set `url` under `[engine.<id>]` in `ezbx.toml`).
- **Model replies stop mid-thought:** Add a `[models."<model-id>"]` section to `ezbx.toml` with `reasoning = true` and `max_tokens = 8192`, then run `ezbx down` and `ezbx up` (or `ezbx engine use <id>`).
- **"Permission denied" in `/workspace`:** If Docker created a missing workspace directory as root, fix ownership from the host with `ezbx admin shell -- chown 1000:1000 /workspace` (from Git Bash, prefix with `MSYS_NO_PATHCONV=1`).
- **"Admin shell is already running":** If a terminal closed unexpectedly while `ezbx admin shell` was open, clean it up with `docker stop ezbx-main-admin`.
