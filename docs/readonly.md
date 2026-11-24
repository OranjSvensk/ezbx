# Read-only folders and secret masking

A read-only folder is a host folder the agent may read but never change, such as other projects it should learn from or a reference checkout. Common secret files inside it are hidden from the box. This page is the design, and all of it is built (`internal/readonly`, `internal/box/readonly.go` and `masks.go`, `internal/doctor/readonly.go`, `internal/setup/readonly.go`). It has been run end to end against a real box ([isolation-test.md](isolation-test.md), "Read-only folders end to end"). The risks it does and doesn't cover are in [threat-model.md](threat-model.md).

```toml
[[box.main.readonly]]
path   = "~/projects"            # the host folder
name   = "projects"              # optional: shown in the box as /readonly/projects
mask   = ["secrets.yml", "private/"]   # hidden on top of the defaults below
unmask = [".npmrc"]              # optional: a default you want visible, named exactly
```

## Decisions

| # | Question | Decision |
|---|---|---|
| R1 | How masking works | The folder is bind-mounted read-only. Each file that matches a mask gets an empty read-only file mounted over it, and each matching folder an empty read-only folder. Chosen over copying the folder into a volume without its secrets (stale at once, slow and costly for large folders) and over listing the files to include (safest, but tedious for real projects). |
| R2 | Where it appears | `/readonly/<name>`, where `name` defaults to the folder's own name, lowercased. Two folders with the same name are refused; set `name` on one. Chosen over mirroring the host path, which would put personal paths inside the box and make harness settings depend on them. |
| R3 | Default masks | A built-in list always applies (below), and `mask` adds to it. Turning a default off takes `unmask` on that folder, naming the default exactly; the review screen and `ezbx doctor` show every unmasked default. Chosen over masking only what is listed (one forgotten `.env` is enough) and over one switch that drops all defaults. |
| R4 | Secret-looking contents under ordinary names | `ezbx init` lists them and offers to mask each. `ezbx up` and `ezbx doctor` warn and still start. The content check is a heuristic, so refusing on it would mostly block on false alarms. |
| R5 | Secret files that appear after `up` | Mounts can't change while the box runs. `ezbx shell` checks the names again before each session (alongside its snapshot) and refuses to start if a file now matches a mask but isn't masked; the fix is `ezbx down`, then `ezbx up`. Names only, no contents, so the check stays quick. |
| R6 | Which folders may be read-only | The same refusals as the workspace: drive roots, your home folder itself, system folders and ezbx's own folders. Also refused: overlap with the workspace or with another read-only folder, and a folder whose own name is a default folder mask (`~/.ssh`, `~/.aws`). This replaces the earlier plan in [init-flow.md](init-flow.md) to allow a whole drive with a warning. |
| R7 | Read-only folders and the network | With `off`, the only way out is the model route to your own engine. With `allow` or `on`, an agent could send what it reads to an allowed host. The `init` review screen, `ezbx net allow` and `ezbx net on` name the read-only folders in a warning. Nothing is refused. |

## The default masks

Matched against each file or folder name at any depth, ignoring case (Windows doesn't tell `.ENV` from `.env`). A trailing `/` matches folders only.

| Kind | Patterns |
|---|---|
| Environment files | `.env`, `.env.*` |
| Keys and certificates | `*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa*`, `id_ecdsa*`, `id_ed25519*` |
| Credential folders | `.ssh/`, `.aws/`, `.gnupg/`, `.kube/`, `.azure/`, `.config/gcloud/` |
| Credential files | `.netrc`, `.npmrc`, `.pypirc`, `.git-credentials`, `.docker/config.json`, `credentials.json` |

`.env.example` and similar files are masked too (`.env.*`); `unmask = [".env.*"]` shows them again, along with every other `.env.` file in that folder. `.npmrc` and `.pypirc` are on the list because they often hold registry tokens.

**Pattern rules** (for `mask` and `unmask`): `*` and `?` match within one name. A pattern with a `/` in the middle (`.docker/config.json`) matches that path ending at any depth; start it with `./` (`./config/secrets.yml`) to match only from the folder's root. No `..`, no leading `/`, no backslashes, no empty patterns. An `unmask` entry that isn't a default is refused, so a typo can't silently do nothing.

## How it works

### At `ezbx up`

1. **Check the folders** (R6), then walk each one on the host. The walk never follows links or Windows junctions and doesn't descend into masked folders. A walk past 200,000 entries stops `up` and suggests a narrower folder, so a mistaken large folder can't stall it.
2. **Build the mask list.** Every match becomes one mount. More than 256 mounts stops `up` and names the pattern with the most matches, so you can mask its parent folder instead. The list is saved as `masks.json` in the box's state folder (never mounted into the box) for the check in R5.
3. **Look for secret-looking contents** (R4) in files up to 1 MB that aren't masked: private key headers, well-known token shapes (cloud, code host and API keys), and credentials written into a URL in `.git/config`. Warnings name the file and the kind of secret, never the text that matched.
4. **Start the box** with, for each folder, `--mount type=bind,src=<folder>,dst=/readonly/<name>,readonly`, then one mount per mask:
   - a file: `--mount type=bind,src=/dev/null,dst=<path>,readonly`. It reads as empty, and anything written to it is thrown away (a device ignores the read-only flag for writes, but the host file is never reached). `/dev/null` is Docker's VM's own, so no host file is involved and nothing from the state folder is mounted (design decision 9).
   - a folder: `--mount type=tmpfs,dst=<path>,tmpfs-size=4k,tmpfs-mode=0555,readonly`, an empty read-only folder owned by root. Without `readonly`, Docker mounts it writable and only the folder's mode keeps the agent out.

Mounts are applied parent first, so each mask lands on top of the read-only folder. Read-only is enforced by the mount itself, not file permissions; the agent has no capabilities, so it can't remount anything.

**Links are left as they are.** A link or junction whose name matches a mask is listed in `masks.json` and noted by `up`, but nothing is mounted over it. In the box, a link points inside the box, never to the host (Docker Desktop shows a link to an absolute host path as one into a folder the box doesn't have), and whatever it points to inside the folder is masked by its own name. Mounting over a link would instead make Docker follow it inside the container. This replaces the first plan to mask the link itself; the end-to-end run confirmed it with Windows junctions.

### At `ezbx shell`

Before the session (and its snapshot), the names are walked again (R5). A newly matching name that isn't in `masks.json` refuses the session with the file's path and the fix (`ezbx down`, then `ezbx up`). A masked file that was removed is fine. During a session, nothing is checked; see [threat-model.md](threat-model.md).

### Elsewhere

- **Snapshots** cover the workspace only; the agent can't change a read-only folder.
- **`ezbx admin shell`** doesn't mount read-only folders. It's for fixing the workspace and the agent's home.
- **`reset`, `destroy`, `down`** never touch the folders.
- **`ezbx doctor`** has a "Read-only folders" group. From `docker inspect`: each recorded folder is mounted, and read-only, and nothing under `/readonly` is writable. From inside the box, as the agent: writing to each folder fails, each masked file is a character device (`/dev/null`; writing to one isn't a useful check, since it accepts and discards), and each masked folder is an empty tmpfs that refuses writes. The paths reach the box in an environment variable, never in the script, so a file name can't run as a command. On the host, it walks each folder again: new files that match a mask fail (R5), and unmasked defaults (R3) and secret-looking contents (R4) warn.
- **`ezbx init`** asks for read-only folders after the workspace (none by default; `--readonly PATH`, given again for more, or `--readonly none`), keeps or drops each existing one, runs the same checks as `up`, and asks about each file that looks like a secret (yes by default, so `--yes` hides them; the answer is written as a `./path` mask). The review screen shows each folder, how many files are hidden and any `unmask`, and the network warning (R7). Rerunning `init` keeps a folder's `name`, `mask` and `unmask`.
- **`ezbx net on`, `ezbx net allow` and `ezbx net allow HOST`** print the network warning (R7) when the running box has read-only folders.

## Proven live, and still to prove

On Docker Desktop for Windows, a host folder reaches the container through Docker's VM. A hand-run test (2026-11-23, [isolation-test.md](isolation-test.md), "Read-only folder masks") confirmed that `/dev/null` and tmpfs masks land on top of paths inside a read-only bind mount of a Windows folder, that every write, delete, rename, unmount and remount was refused for both the agent and a root user without capabilities, and that names in the box keep their host case. The end-to-end run (same day, "Read-only folders end to end") confirmed the rest with a real box: `up` with its masks, the view from inside as the agent, Windows junctions left alone and unreachable, `ezbx shell` and `ezbx doctor` catching new secret files, and `down` removing the record. Still to prove: a large folder's walk time, and a link (not a junction) made on Windows.

## Limits

- Masks hide by name, plus warnings by content. A secret in an ordinary source file (a key pasted into `config.go`) is readable, unless the content check happens to spot it.
- A secret file created on the host during a session is readable until that session ends (R5 catches it at the next `ezbx shell`).
- The folder's other contents are readable by the agent, and with the network `allow` or `on` it could send them to an allowed host (R7).
- File names, sizes and dates of masked files stay visible; only their contents are hidden.
- A link's target stays visible with `readlink`. For a link to a host path, that is the host path (on Docker Desktop, under `/mnt/host/`), which may name your user folder. What it points to can't be read from the box.
