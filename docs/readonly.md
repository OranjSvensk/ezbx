# Read-only folders and secret masking (design, not built yet)

A read-only folder is a host folder the agent may read but never change, such as other projects it should learn from or a reference checkout. Common secret files inside it are hidden from the box. This page is the design; until it is built, `ezbx up` refuses a config with `[[box.<name>.readonly]]` entries. The risks it does and doesn't cover are in [threat-model.md](threat-model.md).

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

1. **Check the folders** (R6), then walk each one on the host. The walk never follows links or Windows junctions (a link is masked or shown as the link itself) and doesn't descend into masked folders. A walk past 200,000 entries stops `up` and suggests a narrower folder, so a mistaken large folder can't stall it.
2. **Build the mask list.** Every match becomes one mount. More than 256 mounts stops `up` and names the pattern with the most matches, so you can mask its parent folder instead. The list is saved as `masks.json` in the box's state folder (never mounted into the box) for the check in R5.
3. **Look for secret-looking contents** (R4) in files up to 1 MB that aren't masked: private key headers, well-known token shapes (cloud, code host and API keys), and credentials written into a URL in `.git/config`. Warnings name the file and the kind of secret, never the text that matched.
4. **Start the box** with, for each folder, `--mount type=bind,src=<folder>,dst=/readonly/<name>,readonly`, then one mount per mask:
   - a file: `--mount type=bind,src=/dev/null,dst=<path>,readonly`. It reads as empty, and anything written to it is thrown away (a device ignores the read-only flag for writes, but the host file is never reached). `/dev/null` is Docker's VM's own, so no host file is involved and nothing from the state folder is mounted (design decision 9).
   - a folder: `--mount type=tmpfs,dst=<path>,tmpfs-size=4k,tmpfs-mode=0555,readonly`, an empty read-only folder owned by root. Without `readonly`, Docker mounts it writable and only the folder's mode keeps the agent out.

Mounts are applied parent first, so each mask lands on top of the read-only folder. Read-only is enforced by the mount itself, not file permissions; the agent has no capabilities, so it can't remount anything.

### At `ezbx shell`

Before the session (and its snapshot), the names are walked again (R5). A newly matching name that isn't in `masks.json` refuses the session with the file's path and the fix (`ezbx down`, then `ezbx up`). A masked file that was removed is fine. During a session, nothing is checked; see [threat-model.md](threat-model.md).

### Elsewhere

- **Snapshots** cover the workspace only; the agent can't change a read-only folder.
- **`ezbx admin shell`** doesn't mount read-only folders. It's for fixing the workspace and the agent's home.
- **`reset`, `destroy`, `down`** never touch the folders.
- **`ezbx doctor`** gains a "read-only folders" check from inside the box: writing to each folder fails, each masked file reads as empty (writing to one isn't a useful check, since `/dev/null` accepts and discards it), each masked folder lists nothing and refuses writes. It also compares `docker inspect` (every read-only source mounted read-only) with the config, and warns about unmasked defaults (R3) and secret-looking contents (R4).
- **`ezbx init`** asks for read-only folders after the workspace (none by default), runs the same checks, lists what it found, and offers to mask each finding (R4). The review screen shows each folder, its mask count and any `unmask`.

## Proven live, and still to prove

On Docker Desktop for Windows, a host folder reaches the container through Docker's VM. A hand-run test (2026-11-23, [isolation-test.md](isolation-test.md), "Read-only folder masks") confirmed that `/dev/null` and tmpfs masks land on top of paths inside a read-only bind mount of a Windows folder, that every write, delete, rename, unmount and remount was refused for both the agent and a root user without capabilities, and that names in the box keep their host case. Still to prove once it's built: how long a walk of a large folder takes, and links and junctions inside a read-only folder.

## Limits

- Masks hide by name, plus warnings by content. A secret in an ordinary source file (a key pasted into `config.go`) is readable, unless the content check happens to spot it.
- A secret file created on the host during a session is readable until that session ends (R5 catches it at the next `ezbx shell`).
- The folder's other contents are readable by the agent, and with the network `allow` or `on` it could send them to an allowed host (R7).
- File names, sizes and dates of masked files stay visible; only their contents are hidden.
