# `ezbx build`, `ezbx.lock` and tool packs

`ezbx build` makes ezbx's two images, the gateway and the box, from pinned inputs, so building again gives the same box. The box image also gets the tools you chose, from a small catalogue of verified tools and tool packs.

```text
ezbx build                  build the images ezbx.toml needs, if they aren't built yet
ezbx build --prune          then remove older ezbx images, after asking
ezbx build --prune --yes    the same, without asking
```

`ezbx init` builds missing images itself, right before it starts the box, and the plan screen says so first. `ezbx up` doesn't build: if an image is missing, it stops and says to run `ezbx build`.

## Decisions

These were decided before the code was written (design decisions 12 to 14).

| # | Question | Decision |
|---|---|---|
| 12 | Where the build files come from | They are carried inside the `ezbx` program (Go's `embed`): the Dockerfiles, the harness install files, the tool catalogue, and the Go source the gateway is compiled from. So `ezbx build` works without a copy of the repository. A test makes sure no file that git ignores is ever carried. |
| 13 | What `ezbx.lock` records, and what `up` accepts | `ezbx.lock` (committed, at the repository root) lists every pinned input: base image digests, each harness's package, version and lockfile hash, and each tool's Debian packages. It is generated from the files, and a test fails while it is out of date. What was actually built on a computer is recorded separately, in `images.json` next to `ezbx.toml`. `up` refuses an image that wasn't built from the current inputs. |
| 14 | Image names, and old images | Each image is named after a hash of its inputs, such as `ezbx-box:3f2a9c1d0b7e`, so a different input always gives a different name. A rebuild keeps the old images. `ezbx build --prune` removes older ones after asking, and always keeps the current images and the ones they replaced (so you can go back). |

Tool packs (also decided first):

- One small data file per tool, in `catalog/tools/<id>.json`, and one per pack, in `catalog/packs/<id>.json`. A pack is a named list of tools.
- Tools are Debian packages, pinned as described below. A direct download with a checked hash will be added only for a tool Debian lacks or has too old.
- Starting packs: `coding` (curl, jq, less, make, python3, unzip), `web` (Chromium) and `media` (FFmpeg, ImageMagick). Each tool is verified in a real build before it ships. fd and ripgrep are in every box, because Pi needs them, so no pack lists them and `init` doesn't offer them.
- You choose in `ezbx.toml`, with packs and single tools mixed: `tools = ["pack:coding", "ffmpeg"]`. `ezbx init` asks, with `pack:coding` as the default for a new config.

## How the images are named

For each image, `ezbx` makes the build context as a tar stream: the build files, sorted, with fixed times, owners and modes, so the same files always give the same bytes. The image's name is the first 12 hex digits of a SHA-256 hash over that stream, the Dockerfile's path and the build arguments (for the box, the chosen tools). The full hash is stored on the image as the label `ezbx.inputs`.

- **Gateway:** the whole carried source, because the program carries its own build files and needs them all to compile. Any change to ezbx's code gives a new gateway image. (Before this, the gateway image had to be rebuilt by hand after code changes, and forgetting it meant an old gateway.)
- **Box:** `docker/box.Dockerfile`, the install script, lockfile and `package.json` of each harness the box has (`harnesses` in `ezbx.toml`), and the chosen tools. A harness's `manifest.json` isn't part of it, so editing its notes doesn't rebuild the box. Only the box's own harnesses go in (design decision 20), so a box with Pi alone doesn't carry OpenCode's 250 MB, and adding or removing a harness gives a new image name: run `ezbx build`, `ezbx down` and `ezbx up` after changing `harnesses`. `ezbx build` without a config builds every verified harness.

`up` works out the same names from the same files and `ezbx.toml`, and checks that each image exists and carries the right `ezbx.inputs` label. An image with the right name but another label was made some other way (for example by hand with `docker build -t`), and is refused.

## What is pinned, and how

| Input | Pinned by |
|---|---|
| Base images (`golang`, `node`) | Digest, as an `ARG NAME_IMAGE=name:tag@sha256:...` default in the Dockerfile. A test refuses any `FROM` that isn't pinned. |
| Debian packages (git, the tools) | The base image's own snapshot. The Node image records in its apt sources the dated [snapshot.debian.org](https://snapshot.debian.org) archive it was made from, and the box Dockerfile points apt at that snapshot. So the base digest also fixes every package version. apt still checks the snapshot's signatures; only the "valid until" date of its release file is ignored, because an old snapshot's has expired. |
| Harnesses and what they pull in | `package-lock.json` with integrity hashes (installed with `npm ci --ignore-scripts`) or `requirements.lock` with wheel hashes (installed with `pip install --no-deps --require-hashes --only-binary :all:`), each recorded by hash in `ezbx.lock`. |
| Go modules (the gateway) | `go.sum`, as before. |

`images.json` records, per image: its name, Docker's image ID, the full inputs hash, when it was built, the name it replaced, and for the box the snapshot date, each Debian package's version and Pi's version. These are read back from the finished image by a throwaway container with no network and no capabilities.

## The tool catalogue

```json
{
  "schema": 1,
  "id": "fd",
  "name": "fd",
  "description": "Fast file finder. Debian names the command fdfind; fd is linked to it.",
  "debian": ["fd-find"],
  "commands": ["fdfind", "fd"],
  "links": { "fd": "fdfind" },
  "verified": { "status": "verified", "observedOn": "YYYY-MM-DD", "notes": "what was checked" }
}
```

- `debian`: the packages to install. `commands`: what must exist afterwards; the build fails if one is missing. `links`: extra command names, as symbolic links in `/usr/local/bin`, each also listed in `commands`.
- Only verified tools can be built in, and a pack may only list verified tools (a test checks the real catalogue).
- A harness lists the tools it always needs in its manifest (`"tools": ["fd", "ripgrep"]` for Pi). They are added whatever the config says.
- Unknown fields, bad package or command names, and unverified tools are all refused, with the reason.

Tools are installed when the image is built, never by the agent: the agent has no root and no package manager rights. To add a tool, change `tools` in `ezbx.toml`, then run `ezbx build`, `ezbx down` and `ezbx up`.

Sizes measured on the first build: the box image is 726 MB with `pack:coding` and 2 GB with all three packs. The gateway image is 13 to 15 MB. With OpenCode beside Pi and no tools, the box is 1.05 GB; with all seven harnesses and no extra tool packs, it is 4.28 GB.

Chromium runs with `--no-sandbox` in the box. Its own sandbox needs kernel features the box drops on purpose, and Debian's alternative (`chromium-sandbox`) is a setuid helper, which the image removes. The box is the sandbox.

## Pi's own downloads

Pi looks for `fd` (or `fdfind`) and `rg` when it starts, and if they're missing it downloads the latest release from GitHub. That bypasses pinning, and the box's network blocks it anyway (the first hand run showed a 403 from the proxy). Pi's manifest therefore needs both tools, and `PI_OFFLINE=1` turns off Pi's startup network calls: tool downloads, version and package update checks, its online model catalogue and install telemetry. Model requests to the gateway are not affected. Like every harness's `env`, it is set on the box container by `ezbx up`, not in the image.

## Prebuilt binaries and Python wheels

OpenCode's npm package, `@opencode/cli`, is only a launcher. Its `opencode` command is a stub that prints an error until the package's install script has run, and that script copies the real program out of a platform package, running `npm install` by itself if the platform package is missing. Install scripts stay off in ezbx, so OpenCode's lockfile pins the platform package directly: `@opencode/cli-linux-x64-baseline`, one prebuilt program of about 250 MB with no install script and no dependencies, checked by its integrity hash like any npm package. The baseline build runs on every x64 CPU; the other build needs AVX2, so choosing between them at build time would make the image depend on the computer that built it.

OpenCode fetches its model catalogue from `models.opencode.ai` every time it runs (seen as blocked requests in the gateway log); `OPENCODE_DISABLE_MODELS_FETCH=1` in its manifest turns that off, and its models come from the generated config instead. It also talks to a background service of its own over the box's loopback, so the box's `NO_PROXY` lists `localhost`, `127.0.0.1` and `::1` beside the gateway. Loopback in the box is the box itself, so nothing leaves it that way.

The other five harnesses follow the same rule (install scripts off, everything pinned by hash):

- **Codex CLI (`codex`)** and **Claude Code (`claude`)** also ship their main npm packages as wrappers around platform binaries. Their lockfiles pin `@openai/codex@0.171.0-linux-x64` (a static Linux x64 musl binary) and `@anthropic-ai/claude-code-linux-x64@2.1.330` directly. Both set their startup update-check and telemetry switches off and talk to the gateway through its `/v1/responses` and `/v1/messages` translation routes.
- **oh-my-pi (`omp`)** runs on the Bun runtime and uses prebuilt Linux x64 native bindings (`@oh-my-pi/pi-natives-linux-x64`). Its lockfile pins `@oh-my-pi/pi-coding-agent@19.1.0` alongside `@oven/bun-linux-x64-baseline@1.4.5`, and its `/usr/local/bin/omp` wrapper runs the CLI under that pinned baseline Bun executable.
- **OpenClaw (`openclaw`)** runs on Node 24 from its `package-lock.json` with scripts off.
- **Hermes Agent (`hermes`)** is a Python package (`hermes-agent@0.19.0`). When a harness folder in the build context has a `requirements.lock`, the harness build stage installs `python3` and `python3-pip` from the pinned Debian snapshot first, then runs `python3 -m pip install --no-deps --require-hashes --only-binary :all:` as `node`. Every transitive wheel (61 wheels) is listed in `requirements.lock` with its SHA-256 hash, so no source archive or `setup.py` runs. Its install script marks `packages/.install_method` as `docker`, and its manifest disables lazy runtime installs and `tirith` binary downloads.

## Not built yet

- Direct downloads with checked hashes, for tools Debian lacks.
- The disk space check in `init` (image sizes are now known: see [isolation-test.md](isolation-test.md)).
- A doctor check that the running containers use the current images.
