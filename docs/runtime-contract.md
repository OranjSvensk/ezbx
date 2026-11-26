# Runtime contract for external callers

> **Status:** v0.4.0 shipped phase 1 with lifecycle commands, status, several boxes and four network modes. v0.5.0 adds `events --jsonl` (section 2). v0.5.1 adds engine-free startup for airgap boxes.

This document defines the stable CLI and JSON contract between `ezbx` (the sandbox runtime) and external desktop or orchestration programs that drive it. `ezbx` owns container isolation, gateway routing, network enforcement, and snapshots; the caller owns Git worktrees, per-agent branch governance, prompt assembly, and human approvals.

---

## 1. Design rules

1. **CLI, not a daemon.** Callers run the `ezbx` binary as a subprocess. With `--json` a command prints one JSON object on stdout and nothing else there; progress and diagnostics go to stderr. There is no background server to discover or authenticate against.
2. **Nothing interactive.** The contract commands never ask a question. A destructive command needs `--yes`, and `--json` on one that would ask (`destroy`) is refused without it.
3. **Fail closed.** If Docker is unavailable, the box isn't running, or its images don't match `ezbx.toml`, `ezbx` exits non-zero with a structured error and never falls back to running anything on the host.
4. **No secrets or prompts in results.** Results report box names, network state, exit codes and timings. Engine keys never enter the box or any result. `exec --json` returns the output of the command the caller ran, which is the caller's own.

---

## 2. Commands

Every command takes `--box NAME`. With one box in `ezbx.toml` it may be left out; with several, it is required (the box is never guessed). `--config FILE` names another config file.

| Command | Purpose | Same as |
|---|---|---|
| `ezbx version --json` | The handshake: version, contract version, features | |
| `ezbx create` | Add a box to `ezbx.toml` without asking anything; builds, starts and downloads nothing | `ezbx init`, non-interactive |
| `ezbx build --box NAME` | Build the box's images (the only command that downloads) | |
| `ezbx start` | Start the box from images that match `ezbx.toml`; never builds or pulls | `ezbx up --no-doctor` (`--doctor` runs the doctor too) |
| `ezbx exec` | Run one command in the running box, without a terminal | `ezbx shell`, non-interactive |
| `ezbx status` | Whether the box is missing, stopped, running or unhealthy | |
| `ezbx stop` | Remove the box's containers and networks; keep its home volume, snapshots and workspace | `ezbx down` |
| `ezbx destroy --yes` | Remove the box and leave a short-lived removal receipt; keep the workspace | |
| `ezbx net MODE` | Change the network mode of a running box (section 3) | |
| `ezbx events --jsonl` | Follow runtime events with optional replay | |

### Command details

- **`ezbx create --box NAME --workspace FOLDER [--network MODE] [--allow HOST]... [--readonly FOLDER]... [--harness LIST] [--tools LIST] [--profile NAME] [--memory 8g] [--json]`**
  - Writes a `[box.NAME]` section (the file's old content is kept as `ezbx.toml.previous`) and makes the workspace folder. Defaults come from the `offline` starter profile unless `--profile` names another.
  - Refuses a box name that exists already (`box_exists`), a workspace that is, holds or sits inside another box's workspace, and the folders `ezbx init` refuses (drive roots, the home folder, system folders, `ezbx`'s own folders). Read-only folders get the default secret masks.
  - `[engine]` (the model server) is shared by every box. `--url`, `--api` and `--key-env` are taken only when `ezbx.toml` doesn't exist yet; otherwise change the server with `ezbx init` or `ezbx engine use`.
  - `data`: `configFile`, `workspace`, `network`, `allow`, `harnesses`, `tools`, `readonly`.
- **`ezbx start --box NAME [--doctor] [--json]`**
  - Refuses images that aren't built from the current inputs (`image_mismatch`) and a box that is already up (`box_already_running`). Waits until the gateway's health check passes. `data` is the box's state, as `status` reports it.
  - A box configured with `network = "airgap"` can start with no `[engine].url`. Both `start` and `up` skip model-server discovery and harness model configuration for this startup, including when they run the doctor. Boxes configured for `off`, `allow` or `on` still require a reachable model server at startup.
- **`ezbx exec --box NAME [--cwd DIR] [--timeout 10m] [--env NAME=VALUE]... [--snapshot] [--json] -- COMMAND [ARGS...]`**
  - Runs as the unprivileged `agent` user (uid 1000) in `/workspace` (or `--cwd`, a clean full path inside the box). Standard input is passed through.
  - `--timeout` ends the command inside the box (coreutils `timeout`, killed 5 seconds later if it hasn't ended), so nothing keeps running there after `ezbx` gives up.
  - `--env` passes plain variables only: proxy variables, `HOME`, `PATH`, `USER`, and names containing `KEY`, `TOKEN`, `SECRET` or `PASSWORD` are refused (`invalid_args`). A model server's key belongs under `[engine] key_env`, where only the gateway reads it.
  - No workspace snapshot unless `--snapshot` (the caller owns its worktrees). The read-only folder check of `ezbx shell` runs every time: a new secret file in a read-only folder stops the command.
  - Without `--json`, the command's output streams through and `ezbx` exits with its exit code. With `--json`, `ezbx` exits 0 once the command has run, and `data` is `{"exitCode": 0, "durationMs": 412, "stdout": "...", "stderr": "..."}`, with `stdoutTruncated`/`stderrTruncated` when either passed 1 MiB (the first 1 MiB is kept) and bytes that aren't UTF-8 replaced. A command stopped by `--timeout` gives `ok: false`, error `timeout`, and the partial `data` with `timedOut: true`.
- **`ezbx status --box NAME [--json]`**
  - `data`: `state` (`missing`: not in `ezbx.toml` and nothing exists; `stopped`; `running`; `unhealthy`, with `problems`), `workspace`, `networkMode` (the running gateway's, else `ezbx.toml`'s), `configNetworkMode`, `allow`, `gatewayHealthy`, `imageTag`, `imageCurrent` (whether the running box uses the image `ezbx.toml` needs now), `ezbxVersion`. It only reads.
- **`ezbx stop --box NAME [--json]`**: `data` is the box's state afterwards.
- **`ezbx destroy --box NAME --yes [--remove-config] [--images] [--json]`**
  - Removes the box's containers, networks, home volume and state folder (snapshots and backups too), and the box's images that no other box in `ezbx.toml` needs and no other container uses. `--images` also removes every other `ezbx` image no container uses, and the build record. Never the workspace.
  - `--remove-config` also takes the box's section out of `ezbx.toml` (the whole file only when it was the only box).
  - A successful removal clears the box's event history and leaves a short-lived `box.destroyed` receipt in the host event journal.
  - With `--json` the list of what is removed goes to stderr, and `data` is `{"removed": [...], "kept": [...]}`.

### The JSON result

Every `--json` command except `version` prints one object:

```json
{
  "ok": true,
  "box": "agent-ada",
  "contractVersion": 1,
  "data": {}
}
```

On failure `ok` is `false`, the exit code is non-zero (2 for `invalid_args`, else 1), and `error` carries a machine-readable `code` plus a plain-English `message`:

```json
{
  "ok": false,
  "box": "agent-ada",
  "contractVersion": 1,
  "error": {
    "code": "image_mismatch",
    "message": "the image ezbx-box:3f2a91c0d4be isn't built yet. Run ezbx build (it needs the internet once), then ezbx up"
  }
}
```

| Code | Meaning |
|---|---|
| `invalid_args` | A flag or value was wrong, or a setting was refused (exit code 2) |
| `box_not_found` | `ezbx.toml` has no box by that name |
| `box_exists` | `create` was asked for a box `ezbx.toml` already has |
| `box_not_running` | The box's containers aren't running |
| `box_already_running` | `start` found the box up already; `stop` it first to apply changes |
| `docker_unavailable` | Docker doesn't answer, or is older than Engine 28 |
| `image_mismatch` | An image isn't built, or wasn't built from the current inputs; run `ezbx build` |
| `timeout` | `exec --timeout` stopped the command, or a step took too long |
| `internal_error` | Anything else; the message says what |

A caller may also define `contract_mismatch` for itself (section 4); `ezbx` never prints it.

### `ezbx events --jsonl [--box NAME] [--config FILE] [--since RFC3339]`

One compact JSON line per event on stdout until interrupted. Diagnostics go to stderr. Omit `--since` for live events only. Pass a timestamp to replay retained records at or after it, then follow new records from the same journal without a replay-to-live gap.

```json
{"id":"0a5fcf7996274c668c51b135025ca549","ts":"2026-11-24T19:00:00Z","event":"box.started","box":"agent-ada","data":{"networkMode":"off"}}
{"id":"1b5fcf7996274c668c51b135025ca549","ts":"2026-11-24T19:00:08Z","event":"net.changed","box":"agent-ada","data":{"previousMode":"off","networkMode":"airgap"}}
```

| Event | When ezbx records it | Data |
|---|---|---|
| `box.created` | `create` or `init` adds a box | `networkMode` |
| `box.started` | `start` or `up` has brought the box up, before any doctor checks | `networkMode` |
| `box.stopped` | `stop` or `down` completes for a configured box | Empty |
| `box.destroyed` | `destroy` completes a removal | Empty |
| `exec.started` | After preflight, just before ezbx invokes Docker exec | `execId`, `program` (basename or `(unnamed)`), `argCount` |
| `exec.exited` | The exec attempt returns | `execId`, `durationMs`, `timedOut`, and either `exitCode` or `errorCode` |
| `net.changed` | `net MODE` changes the running gateway mode | `previousMode`, `networkMode` |
| `snapshot.created` | A manual, automatic, or rewind safety snapshot is saved | `snapshotId`, `kind` |

- Records appear in journal order for one box. Timestamps are UTC and can repeat. To reconnect, pass the last timestamp seen and deduplicate by `id`. Both exec records share an `execId`. Arguments, command output, request content, and snapshot notes stay out of the journal.
- The host journal keeps up to 1,024 records per box, including while it is stopped. Reading it works independently of Docker and gateway logging. After destroy, use the explicit box name to look up its receipt, even if the config was removed. The receipt is eligible for replay for up to 24 hours and is removed when the journal is next accessed after expiry. A new box with the same name produces new records and IDs.
- If the requested history has been pruned or destroyed, or a live reader falls behind retention, the command reports the gap on stderr and exits 1. An available removal receipt is sent first when the gap crosses a destroy. Refresh state with `status` before resuming.
- The stream records ezbx CLI actions. Use `status` to check current state, including Docker restarts and manual container changes. A process interruption can leave an exec start without an exit. Journal write failures go to stderr and the command keeps the result of the operation it already performed.

From the repository root on Windows with Go, Python, and Docker Desktop, run `python scripts/test-runtime-events.py` for the fake-engine acceptance checks. The test builds the pinned Pi and gateway images, which may fetch build inputs, and uses temporary workspaces and a loopback fake engine.

---

## 3. Four network modes

| Mode | Outbound internet | Model route (host model server via the gateway) | Intended use |
|---|---|---|---|
| `airgap` | Blocked | Blocked | Offline tests, builds, linters and reviewed scripts |
| `off` | Blocked | Allowed | Local AI coding with no internet at all (the default) |
| `allow` | Approved hosts only | Allowed | Package registries or other hosts on an explicit allowlist |
| `on` | The public internet through the proxy | Allowed | Research or setup sessions a human approved |

- The same four words are used in `ezbx.toml`, `ezbx create --network`, `ezbx init --network` and `ezbx net`. A config written before 0.4 keeps its meaning: `off` has always been "the model only".
- In `airgap` the gateway refuses every model request with 403 (`network_airgap`) and its proxy refuses everything. Switching a running box to `airgap` ends model requests in flight, streams included, and cancels them at the engine. The box still reaches the gateway's two ports, which refuse it.
- A box started in `airgap` has no model server attached. If you change its network mode while it runs, model requests return 503 (`engine_unconfigured`) until you attach an unkeyed server with `ezbx engine use` or restart using an engine and a model-enabled mode in `ezbx.toml`. A box started in a model-enabled mode keeps its engine when switched into and out of `airgap`.
- **No silent escalation:** `ezbx net` changes the mode only when asked, and the next `start` goes back to the mode in `ezbx.toml`.

---

## 4. Version pinning and capability handshake

Callers check compatibility before creating or starting any box:

```text
ezbx version --json
```

```json
{
  "ok": true,
  "version": "0.6.0",
  "contractVersion": 1,
  "features": ["events-jsonl", "lifecycle-v1", "net-four-modes", "readonly-masks", "status-v1"],
  "platform": "windows/amd64"
}
```

Callers that need the event stream check for `events-jsonl` in `features`.

Pinning rules for callers:

1. **Exact contract major check:** require `contractVersion === 1`. Any breaking change to flags, JSON fields or error codes raises it.
2. **Minimum SemVer range:** pin a tested minimum and upper major bound (for example `>=0.4.0 <1.0.0`).
3. **Fail closed on mismatch:** if `ezbx` is absent, exits non-zero, reports an unsupported `contractVersion`, or lacks a required feature, the caller surfaces an actionable message pointing to `ezbx doctor` and refuses sandboxed execution.
4. **No silent auto-updates:** callers never download or overwrite the `ezbx` binary or its images without explicit human consent.

---

## 5. Security invariants

All protections in [threat-model.md](threat-model.md) and [readonly.md](readonly.md) stay enforced whoever calls:

- The box runs unprivileged (`agent`, uid 1000), with all Linux capabilities dropped, `no-new-privileges`, no Docker socket, and memory, CPU and process limits.
- Only `/workspace` (the caller's chosen project or worktree folder) is writable from the box; optional read-only folders get the default secret masks (`.env` files, keys, `.ssh/` and more).
- Engine keys stay with the gateway and never enter the box's environment or files.
- In `airgap` and `off`, the internal Docker network keeps the box away from the LAN, Docker's own addresses and the internet.
- Several boxes never share a workspace, so one box's `rewind` can't undo another agent's work.
