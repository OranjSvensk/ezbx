# Threat model

What ezbx protects, against whom, how, and what it leaves to you. It covers ezbx `v0.3.1` on Windows 11 with Docker Desktop, the only verified setup. Each claim points to where it is tested; [isolation-test.md](isolation-test.md) holds the live results, including the end-to-end test of read-only folders ([readonly.md](readonly.md)).

The short version: ezbx assumes the agent may misbehave, through a bad model, a prompt injection or a hostile file it reads, and limits what it can reach. It doesn't protect against a container escape, against malware already on your PC, or against you running, outside the box, something the agent wrote.

## What is protected

| Asset | Where it lives |
|---|---|
| Your files outside the workspace | The host. The box sees only the workspace and its own home volume and any read-only folders you add (which it can read, never change). |
| Secrets on your PC (SSH keys, cloud logins, `.env` files, browser profiles) | The host, never mounted. In read-only folders, masked by name. |
| The engine's key | `engine-key` in the box's state folder while the box is up; mounted read-only into the gateway only. |
| The admin token | `admin-token` in the state folder; mounted read-only into the gateway only. |
| Your network: LAN, the host's own services, cloud metadata | Reachable only through the gateway, which refuses local addresses in every mode. |
| Your undo history | Snapshots in the state folder, never mounted into the box. |
| Your PC's resources | Memory, CPU and process limits on the box. |

## Who might attack

1. **A misled agent.** A model that makes a bad call, or follows instructions hidden in a web page, issue, README or file it reads (prompt injection). This is the main case.
2. **A malicious or compromised harness**, including a dependency of one (a supply-chain attack on npm or PyPI).
3. **A hostile repository** opened in the workspace, whose files try to steer the agent or run code.
4. **A malicious model reply.** The same as 1 in effect: replies only cause harm through what the harness does with them.

Trusted: you, your inference engine, Docker Desktop and its VM, Windows, and other programs running as you. Any program running as you can already control Docker, read the state folder and read your files, so ezbx can't defend against one.

## Boundaries and controls

### Box to host files

- **Control:** only `/workspace` and the agent's home volume are writable. The box runs as user `agent` (uid 1000) with every capability dropped, `no-new-privileges`, no Docker socket, and no host folders except the workspace and any read-only folders, which are mounted read-only. `up` refuses a workspace that is a drive root, your home folder, a system folder, or overlaps ezbx's own folders. Snapshots live outside the box, so the agent can't rewrite its own history.
- **Proven by:** [isolation-test.md](isolation-test.md) ("The box image", "`ezbx up` and `ezbx down`", "A workspace the agent can't write"), `ezbx doctor`'s box lock-down and no-socket checks, `CheckWorkspace` tests.
- **Read-only folders:** mounted read-only, secrets masked by name, secret-looking contents warned about, and a session refused when a new secret file appears ([readonly.md](readonly.md)). Proven by unit tests, the mask test in [isolation-test.md](isolation-test.md) ("Read-only folder masks") and `ezbx doctor`'s "Read-only folders" checks.

### Box to network

- **Control:** isolation by topology. The box is only on an `--internal` network created with `gateway_mode_ipv4=isolated` and IPv6 off, so the gateway is the only thing it can reach. The gateway's proxy decides by mode: `off` refuses everything, `allow` only listed host names, `on` the public internet. In every mode it refuses local, private, link-local (including `169.254.169.254`), reserved and Docker Desktop addresses, checks every address a name resolves to and connects to the checked address, and allows ports 80 and 443 only. A mode switched with `ezbx net` lasts until the next `up`.
- **Proven by:** the probe in [isolation-test.md](isolation-test.md) (every TCP target and DNS path listed there), the gateway's address tests, and `ezbx doctor`'s throwaway check container, which repeats the port scans and proxy refusals.

### Box to engine

- **Control:** the gateway forwards to one fixed engine address, `/v1/...` paths only, GET, HEAD and POST only, with path checks (no `.`, `..`, `//`, backslashes or control characters), no protocol upgrades, size and time limits. The engine's key is added by the gateway and never enters the box. The gateway's log records requests without their content.
- **Proven by:** gateway tests, [isolation-test.md](isolation-test.md) ("With a real engine", "The engine key file").

### Box to gateway administration

- **Control:** the admin API listens only on the gateway container's own loopback, reached from the host with `docker exec`, and needs the token. The box has no route to it, and the proxy refuses `localhost`.
- **Proven by:** [isolation-test.md](isolation-test.md) ("The egress proxy and the admin API"), `ezbx doctor`'s admin API check.

### The human's root shell

- **Control:** `ezbx admin shell` is a separate temporary container, as root with Docker's normal capabilities (never privileged), no socket, and no network unless `--net`. The agent never gets it.
- **Proven by:** [isolation-test.md](isolation-test.md) ("`ezbx admin shell`, `reset` and `destroy`").

### What goes into the images

- **Control:** base images pinned by digest; harness versions and their dependency lockfiles pinned in `ezbx.lock` (npm with install scripts off, pip with `--require-hashes --no-deps --only-binary :all:`); `up` refuses images not built from the current inputs; CI actions pinned to commits; release archives with SHA-256 sums.
- **Proven by:** `TestLockFile`, the build checks in [build.md](build.md).

## What remains (residual risks)

These are real and not solved by ezbx. Most have a habit that helps.

| Risk | Why it remains | What helps |
|---|---|---|
| **The workspace runs on your PC later.** The agent can leave git hooks, editor tasks (`.vscode/tasks.json`), `package.json` scripts, build files or plain malicious code in the workspace, which run with your full rights when you open or build the project outside the box. | The workspace is meant to be written. | `ezbx review --diff` before you use the agent's work outside the box; build and test inside the box; look at `.git/hooks` and tool configs. |
| **The agent reads everything you give it** (the workspace, its home, read-only folders) and, with `allow` or `on`, can send it to any allowed host. `allow` filters by host name only, and common hosts such as code hosts and package registries accept uploads. | HTTPS isn't decrypted; filtering is by domain. | Keep secrets out of the workspace; use `off` when you can; approve hosts narrowly. |
| **Read-only masks are by name.** A secret in an ordinary file is readable; one created during a session is readable until the next `ezbx shell`. | Mounts are fixed while the box runs. | Keep secrets out of shared folders; heed the content warnings. |
| **A container escape** through a Linux kernel, runc or Docker Desktop bug. | Containers share the VM's kernel. | Keep Docker Desktop updated; run `ezbx doctor` after upgrades. |
| **A harness or its dependencies are malicious.** Pinning stops a silent change, not a bad version that was pinned. | ezbx trusts the versions in `ezbx.lock`. | The box contains it like a misled agent; review lockfile changes. |
| **The engine's own `/v1` API.** The agent can call any `/v1` route the engine offers, and can keep the GPU busy. | The gateway doesn't filter by route inside `/v1` or queue requests (an open question in [gateway.md](gateway.md)). | Use an engine whose `/v1` API holds nothing you'd mind the agent using. |
| **Release archives aren't signed.** Checksums prove an archive matches the release, not who built it. | Signing isn't set up. | Install with `go install`, or build from a checkout you trust. |

## Out of scope

- Malware or other people with access to your Windows account. They can control Docker, read the state folder and change ezbx itself.
- Attacks on the inference engine through the model files you load.
- Use on a shared or multi-user machine. ezbx is built for one person's PC.
- Linux and macOS hosts, until they're verified. The design doesn't assume Windows, but none of the results above were measured there.
- Cloud API keys. Not built yet (design decision 8); this page will cover them when they are.

## Reporting

See [SECURITY.md](../SECURITY.md). A way around any control above is a security bug.
