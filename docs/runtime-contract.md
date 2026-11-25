# Runtime contract for external callers (planned)

> **Status:** planning contract (target: `ezbx` v0.4.0+). None of the new flags or subcommands below exist in `ezbx` v0.3.0 yet; where an item maps to an existing v0.3.0 command, it is noted explicitly.

This document defines the stable CLI and JSON contract between `ezbx` (the sandbox runtime) and external desktop or orchestration callers such as Cadre. `ezbx` owns container isolation, gateway routing, network enforcement, and snapshots; the caller owns Git worktrees, per-agent branch governance, prompt assembly, and human approvals.

---

## 1. Design rules

1. **CLI over IPC/HTTP daemon.** Callers invoke the `ezbx` binary directly as a subprocess (`--json` or `--jsonl` on stdout, diagnostics on stderr). There is no separate host background server to discover or authenticate against.
2. **Structured machine output.** Every command in section 2 supports `--json` and returns one JSON object on stdout. Non-interactive commands never prompt on stdin; destructive actions require `--yes`.
3. **Fail closed.** If `ezbx` is missing, Docker is unavailable, the version/contract check fails, or a requested box has stale images, `ezbx` exits non-zero with a structured error and never falls back to running unboxed on the host.
4. **No secret or prompt leaks in events.** Streams and status responses report box names, network state, exit codes, and byte/latency metadata only — never model prompts, responses, tokens, or API keys.

---

## 2. Lifecycle and execution commands

Every command accepts `--box <name>` (defaults to `main`) and `--json` (except `events --jsonl`, which streams newline-delimited JSON).

| Command | Purpose | Maps from v0.3.0 |
|---|---|---|
| `ezbx create` | Validate workspace/mounts and register a named box config without starting containers | `init` / config write |
| `ezbx start` | Ensure images match `ezbx.inputs`, start gateway + box networks and containers, wait healthy | `up` |
| `ezbx exec` | Run one command or harness inside the running box non-interactively and stream stdout/stderr | `shell` (non-TTY) |
| `ezbx stop` | Stop and remove box + gateway containers and networks while keeping home volume, snapshots, and workspace | `down` |
| `ezbx destroy` | Remove everything `ezbx` created for `--box <name>` (containers, volumes, state); never deletes the workspace | `destroy --box <name>` |
| `ezbx status` | Report whether the box is stopped/starting/running, active network mode, gateway health, and image input hash | `doctor` / container inspect |
| `ezbx events --jsonl` | Stream structured lifecycle, network, and gateway events as one JSON object per line until interrupted | gateway log + container events |

### Command details

- **`ezbx create --box <name> --workspace <dir> [--network <mode>] [--allow <host>...] [--readonly <dir[:name]>...] [--json]`**
  - Registers the box under `EZBX_HOME` without touching Docker images or downloading anything.
  - Refuses overlapping state/workspace folders and enforces `internal/readonly` default secret masks.
- **`ezbx start --box <name> [--json]`**
  - Refuses unbuilt or mismatched images (`ezbx.inputs` check); never pulls or builds implicitly.
  - Starts the isolated gateway and box containers and blocks until health checks pass.
- **`ezbx exec --box <name> [--cwd /workspace] [--timeout <duration>] [--env KEY=VAL...] [--json] -- <cmd> [args...]`**
  - Runs as the unprivileged `agent` user (`uid 1000`) inside the box.
  - `--env` only forwards explicit non-secret variables; engine keys remain inside the gateway and never enter the box environment.
  - With `--json`, returns `{ "ok": true, "exitCode": 0, "durationMs": 412, "stdout": "...", "stderr": "..." }` (or streams raw stdout/stderr when `--json` is omitted).
- **`ezbx stop --box <name> [--json]`**
  - Cleanly shuts down the running box and gateway for `<name>` without touching other boxes.
- **`ezbx destroy --box <name> --yes [--json]`**
  - Scoped strictly to `--box <name>` (unlike `v0.3.0`'s global image prune). Never removes shared base images or the workspace folder.
- **`ezbx status --box <name> [--json]`**
  - Returns box state (`"missing" | "stopped" | "running" | "unhealthy"`), `networkMode`, `allow` list, `workspace`, `gatewayHealthy`, `imageTag`, and `ezbxVersion`.

### Standard JSON envelope

Every `--json` command returns one object:

```json
{
  "ok": true,
  "box": "agent-ada",
  "contractVersion": 1,
  "data": {}
}
```

On failure (non-zero exit code), `ok` is `false` and `error` carries a machine-readable `code` plus a plain-English `message`:

```json
{
  "ok": false,
  "box": "agent-ada",
  "contractVersion": 1,
  "error": {
    "code": "image_mismatch",
    "message": "box image inputs changed; run ezbx build first"
  }
}
```

Standard error codes: `invalid_args`, `box_not_found`, `box_already_running`, `docker_unavailable`, `image_mismatch`, `network_blocked`, `contract_mismatch`, `timeout`, `internal_error`.

### `ezbx events --jsonl [--box <name>] [--since <rfc3339>]`

Emits one compact JSON line per event, flushed immediately:

```json
{"ts":"2026-11-24T19:00:00Z","event":"box.started","box":"agent-ada","data":{"networkMode":"local-model-only"}}
{"ts":"2026-11-24T19:00:02Z","event":"exec.started","box":"agent-ada","data":{"execId":"ex_01","cmd":"npm test"}}
{"ts":"2026-11-24T19:00:08Z","event":"net.blocked","box":"agent-ada","data":{"host":"example.com","port":443}}
{"ts":"2026-11-24T19:00:11Z","event":"exec.exited","box":"agent-ada","data":{"execId":"ex_01","exitCode":0,"durationMs":9012}}
```

Defined event types: `box.created`, `box.started`, `box.stopped`, `box.destroyed`, `exec.started`, `exec.exited`, `net.changed`, `net.blocked`, `gateway.request`, `snapshot.created`.

---

## 3. Four network modes

`ezbx` exposes four explicit network modes on `create`, `start`, and `ezbx net`:

| Contract mode | Outbound internet | Local model gateway route | Intended use | `ezbx.toml` v0.3 equivalent |
|---|---|---|---|---|
| `off` | Blocked | Blocked | Offline tests, builds, linters, and reviewed skill scripts | *(new in v0.4)* |
| `local-model-only` | Blocked | Allowed (host model server via gateway only) | Local AI coding sessions with zero internet egress | `network = "off"` |
| `allowlist` | Approved hosts only | Allowed | Cloud model APIs or package registries on an explicit allowlist | `network = "allow"` |
| `open` | Unrestricted via proxy | Allowed | Explicit user-approved research or setup sessions | `network = "on"` |

- **Backwards compatibility:** Existing `ezbx.toml` files using `"off"`, `"allow"`, and `"on"` remain valid; the CLI and contract layer map them to `"local-model-only"`, `"allowlist"`, and `"open"` automatically, while `"airgap"` / `"none"` (or `--network off` in contract mode) enforce full gateway + internet isolation.
- **No silent escalation:** Switching from `off` or `local-model-only` to `allowlist` or `open` must be requested explicitly by the caller (e.g., after a human approval gate).

---

## 4. Version pinning and capability handshake

Callers verify compatibility before creating or starting any box by running:

```text
ezbx version --json
```

Expected response:

```json
{
  "ok": true,
  "version": "0.4.0",
  "contractVersion": 1,
  "features": [
    "lifecycle-v1",
    "events-jsonl",
    "net-four-modes",
    "readonly-masks"
  ],
  "platform": "windows/amd64"
}
```

Pinning rules for callers:

1. **Exact contract major check:** Require `contractVersion === 1`. Any breaking change to flags, JSON fields, or event schemas bumps `contractVersion`.
2. **Minimum SemVer range:** Pin a tested minimum and upper major bound (for example `>=0.4.0 <1.0.0`), recorded in the caller's manifest or release config.
3. **Fail closed on mismatch:** If `ezbx` is absent, exits non-zero, reports an unsupported `contractVersion`, or lacks a required feature flag, the caller surfaces an actionable message pointing to `ezbx doctor` and refuses sandboxed execution.
4. **No silent auto-updates:** Callers never download or overwrite the `ezbx` binary or container images automatically without explicit human consent.

---

## 5. Security invariants

All protections documented in [threat-model.md](threat-model.md) and [readonly.md](readonly.md) stay enforced regardless of caller:

- The box runs unprivileged (`agent`, `uid 1000`), with all Linux capabilities dropped, `no-new-privileges`, no Docker socket, and memory/CPU/PID limits.
- Only `/workspace` (mapped to the caller's chosen project or worktree path) is writable on the host; optional `--readonly` folders apply default secret masking (`*` `.env*`, keys, `.ssh/`, etc.).
- API keys stay on the host/gateway side and are never injected into the box environment or mounted state.
- On `off` and `local-model-only` modes, isolated Docker internal networks prevent the container from reaching LAN addresses, Docker VM bridges, or public internet endpoints.
