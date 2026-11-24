# `ezbx init` flow (draft 0.1)

`ezbx init` is the one command that takes a new user from nothing to a working box. This page describes what it asks, what it checks, what it writes and what happens when something goes wrong. Naming: the command and files are `ezbx`, the brand is ezbx.

**Built.** What the first version does differently from the design below, or doesn't do yet:

- `--config FILE` is the config file to read and write, the same as for every other command (not a separate file of answers). If it exists, `init` starts from it.
- `--plan` never prompts: it takes the defaults and flags, prints the plan, and stops, so it can be used in scripts.
- `init` doesn't start model servers; ezbx knows servers only by the API they speak, not by the program. If none answers, `init` says so and can still write the config (`--later`).
- An address given with `--url` where nothing answers yet is written to the config anyway, and the plan says the box won't start yet. Moving to a new address drops the old `key_env` and `host`, so a key never follows to another server.
- For each model, `init` asks whether it is a reasoning model, because servers don't say and a reasoning model's answers can be cut off without it (`--reasoning` answers yes for all).
- After the harnesses, `init` asks which tool packs and tools go into the box image ([build.md](build.md)). A new config starts with `pack:coding`. An existing config keeps its `tools` setting, and one written before tools existed keeps none, so taking every default still changes nothing. `--tools pack:coding,ffmpeg` or `--tools none` answers it.
- Images are built by `ezbx build` right before `ezbx up`, only if they aren't built yet; the plan says which. `ezbx.lock` is part of the repository, not written by `init` (see [build.md](build.md)). With `--no-start`, nothing is built.
- Not yet: `--here`.
- The disk space check is a warning on the review screen, not a refusal. It measures the drive of the state folder (snapshots) and of Docker's data folder in its usual place (`%LOCALAPPDATA%\Docker` on Windows, `~/Library/Containers/com.docker.docker` on macOS, `/var/lib/docker` on Linux), once per drive. When images must be built it asks for twice the estimated box image (about 750 MB for the first harness and 600 MB for each more, from the sizes in [isolation-test.md](isolation-test.md), doubled for Docker's build cache) plus 1 GB for snapshots; otherwise 1 GB. Docker Desktop can keep its disk elsewhere, and tool packs such as Chromium add more, so the number is a guide.
- On a box that is already up, `init` writes a changed config but doesn't restart the box. It says to run `ezbx down`, then `ezbx up`.

## Principles

1. **Show the plan before changing anything.** Every run ends with a review screen listing exactly what will be created, downloaded and changed. Nothing happens until you confirm.
2. **Safe defaults.** Network off, one writable folder, no extra folders exposed, no keys kept on disk (a model server's key is copied into the state folder for the gateway only while the box exists).
3. **Never touch what it didn't create.** Every Docker resource ezbx makes carries a label, and ezbx only ever changes labelled resources.
4. **Re-runnable.** Running `init` again reads the existing setup, shows what would change, and offers to add a harness or point at another server. It never silently replaces your config or regenerates secrets.
5. **Same questions without prompts.** Every prompt has a flag, so the whole flow can run unattended.
6. **No telemetry.** The only network traffic is what Docker needs to pull images and what each harness's install needs.

## Run modes

```text
ezbx init                 guided, with prompts
ezbx init --plan          show what it would do; change nothing
ezbx init --yes           accept detected defaults without prompting
ezbx init --config FILE   take every answer from a config file
```

Useful flags: `--profile NAME`, `--list-profiles`, `--url ADDRESS`, `--api auto|openai|lmstudio|anthropic`, `--key-env NAME`, `--later`, `--harness a,b`, `--workspace PATH`, `--network off|allow|on`, `--no-start`, `--here` (write config next to the current folder, for developers).

Prompts are plain text questions in version 0.1. Arrow-key menus can come later without changing the flow.

## The steps

### 0. Preflight

Checks, in this order, each with a plain-language fix if it fails:

- The operating system and architecture are supported.
- The Docker command exists, and the Docker engine answers.
- Enough free disk space for the images and snapshots (built as a warning on the review screen; see above).
- The chosen workspace parent folder exists and is writable.

If the Docker engine isn't running, `init` says so and offers to wait while you start it. It does not start other programs without asking. It also does not install Docker.

### 1. Model server

1. Look at the common local ports, the configured `url` and any `--url`, and show each server found with the API it speaks (see [engine-adapters.md](engine-adapters.md)):

   ```text
   Model servers found on this computer (any OpenAI- or Anthropic-compatible API works)
     1. LM Studio API            http://127.0.0.1:1234, 3 model(s)
     2. OpenAI-compatible API    http://127.0.0.1:8080, 1 model(s)
     L. I'll start one later
     Not listed? Run ezbx init --url http://127.0.0.1:PORT
   Use which? [1]
   ```

2. The configured address, if it answers, is the default, then the first one found. `--api KIND` lists only servers that speak that kind. If none is found, `init` finishes with a note, because the box works without a running model and the gateway just reports "engine not reachable".
3. The chosen address is written as `url` under `[engine]`, and `api` as the kind it answered (or `auto`, with `--api auto`). `ezbx up` contacts only that address.
4. If the server requires a key, give the **name of an environment variable** that holds it with `--key-env NAME`. The key is never typed into a file. (The operating system's credential store comes after 0.1; decided below.) The key is only ever sent to `url`. A server that wants the key for its model list answers detection with 401; `init` says so and shows the command to use it: `ezbx init --url ADDRESS --key-env NAME`, which sends the key to that address only.

### 1b. Starter profile (new configs, or `--profile`)

For a new config, `init` offers the starter profiles ([profiles.md](profiles.md)), defaulting to `offline` so `ezbx init --yes` stays offline. Choosing one sets defaults for harnesses, tools, network mode and allowed hosts; every step after it (and flags such as `--harness`, `--tools` and `--network`) can still override them, and explicit fields are written to `ezbx.toml` (no hidden `profile` setting). Re-runs with an existing config skip this step unless `--profile NAME` is passed. `ezbx init --list-profiles` prints all profiles and exits.

### 2. Harnesses

A checklist grouped by kind (coding tools first, assistant-style tools after).

- Each line shows its pinned version and its `verified` state. Unverified ones are marked and not preselected.
- Pi is preselected while it is the only verified-in-practice option.
- If a chosen harness needs the internet (a cloud-backed tool), the review screen reminds you that it won't work while the network is `off`.

### 3. Folders

1. **Writable workspace.** Default is a folder inside the user's home, shown with the exact path. It is created if missing. These are refused with a clear reason: a drive root, the home folder itself, a system folder, or a folder that already contains ezbx's own config or state.
2. **Read-only folders (optional, none by default; `--readonly PATH`).** Existing ones are kept or dropped one by one. For each one added, `init` refuses the same folders as for the workspace, plus any overlap with the workspace or another read-only folder. It applies the default masks, lists files whose contents look like secrets and offers to mask each one, and warns that what the agent can read could be sent out if the network is `allow` or `on`. See [readonly.md](readonly.md).

### 4. Network mode

Default `off`. A one-line explanation of each mode is shown. If `allow` is chosen, the allowlist is built from the chosen harnesses' domains plus the small default list, and shown for review.

### 5. Limits

Memory defaults to **half of the memory Docker has**, rounded down to whole gigabytes, at least 2 GB and at most 8 GB, so the box can't take over a gaming or work session. On Docker Desktop the box runs inside Docker's own virtual machine, so its memory (`docker info`), not the computer's, is what counts. There is no CPU limit by default, and the process limit is 512. All are editable, and the review screen shows them.

### 6. Review

```text
ezbx plan (nothing has been changed yet)

Engine        LM Studio API at http://127.0.0.1:1234, 3 model(s)
Harnesses     Pi 1.5.0 (observed in a prior setup, will be re-checked on first build)
Workspace     <workspace path>   (will be created)
Read-only     none
Network       off
Memory        8g
Disk          120.0 GB free at <state folder>, the state folder (snapshots) and Docker's images

Will create
  2 Docker networks, 1 volume, 2 containers, 1 image set (build needs internet once)
  config file       <config folder>/ezbx.toml
  state folder      <state folder>   (logs, snapshots, admin token)

Continue? [y/N]
```

### 7. Apply

In order, each step recorded so a failed run can resume:

1. Write the config and lock file.
2. Create the networks and volume, all labelled.
3. Build the base image and add each chosen harness's install layer.
4. Generate each harness's config from the server's model list.
5. Create and start the gateway, then the box.
6. Run `ezbx doctor`.
7. Print what to do next, such as `ezbx shell pi`.

If a step fails, `init` stops, explains in plain words, and leaves the earlier steps in place. Running `init` again resumes from the failed step. `ezbx destroy` removes everything ezbx created (never your workspace) if you want a clean slate.

## Where files go

| Kind | Location | Notes |
|---|---|---|
| Config | per-user config folder | `ezbx.toml` |
| State | per-user state folder, one folder per box | logs, snapshots, admin token. Never inside the workspace and never mounted into the box. |
| Built images | next to the config | `images.json`: what `ezbx build` built on this computer. The pinned inputs are in `ezbx.lock`, in the repository. |
| Workspace | chosen in step 3 | the only writable mount |

`--here` keeps config next to the current folder instead, which suits developers working from a clone of the repo.

The folders per operating system are design decision 9 in [design.md](design.md): `%LOCALAPPDATA%\ezbx` on Windows, `~/Library/Application Support/ezbx` on macOS, and the XDG config and state folders on Linux. State is private to the user and kept out of any mounted folder.

## Docker resource naming

All names start with `ezbx-` and include the box name, for example `ezbx-main-gateway`, `ezbx-main-box`, `ezbx-main-inside`, `ezbx-main-home`. Every resource also has a label (`ezbx.managed=true`) so tools can find exactly what belongs to ezbx.

## Failure and edge cases

| Situation | Behavior |
|---|---|
| Docker missing | Explains how to install it. Does not install it. |
| Docker engine stopped | Offers to wait while you start it. |
| No model server found | Continues if you choose, with a clear note. |
| Server only listening on loopback outside Docker Desktop | `doctor` explains the limitation (design decision 4). |
| Offline machine | Build steps fail clearly. Everything not needing the network is still done. |
| Harness install fails | The harness is skipped, the others continue, and the failure is shown. |
| Existing setup | Shows a diff of what would change and asks first. |

## Decided here

- **Plain prompts first**, with every question also available as a flag (closes open question 2 in `design.md`).
- **Config and state live in per-user folders, not in the repo and not in the workspace** (closes open question 3).

- **Keys come from environment variables only in 0.1.** `init` asks for the name of the variable. Using the operating system's credential store (a different interface on each system) comes later.
- **The default memory limit** is half of Docker's memory, rounded down to whole gigabytes, from 2 to 8 GB (step 5).
- **`init` never starts Docker Desktop.** It says Docker isn't running, offers to wait, and carries on when Docker answers. Starting it would need to know where Docker Desktop is installed on each system.
- **`init --plan` makes no network calls.** It lists which images are already on the computer and which would be downloaded. Whether a download works shows when the plan is applied.

## Open questions

None at the moment.
