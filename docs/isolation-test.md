# Isolation test

The central safety claim of ezbx is that the box can reach the gateway's model and proxy ports and nothing else. This page records how that is tested and what the results were.

## Setup

Everything is built from this repository and labelled `ezbx.managed=true`.

| Part | What it is |
|---|---|
| `ezbx-gateway:dev` | [docker/gateway.Dockerfile](../docker/gateway.Dockerfile): the `ezbx` binary alone in a `scratch` image (about 11 MB), run as user 65532 with a read-only root filesystem, every capability dropped and `no-new-privileges`. |
| `ezbx-probe:dev` | [docker/probe.Dockerfile](../docker/probe.Dockerfile): [tools/isoprobe](../tools/isoprobe/main.go), which tries each target and prints PASS or FAIL. It runs with the same restrictions the box will have. |
| fake engine | [tools/fakeengine](../tools/fakeengine/main.go), listening on the host's loopback address only, like a real engine. No real model is involved. |
| inside network | `docker network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated ...` |
| outside network | an ordinary bridge network, not Docker's default one. |

The gateway is attached to both networks and forwards to `http://host.docker.internal:18080`. The probe is attached to the inside network only.

## What the probe tries from the inside network

- The model route through the gateway: **must work**.
- Every TCP port on the gateway: only the model port may be open.
- Public addresses by IP (HTTPS) and public names (DNS through Docker's resolver).
- Public DNS servers, the LAN router and Docker Desktop's DNS, asked directly.
- Docker Desktop's own addresses (`host.docker.internal`, `gateway.docker.internal`, `kubernetes.docker.internal`, and the addresses they resolve to on the outside network), including the port the engine listens on.
- The host's LAN address and its other network adapters.
- Other containers, on the outside network and on Docker's default bridge, by name and by address.
- The gateway's own outside-network address, on every port. Linux accepts a packet for any of its own addresses on any interface, so this checks that the box has no route there.
- The bridge addresses of the outside network, the default bridge and the inside network.
- The cloud metadata address `169.254.169.254`.

## Results (2026-11-20, Docker Desktop on Windows 11, Engine 29.9.3)

**With a plain `--internal` network, one gap was found.** The inside network's bridge still has an address, and that address belongs to Docker Desktop's Linux VM. From the box, that address answered on TCP port 111 (the RPC portmapper) and on one high port. Anything listening on all addresses inside the VM was reachable from the box. Everything else was blocked.

**With `gateway_mode_ipv4=isolated` added, every check passed.** The bridge has no address at all, so there is nothing of the VM's to connect to. The model route worked, the gateway showed only its model port, and every other target failed with "network is unreachable". Names outside the network didn't resolve.

Decided from this: the inside network is always created with `--internal` **and** `gateway_mode_ipv4=isolated`, with IPv6 off. That option needs Docker Engine 28 or later, and `ezbx doctor` must check for it.

Also confirmed on the outside network: a container there reaches an engine bound to the host's loopback address through `host.docker.internal`.

### The box image (same day)

The first box image ([docker/box.Dockerfile](../docker/box.Dockerfile): Node 24 LTS, git and Pi 1.5.0) was run on the isolated inside network as user `agent`, with every capability dropped, `no-new-privileges`, and memory and process limits.

- The probe, run inside the box image itself, passed every check above, plus `registry.npmjs.org` and `github.com` not resolving.
- Inside the box: no capabilities at all, no file with a setuid or setgid bit, no `sudo`, and the Pi install belongs to root, so the agent can run it but not change it.
- `pi -p` with a config written by hand from the Pi manifest's template sent its request through the gateway to the fake engine on the host, and printed the reply. The gateway log shows the request. Pi needed nothing else from the network. It wrote only two empty files to its config folder and nothing to the workspace.

Not covered: Pi with a real model, tool calls, and the interactive interface.

### The egress proxy and the admin API (same day)

The gateway ran with the proxy on port 3128 (starting in `off`) and the admin API on its own loopback, with the token file mounted read-only. Docker reported the gateway healthy through `ezbx gateway health`. Every change below was made from the host with `docker exec ... ezbx gateway admin`. Tests ran from the box image with the usual proxy variables set.

- **Admin API out of reach.** A full port scan of the gateway from the box found only 8000 and 3128. Port 8099 was closed as `gateway:8099`, and through the proxy as `127.0.0.1:8099`, `localhost:8099`, `[::ffff:127.0.0.1]:8099` and `0.0.0.0:8099`.
- **`off`:** every proxy request got 403, including `example.com`, `github.com` and `1.1.1.1`.
- **`allow` with `registry.npmjs.org`:** `npm view left-pad version` worked through the proxy. `git ls-remote https://github.com/...` was refused with 403 (`not_on_allowlist`), then worked after `allow github.com`.
- **`on`:** tunnels to `example.com:443` and `1.1.1.1:443` opened. Refused with 403: `host.docker.internal`, `gateway.docker.internal`, Docker Desktop's host and DNS addresses, the bridge addresses, `169.254.169.254`, `10.0.0.1`, `[::1]`, and ports 22 and 8080. Also refused: public names that resolve to local addresses (`localtest.me` to 127.0.0.1, and `169.254.169.254.nip.io` and `10.0.0.1.nip.io`). The log gave `local_address` as the reason, so the check after the lookup caught them.
- **Tightening:** with a tunnel to `example.com` open, switching from `on` to `allow` (npm registry only) closed it at once. The admin reply said one tunnel was closed, and its log line says `network_tightened`.
- The admin token never appeared in the gateway log.

### With a real engine (same day)

Strata ran on the host, bound to its loopback address, with one model loaded (a reasoning model with a 262,144-token context and text and image input). The gateway forwarded to it through `host.docker.internal`.

- `ezbx engine detect` on the host found it on its custom port through Strata's own status command, with the model's context size, input types and loaded state.
- **Found and fixed:** Strata refused every request from the gateway with 403, because of its DNS rebinding protection: the Host header was `host.docker.internal:PORT`, which isn't a local name. Readiness hid this by saying "no models". The gateway now sends `Host: localhost:PORT` when the upstream is Docker Desktop's host alias, and readiness reports the engine's status code (see [gateway.md](gateway.md), section 4).
- After the fix, from the box: the model list, a chat reply, and a streamed reply that arrived in 10 chunks (first after 7 ms). A stream cancelled by the box was logged as 499 `client_gone`, and the gateway kept working. `/v1/../props`, `/v1/%2e%2e/props`, `/props`, `/readyz` and `DELETE` were refused.
- **Pi with the real model**, on the isolated network: it answered a plain prompt, used its read tool to find a word in a workspace file, and used its write tool to create a file with exact content, which appeared in the workspace folder on the host. Each took 3 to 7 seconds. Every request went through the gateway.
- Then again with a config produced by `ezbx harness render pi --reasoning`, with no hand edits: Pi read a word from a workspace file and wrote it into a new file, in 6 seconds.
- The model is a reasoning model: with a 16-token limit, the reply stopped inside its reasoning with no answer. Pi's config needed `reasoning: true` and room for the answer (8,192 tokens). Engines don't report this, so the config generator must take it from a default or the user.

Not covered: Pi's interactive interface.

### `ezbx doctor` (same day)

On the setup above (isolated inside network, locked-down gateway in front of Strata, a running box container), `ezbx doctor` passed all 21 checks in about 4 seconds.

Then a deliberately broken box: the inside network made with `--internal` alone; a gateway running as root, writable, with its capabilities and a port published to every address; and a box running as root, with its capabilities, also attached to Docker's default bridge, with a file mounted at `/var/run/docker.sock` (an empty file, not the real socket). The doctor failed exactly those seven things, each with a fix. From inside, its port scan of the network's bridge address found the Docker VM's ports open, as the first test above did by hand. The broken box was removed straight after, because its published port made the engine reachable from the network.

The unit tests feed it recorded `docker inspect` output and check that 26 separate faults are each caught by the right check.

### `ezbx up` and `ezbx down` (same day)

The setup above, made by `ezbx up` from an `ezbx.toml` instead of by hand, with `EZBX_HOME` pointing at a test folder. Network mode `allow` with `registry.npmjs.org`.

- **With the fake engine:** `ezbx up` created both networks, the home volume and the admin token, started the gateway, waited for its healthcheck, then started the box, in about 5 seconds. Pi's config was copied into the home volume as a tar stream and arrived owned by the agent (folders 700, files 600). `pi -p` answered through the gateway. `npm view` worked and `git ls-remote https://github.com/...` was refused with 403 (`not_on_allowlist`). The gateway log was written to `logs/gateway.log` in the state folder. Neither container published a port, and the token was mounted read-only into the gateway only.
- Running `ezbx up` while the box was up refused and changed nothing. `ezbx down` removed the two containers and two networks and kept the volume, the state folder and the workspace. `ezbx up` again reused the volume (a file left in the agent's home survived) and applied a changed `[models."<id>"]` override (`reasoning`, `max_tokens`).
- `ezbx doctor` on that box: 20 checks passed, including every check from inside the box, with 1 warning: the doctor looked for engines only on their usual ports, so it didn't see the fake engine on its test port, although the gateway reached it. **Fixed:** the doctor now also checks the address the gateway was started to forward to (Docker Desktop's host alias read as this computer's loopback). The same box then passed all 21 checks.
- **With Strata:** `engine.use = "auto"` with both engines running refused, naming both. With `engine.use = "strata"`, detection found it through Strata's status command, and `ezbx up` with the doctor passed **21 of 21**. Pi, with `reasoning` and `max_tokens` set for the model in `ezbx.toml`, answered a plain prompt in about 3 seconds and wrote a file into the workspace with its write tool in about 6.

Not covered: Linux (a gateway that isn't root may not be able to write its log into a folder owned by the host user), a restart of Docker Desktop with the box up (both containers have the restart policy `unless-stopped`), and engines that need a key (since covered with the fake engine; see "The engine key file" below).

### `ezbx shell`, `ezbx net` and `ezbx engine use` (same day)

A box made by `ezbx up` in front of Strata, network `off` in `ezbx.toml`. The doctor passed 21 of 21.

- `ezbx net` listed `registry.npmjs.org` as blocked after `npm view` failed in the box. (It also lists `example.com`, which the doctor's own checks ask for.)
- `ezbx net allow registry.npmjs.org` saved the host and added it live; with the network still `off`, npm stayed blocked. After `ezbx net allow`, npm worked and `git ls-remote https://github.com/...` was refused. After `ezbx net on`, GitHub answered. After `ezbx net off`, npm was blocked again.
- **Restart:** after `docker restart` of the gateway, the mode was `off` again (from `ezbx.toml`) and `registry.npmjs.org` was still allowed (read from the saved file). `ezbx net remove` took it out of the file and the running gateway.
- `ezbx engine use` found Strata again and refreshed Pi's config in the box. `ezbx engine use strata --url` with an address where nothing listens was refused and left the gateway as it was.
- `ezbx shell` ran as the `agent` user in `/workspace`. `ezbx shell pi -- -p ...` answered through Strata in about 1.5 seconds and wrote a file into the workspace with its write tool.
- **Found and fixed:** the first `ezbx shell pi` failed with "cannot attach stdin to a TTY-enabled container", because Windows reports the `NUL` device as a character device, which looked like a terminal. The check now asks the Windows console itself (and rules out `/dev/null` elsewhere).

Not covered here: Pi's full-screen interactive mode, which needs a person at a real terminal (see the next sections).

### `ezbx init` (same day)

From an empty `EZBX_HOME`, with Strata running, answers typed as a person would.

- `init --plan` showed every question with its default and the plan, and created nothing.
- `init` found Strata, asked whether its model is a reasoning model, offered Pi, took a workspace, network `off` and the default memory (8g, half of Docker's 31.3 GB, capped), showed the plan, and after "y" wrote `ezbx.toml` and ran `ezbx up`.
- **Found and fixed:** the doctor at the end failed its one-token completion, because the model had just been loaded and shared the GPU with a game, and the check waited only 30 seconds. Run again a minute later, it passed 21 of 21. The check now waits up to 2 minutes and reports a timeout as a warning, since the model list had already come back through the gateway.
- `init --yes` again: "Nothing to change". `init --yes --network allow` showed `network off -> allow`, kept the old file as `ezbx.toml.previous`, and didn't restart the running box. Pi answered through the box `init` made in about 3 seconds.
- Answering "n" at the plan changed nothing, and input that ended early stopped with a clear message; neither created a file.

### First run by hand (same day)

I built `ezbx` with `go install ./cmd/ezbx` and ran `ezbx init` and `ezbx shell pi` myself in PowerShell, with my real config folder and Strata. Setup, the box and Pi's full-screen interface all worked.

- **Found:** Pi printed "fd not found. Downloading..." and the same for ripgrep, then a 403 from the proxy. Pi looks for both tools and, if they are missing, downloads the latest release from GitHub. The box's network was `off`, so the gateway refused, as it should. Fixed: the image now installs Debian's `fd-find` and `ripgrep` (Pi also accepts the name `fdfind`), and `PI_OFFLINE=1` turns off Pi's startup network calls. In a container with no network, Pi's own tool finder then returned `fdfind` and `rg` without trying a download.

### `ezbx build`, `ezbx.lock` and tool packs (next day)

From a scratch `EZBX_HOME`, with the fake engine, a box named `vt` with all three tool packs.

- `ezbx build` built both images from the files inside the program in about 3 minutes: `ezbx-gateway:<hash>` (13 MB) and `ezbx-box:<hash>` (2 GB with every pack; Chromium and FFmpeg are most of it). apt used the base image's own snapshot (`20261104T212339Z`), and `images.json` recorded each package's version, Pi 1.5.0 and both image IDs.
- After a change to the carried files that doesn't touch the box's inputs (tool notes), `ezbx build` rebuilt only the gateway (12 seconds, from Docker's cache) and said the box was built already.
- With only `pack:coding`, the box image is 726 MB (the hand-built image before tools was 671 MB).
- In a container like the box (the agent user, every capability dropped, no-new-privileges, no network), every tool did real work: fd and rg found files, jq, python3, make and unzip ran, FFmpeg made a video and ffprobe read it, ImageMagick made and measured a PNG, and Chromium rendered a page and took a screenshot. Chromium needs `--no-sandbox`: its own sandbox needs kernel features the box drops on purpose.
- `ezbx up` with the new images: about 10 seconds, the doctor passed 21 of 21. In the box, Pi answered through the gateway, `PI_OFFLINE` was set, Pi downloaded no tools of its own, and curl was refused by the proxy (`CONNECT tunnel failed, response 403`) with the network `off`.
- `ezbx build --prune`, answered "n": it listed the older images, kept the ones containers use and said so, and removed nothing.
- **Found and fixed:** the first prune offered images that containers were still using (Docker would have refused, but ezbx shouldn't offer them). Prune now leaves them out and names them.

Not covered: `ezbx init` building images live (covered by unit tests; a live run would have probed the engines on this computer, and the real engine was in use).

### Snapshots, `review` and `rewind` (same day)

From a scratch `EZBX_HOME`, with the fake engine, a box named `vt` and a workspace holding a project that is a git repository itself, with a `node_modules` folder.

- `ezbx shell` took a snapshot before the session: 29 files (the project's `.git` folder included), `node_modules` left out. In the session, the agent changed a file, deleted one, made a new folder and committed in the project's repository.
- `ezbx review` listed the three changes and summed up git's own files in one line; `--diff` showed the changed lines.
- `ezbx rewind` refused while another process ran in the box. With the box idle, it listed the changes, asked, took a safety snapshot and put the workspace back: the file and the deleted file returned, the new folder was gone, the agent's commit was undone (`git log` and `git fsck` clean), and `node_modules` was left as it was.
- `ezbx rewind` to the safety snapshot undid the rewind, commit included.
- A snapshot by hand with a note, `snapshot list` and `snapshot remove` worked. Four snapshots of about 28 KB took 14 KB of store. The box's only mounts were the workspace and its home volume.
- With the store locked, `ezbx shell` refused to start the session and named `--no-snapshot`.
- **Found and fixed:** `review` printed git's internal files one by one, burying the real changes; they are now summed up per project. Mutation checks found four weak tests (size-only change, the safety snapshot not checked on disk, a folder the target left out, the big-diff fallback), all fixed. The link protection is tested on Linux only, because Windows needs extra rights to make links.

### `ezbx admin shell`, `reset` and `destroy` (same day)

From a scratch `EZBX_HOME`, with the fake engine and a box named `vt`, while my own box kept running.

- **Admin shell:** root, home `/root`, in `/workspace`; it read the agent's files and changed their owner; Docker's normal capabilities (`CapEff 00000000a80425fb`), no Docker socket; no network by default (name lookups fail), and `--net` reached a public site (HTTP 200). Files left in `/root` and `/opt` were gone in the next admin shell, and no admin container was left.
- **Reset:** answering "n" changed nothing. With `--yes` it backed up `ezbx.toml`, `approved-hosts` and `home.tar`, gave the agent a fresh home (a file the agent had left was gone, Pi's settings were back), kept the approved host, and the doctor passed 21 of 21. `--restore` put the file back, owned by the agent with its modes. It refused while a process ran in the box, and when the backup couldn't be written it changed nothing and the box stayed up.
- **Destroy:** with `--yes` it refused while a process ran in the box. It listed everything with sizes and the snapshot and backup counts, kept the images my own box was using and said so, and removed nothing when the wrong name was typed. Typing the box name removed the containers, networks, home volume, state folder, its two gateway images and `images.json`, and left the workspace and the config. `--remove-config` then removed the config.
- **Found and fixed:** `docker container ls` has no `ImageID` field, so the first version of destroy's in-use check would have failed (found by trying the command before the live run). Destroy would have removed images another box uses; it now keeps them. A backup cut off between two files passed the first check; the check now reads every file in full and needs tar's end marker. Destroy said it removed a state folder that wasn't there. `reset` gained `--no-doctor`, because the doctor checks every engine on the computer.

### The engine key file (same day)

From a scratch `EZBX_HOME`, with a box named `vt` and the fake engine started with `--key-env`, so it answered chat requests without the right key with 401.

- `ezbx up --no-doctor` wrote the key to `engine-key` in the state folder and mounted it read-only into the gateway at `/run/ezbx/engine-key`. The gateway ran with `--key-file`, and its environment held only `PATH`.
- The key was found 0 times in `docker inspect` of the gateway and the box, and 0 times in the gateway log. The box had no `/run/ezbx` and no key in its environment.
- A chat request from the box through the gateway got 200 from the engine: the gateway added the key. After `docker restart` of the gateway (as after a Docker Desktop restart), it was healthy and the request again got 200.
- `ezbx down` removed the key file with the containers and kept the token, log and approved hosts.
- **Found, then fixed (design decision 19):** with the model list behind the key too (`--key-on-models`), `ezbx up` said the engine was not found and changed nothing, because detection sent no key. Every protection was mutation-checked (20 of 20 caught).

### Detection with the engine's key (same day)

The fake engine ran with `--key-env --key-on-models`, so its model list answered 401 without the key, and a real Strata (no key) ran on its usual port.

- `ezbx engine detect` found Strata as before. With `--url` pointing at the fake engine, it also printed that the address asks for a key, instead of leaving it out silently.
- `ezbx up` stopped before contacting anything or creating anything when `key_env` had no `url` (refused by the config) and when the variable was empty. With a wrong key it said the engine refused the key in the named variable, and the wrong key wasn't in the output. With no `key_env` it said the engine asks for a key and how to give it.
- With the right key, `up` found the engine and started the box. A chat request from the box got 200. The key was found 0 times in `docker inspect` and in the state folder apart from the key file, which `down` removed.
- `ezbx doctor` passed all 22 checks. It showed Strata as the running engine and the gateway as forwarding to "an engine that asks for a key"; before a fix found here, that row would have warned that no engine was there.
- Every new protection was mutation-checked (18 of 18 caught), including one that sends every key to every address.

### A workspace the agent can't write, and a leftover admin shell (same day)

- With `/workspace` made owned by root with mode 755 (as Docker leaves a folder it creates itself), the agent got "Permission denied". `ezbx doctor` failed "agent can write the workspace", naming the owner and mode, and `ezbx up` warned the same way after starting the box. Both gave the fix `ezbx admin shell --box vt -- chown 1000:1000 /workspace`, which worked, and the doctor then passed. (Run from Git Bash, the fix needs `MSYS_NO_PATHCONV=1`, because Git Bash turns `/workspace` into a Windows path; PowerShell doesn't.)
- With an admin shell left running, a second `ezbx admin shell` and `ezbx reset` both said one was open and gave `docker stop ezbx-vt-admin`, which removed it.
- Every new check was mutation-checked (9 of 9 caught).

Not covered: a real engine started with a key, and Linux, where the gateway's user may not be able to read a key file only you can read (the admin token has the same open question).

### OpenCode, the second harness (2026-11-22)

A test box with `harnesses = ['opencode', 'pi']`, no tools, the fake engine and the network `off`, built from nothing (no ezbx or base images on the computer beforehand).

- `ezbx build` installed both harnesses from their lockfiles with install scripts off, in the one shared harness stage. The image read back `opencode v2.3.2` and Pi `1.5.0`, and is 1.05 GB with no tools. Its `ezbx.harnesses` label says `opencode,pi`.
- `up` set each harness's `env` on the box container (`PI_OFFLINE`, `PI_SKIP_VERSION_CHECK`, `OPENCODE_DISABLE_MODELS_FETCH`), and `NO_PROXY` listed the gateway and loopback.
- OpenCode's generated config was read without changes: `opencode run "say hi"` answered through the gateway, and so did `ezbx shell opencode -- run "say hi"`, after its snapshot. Pi still answered with `pi -p`.
- **Found:** at every run, OpenCode fetched its model catalogue from `models.opencode.ai`. The gateway refused it (2 blocked requests per run, network `off`). `OPENCODE_DISABLE_MODELS_FETCH=1` stops it: 0 blocked requests in each of 4 runs with it, against 2 per run in 3 runs without it. The manifest now sets it. No other request left the box except to the gateway, and OpenCode's log showed no errors.
- The race detector and the Linux-only tests passed in the Go container. Every new protection was mutation-checked (17 of 17 caught).

Not covered: OpenCode's file tools (the fake engine can't make tool calls), its full-screen interface, a box with OpenCode alone, and a real model.

### Five more harnesses: oh-my-pi, Codex CLI, Claude Code, Hermes Agent and OpenClaw (same day)

A test box `vt` with all seven harnesses (`harnesses = ['claude', 'codex', 'hermes', 'omp', 'openclaw', 'opencode', 'pi']`), no extra tool packs, the fake engine and the network `off`.

- `ezbx build` installed all seven harnesses in the shared harness stage with install scripts off (and `pip --no-deps --require-hashes --only-binary :all:` for Hermes Agent's 61 pinned wheels). The image read back `2.1.330 (Claude Code)`, `codex-cli 0.171.0`, `Hermes Agent v0.19.0 (2026.7.20)`, `omp/19.1.0`, `OpenClaw 2026.11.6 (06afb10)`, `opencode v2.3.2` and Pi `1.5.0`, and is 4.28 GB with `fd`, `ripgrep` and `python3` (pulled in by the harnesses' `tools` lists).
- `ezbx up --no-doctor` rendered all seven harnesses' configs into the agent's home volume — JSON for Pi, OpenCode, Claude Code and OpenClaw, TOML for Codex CLI (`~/.codex/config.toml`), and YAML for oh-my-pi (`~/.omp/agent/models.yml` and `config.yml`) and Hermes Agent (`~/.hermes/config.yaml`) — and set each harness's offline environment variables on the box container.
- All seven harnesses answered `"say hi"` through the gateway with `"Hello from the fake engine."`:
  - `pi -p "say hi"` and `opencode run "say hi"` answered over `/v1/chat/completions` with 0 blocked proxy requests.
  - `omp -p "say hi"` (running under its pinned `@oven/bun-linux-x64-baseline` binary) answered over `/v1/chat/completions`; its non-interactive startup starts a background catalog refresh (`catalog.stencil.so` and 6 provider discovery hosts), which the gateway blocked with 403 (`network_off`) without affecting the reply.
  - `codex exec --skip-git-repo-check "say hi"` sent `POST /v1/responses` to the gateway, which translated it to the engine's `/v1/chat/completions` and streamed back OpenAI Responses SSE events. With `check_for_update_on_startup = false`, `analytics.enabled = false` and `features = { apps = false, plugins = false, remote_plugin = false }` in `~/.codex/config.toml`, it made 0 blocked proxy requests (down from 4 startup calls to `github.com`, `api.github.com`, `chatgpt.com` and `ab.chatgpt.com`).
  - `claude -p "say hi"` sent `POST /v1/messages` to the gateway, which stripped `X-Api-Key`, translated the request to `/v1/chat/completions`, and returned Anthropic Messages SSE events, with 0 blocked proxy requests.
  - `hermes chat -q "say hi"` answered over `/v1/chat/completions` with 0 blocked proxy requests once `~/.hermes/config.yaml` set `security.tirith_enabled: false` (skipping a GitHub release check for `tirith`) and `model.context_length: 131072` (satisfying Hermes Agent's 64,000-token minimum and short-circuiting its OpenRouter metadata lookup).
  - `openclaw agent exec "say hi"` answered over `/v1/chat/completions` with 0 blocked proxy requests once `agents.defaults.models` in `~/.openclaw/openclaw.json` was rendered as a keyed record (`"ezbx/<id>": {}`).
- **With Strata and a real model (same day):** the same box `vt` (`network = "off"`) was started against Strata with a loaded reasoning model (`262,144`-token context, `reasoning = true`, `max_tokens = 8192`). `ezbx up` ran `ezbx doctor`, passing **22 of 22** checks. Each of the six newer harnesses was asked to read `note.txt` in `/workspace` (`The secret word is lantern.`) and write the secret word into its own `answer-<harness>.txt` file:
  - `opencode run ...`: read `note.txt` and wrote `/workspace/answer-opencode.txt` (`"lantern"`) over 4 `/v1/chat/completions` requests in 56 seconds, with **0 blocked proxy requests**.
  - `omp -p ...`: read `note.txt` and wrote `/workspace/answer-omp.txt` (`"lantern"`) over 3 `/v1/chat/completions` requests in 19 seconds (its 7 background catalog refresh requests were blocked with 403 `network_off`).
  - `codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox ...`: read `note.txt` and wrote `/workspace/answer-codex.txt` (`"lantern"`) over 3 `/v1/responses` requests (including multi-turn `function_call` and `function_call_output` translation) in 18 seconds, with **0 blocked proxy requests**.
  - `claude -p --dangerously-skip-permissions ...`: read `note.txt` and wrote `/workspace/answer-claude.txt` (`"lantern"`) over 3 `/v1/messages` requests (including multi-turn `tool_use` and `tool_result` translation) in 23 seconds, with **0 blocked proxy requests**.
  - `hermes chat --yolo -q ...`: called its `read_file` and `write_file` tools to write `/workspace/answer-hermes.txt` (`"lantern"`) over 3 `/v1/chat/completions` requests in 27 seconds, with **0 blocked proxy requests**.
  - `openclaw agent exec ...`: read `note.txt` and wrote `/workspace/answer-openclaw.txt` (`"lantern"`) over 4 `/v1/chat/completions` requests in 42 seconds, with **0 blocked proxy requests**.
- Every new protection was mutation-checked (17 of 17 caught).

Not covered: full-screen interactive sessions for the six newer harnesses (which need a person at a terminal), and boxes containing each harness alone.

### LM Studio, llama.cpp and Ollama (same day)

Each engine was run on the host, bound to loopback, with one local 12B reasoning model, and checked with `ezbx engine detect` and with `ezbx gateway serve` on the host (`--listen 127.0.0.1:18080`, `--upstream` set to the engine). No box or Docker was involved; prompts were capped at 64 tokens.

| Engine | Version | Port | Detected as | Through the gateway | Control |
|---|---|---|---|---|---|
| LM Studio | `lms` CLI commit `43d4b26` | 1234 | LM Studio (by `/api/v1/models`) | model list, chat, streamed chat, `/v1/messages`, `/v1/responses` | `lms server start`, `lms unload --all`, `lms server stop` worked |
| llama.cpp | `llama-server` build 12648 (Vulkan) | 9931, its default with no `--port` | llama.cpp (by `/health`) | the same five | none (started and stopped by hand) |
| Ollama | 0.40.17 (`ollama serve` from the release zip) | 11434 | Ollama (by `/api/ps`), also with no models | the same five | `/api/ps` showed the model loaded; `POST /api/generate` with `keep_alive: 0` unloaded it |

- Every reply carried `X-Ezbx-Engine` with the right engine id, and `X-Ezbx-Model` when the request named a model.
- **Found:** LM Studio's `/v1/models` lists every downloaded model, embedding models too, so a harness config offers an embedding model as a chat model. Its native list says which is which (`type`), with the context size, image input and loaded state, but ezbx doesn't read it yet. **Fixed the same day:** the adapter reads that list (`models.list`), leaves out `type: embedding` (`models.skip`) and takes the context size from it; `ezbx engine detect` then showed one model with its 262,144-token context. The native keys were checked to be the OpenAI ids.
- **Found:** llama.cpp names its model by the model file's full path unless `llama-server` is given `--alias NAME`, and that name is written into the harness config inside the box. Start it with `--alias`. **Since the same day,** `ezbx engine detect`, the `ezbx init` review and `ezbx up` warn about a model named by a file path (checked with unit tests and a fake server; llama.cpp itself was not run again).
- **Found:** the gateway doesn't pass on why a reply ended. A reply cut off by `max_tokens` (`finish_reason: length`, seen here when a reasoning model spent its 64 tokens thinking) comes back from `/v1/messages` as `stop_reason: end_turn` instead of `max_tokens`, and from `/v1/responses` as `completed` instead of `incomplete`. **Fixed the same day:** with LM Studio and an 8-token limit, `/v1/messages` now says `max_tokens` (streamed too) and `/v1/responses` says `incomplete` with reason `max_output_tokens`, its stream ending in `response.incomplete`; with 256 tokens all four say the reply finished normally. 14 of 14 mutations were caught.
- With 8 tokens, the reasoning model spent all of them thinking and every reply was empty, which is why `ezbx init` asks whether a model is a reasoning model.

Not covered: these engines from a box through Docker Desktop's host alias (only Strata has been), and engines started with a key.

These results, and the ones above, were recorded with the per-engine adapters of ezbx 0.2. Those were later replaced by API kinds (design decision 22): ezbx now treats every one of these servers as an OpenAI-compatible API, and the one with LM Studio's own model list as the `lmstudio` kind, so testing each engine program from a box is no longer planned. The config forms named here (`engine.use`, `ezbx engine use strata`) are the old ones.

### Read-only folder masks (2026-11-23, before the code)

A hand-run check of the mounts planned in [readonly.md](readonly.md), so the design rests on something tested. A scratch folder on the host held `app.txt` and fake secrets (`.env`, `sub/.env.local`, `keys/server.pem`, `Upper/.ENV`, `.ssh/id_ed25519`). It was mounted with `--mount type=bind,...,readonly` into a throwaway container from the box's pinned base image, with `--network none`, every capability dropped and `no-new-privileges`. The four files were masked with `type=bind,src=/dev/null,readonly`, and `.ssh` with `type=tmpfs,tmpfs-size=4k,tmpfs-mode=0555`. The container ran once as user 1000 and once as root (still without capabilities).

- `app.txt` read normally; each masked file read as 0 bytes; `.ssh` listed nothing. Names kept their host case (`Upper/.ENV`).
- Refused for both users: creating, overwriting, deleting and renaming files, `mkdir`, writing into `.ssh`, `umount` of a mask, and `mount -o remount,rw` of the folder. `app.txt` was unchanged afterwards.
- **Found:** writing to a masked file "succeeds", because the write goes to `/dev/null`, which ignores a read-only mount for writes. The host file was checked afterwards and still held its text, so nothing leaks or changes; the doctor's check reads masks instead of writing them.
- **Found:** a tmpfs mask is mounted read-write unless `readonly` is added; only its 0555 mode kept both users out. With `readonly`, `/proc/self/mountinfo` showed `ro,nosuid,nodev,noexec` and `touch` failed with "Read-only file system". The design uses `readonly`.
- The folder reaches the VM through Docker Desktop's file share, which carries the whole host drive. A container escape would see it, as the threat model says.

Not covered: links and junctions inside a read-only folder, a large folder's walk time, and the masks as `ezbx up` will build them (they were typed by hand here). The test image and folder were removed afterwards.

### Read-only folders end to end (same day, after the code)

Box `vt` with Pi, built by `ezbx build` from `0778277` (with `tools` left out), on the fake engine. Its read-only folder held `app.txt`, `.env`, `sub/.env.local`, `.ssh/id_ed25519`, `keys/server.pem`, a `notes.txt` with a fake AWS key, and two Windows junctions to a folder outside it: `.aws` (a default folder mask) and `plainlink`.

- **`ezbx up`** noted `.aws` as a link it leaves alone, warned that `notes.txt` looks like it holds an AWS access key (without printing it), wrote `masks.json`, and started the box. `docker inspect` showed the folder at `/readonly/ref` and four masks, every one `RW=false`: three files bound to `/dev/null` and `.ssh` as a tmpfs.
- **From inside, as the agent:** `app.txt` read normally; every masked file read as empty; `.ssh` listed nothing. Writing a new file, overwriting `app.txt`, deleting `.env`, writing into `.ssh`, `umount` of a mask and `mount -o remount,rw` were all refused. `notes.txt` was readable, as the warning said.
- **Junctions:** both showed in the box as links to a path under `/mnt/host/` (how Docker Desktop shows a link to a host path), which doesn't exist in the box. Listing them and reading the outside file through them failed with "No such file or directory". This confirms leaving links alone ([readonly.md](readonly.md)). **Found:** the link's target, a host path, is visible to the agent with `readlink`. Not a secret, but a personal path; now listed under Limits in readonly.md.
- **`ezbx doctor --box vt`:** 26 passed, 1 warning (the AWS-shaped key in `notes.txt`). The read-only group passed: "mounted read-only, 4 masked" and "the agent can't write the folders, and every mask hides its file or folder".
- **New secret files:** after `ezbx shell` ran a session normally, `.env.prod` and `sub/id_rsa` were added on the host. `ezbx shell` (also with `--no-snapshot`) then refused to start, naming both, and `ezbx doctor` failed "new files" naming both. After `ezbx down` and `ezbx up`, 7 were masked, both read as empty, and the session started.
- **`ezbx net on`** printed the read-only warning naming `/readonly/ref`. **`ezbx down`** removed `masks.json`.
- **Found:** `up` said "5 masked" while 4 masks were mounted, counting the link. Fixed the same day: links are no longer counted.

Not covered: a link (not a junction) made on Windows, which needs rights this account doesn't have (Linux CI covers links in the walk); a large folder's walk time; `ezbx init` asking about the folder (covered by unit tests). Everything made was removed afterwards: box `vt`, its volume, the images and build cache (Docker was empty before), the fake engine and the scratch folders.

### Two boxes, `airgap`, and the runtime contract (2026-11-25)

A live run of the 0.4 commands against the fake engine (`tools/fakeengine`, OpenAI kind), from a freshly reset Docker Desktop (Engine 29.9.3), with two boxes in one `ezbx.toml`: `vt` (`off`) and `vt2` (`airgap`), both Pi with no tools.

- **`create`:** added both boxes without asking anything, and refused a third whose workspace sat inside `vt`'s (`invalid_args`). `ezbx build` built one box image for both (same inputs) and the gateway image.
- **`start`:** started both side by side. With two boxes and no `--box` it refused (`invalid_args`), and a second start of `vt` gave `box_already_running`. The result held each box's state: running, gateway healthy, image current.
- **`exec`:** ran as uid 1000 in `/workspace`, returned the command's exit code (3) and output in the result, and passed `--env CI=1` and `--cwd /tmp`. `--env GITHUB_TOKEN=x` was refused. `--timeout 2s` on a 300-second sleep ended it at about 2.2 s (`timeout`, `timedOut`, partial output kept), and afterwards no such process was left in the box.
- **The model route:** from `vt` the model list and a chat request answered 200 through the gateway; from `vt2` both got 403 `network_airgap`. `ezbx net airgap` on `vt` closed the route at once (403) and `ezbx net off` opened it again (200); `ezbx status` showed the running mode beside `ezbx.toml`'s.
- **The log:** model lines carried `tokens_in` and `tokens_out` (the fake engine reports 1 and 1), and the airgap refusal `blocked` with `network_airgap`.
- **`ezbx doctor`:** passed on both (24 checks on `vt`; 23 and one skip on `vt2`, where "airgap closes the model route" passed and the tiny completion was skipped). Its 11 proxy probes were marked `doctor` in the log, and `ezbx net` listed none as blocked.
- **`destroy`:** `destroy --json` without `--yes` was refused; `destroy --box vt2 --yes --remove-config --json` removed `vt2`'s containers, networks, volume, state folder and `ezbx.toml` section, kept both images (`vt` was using them), and left `vt` running. `ezbx status --box vt2` then said `missing`.
- **Found:** the comment `init` writes after `network` still listed three modes, and the doctor named its proxy check "network off" in `airgap`. Both fixed the same day.

Not covered: a real model server, harnesses using the `default` alias, translated streams against a server that refuses `stream_options`, and Linux.

### The 0.4 gateway against LM Studio (same day)

`ezbx gateway serve` on the host (no box or Docker), built from `521570b`, in front of LM Studio on loopback port 1234 with one small local reasoning model loaded. It ran twice, once with `--engine openai` and once with `--engine anthropic` (LM Studio serves both APIs), each with `--default-model` set. Every request was capped at 16 tokens.

| Request | OpenAI kind | Anthropic kind |
|---|---|---|
| `GET /v1/models` | 200 | 200 |
| Plain request (`/v1/chat/completions` or `/v1/messages`) | 200, logged 18 in / 16 out | 200, logged 11 / 11 |
| Streamed `/v1/chat/completions` with `stream_options.include_usage` | 200; LM Studio accepts the option and sends a usage chunk; logged 18 / 16 | translated to Messages; 200, logged 11 / 10 |
| Streamed `/v1/messages` | translated; 200, real counts in `message_delta`, logged 18 / 16 | passed through; logged 11 / 10 |
| Streamed `/v1/responses` | translated; 200, logged 18 / 16 | translated; 200, logged 11 / 11 |
| `"model": "default"` | rewritten; `X-Ezbx-Model` named the real model | the same |

- Translated streams now carry the server's real token counts (they were 1/1 placeholders before 0.4).
- A streamed `/v1/chat/completions` passed through without `stream_options` logged no counts, as designed: the gateway leaves such a stream as the harness asked for it.
- **Found:** LM Studio's Anthropic API has no `/v1/messages/count_tokens` and answers it with status 200 and an error body, which the gateway passed on as a reply (its fallback looked only for 404). **Fixed the same day:** the server's answer is used only when it is a 200 with a whole, non-negative `input_tokens`; anything else gets the local estimate. 12 of 12 mutations were caught.

Not covered: a harness in a box using `default` (harness configs still name the real model), a server that refuses `stream_options` with 400 (unit-tested), and servers other than LM Studio.

### Harnesses using the `default` alias (2026-11-25, after v0.4.0)

On Windows 11 with Docker Desktop, a Pi-only box started with `default_model = 'fake-model'` and its generated settings selected `default`. From inside the box, `GET /v1/models` showed `fake-model` and `default`. Pi answered a short prompt through `ezbx shell pi`, and the gateway log recorded the concrete model `fake-model` with HTTP 200.

A second box with all seven harnesses and `network = 'off'` used the same OpenAI-compatible fake engine. All seven answered through the gateway. Codex CLI used `/v1/responses`, Claude Code used `/v1/messages`, and Pi, OpenCode, oh-my-pi, Hermes Agent and OpenClaw used `/v1/chat/completions`. The gateway logged `fake-model` and HTTP 200 for each. These checks covered text replies.

## Native Linux pilot

The [Ubuntu 24.04 amd64 CI run](https://github.com/OranjSvensk/ezbx/actions/runs/45875263990) used Docker Engine 28.0.4, Pi, and a keyed OpenAI-compatible fake engine on the host's Docker bridge. The gateway read its key from a private read-only file and stayed unprivileged. The fixture checked a writable workspace, a masked read-only file, the isolated inside network, and `doctor`'s direct route checks. It also exercised the `default` model alias, engine-free `airgap` startup, exec timeouts, snapshot creation, and event replay through destroy.

The test used a temporary workspace writable by the container agent. Coverage is limited to the CI runner, Pi, and the fake engine. See [native Linux setup](getting-started.md#native-linux-pilot) for the supported pilot configuration.

## Limits of this result

- On the test machine, public DNS servers didn't answer even from the outside network, so the "public DNS blocked" checks prove less than they appear to. The checks against Docker Desktop's own DNS server, which did answer from the outside, are the meaningful ones.
- Only TCP and DNS over UDP were tested. Other UDP and ICMP were not.
- The proxy test used live public sites and public DNS. The address checks themselves are also covered by unit tests that need no network.
- One machine, one Docker Desktop version. `ezbx doctor` now repeats the main checks, so run it after every Docker Desktop upgrade.
