# Starter profiles

Starter profiles are ready-made combinations of harnesses, built-in tools, network mode and allowed hosts that `ezbx init` offers when setting up a new box.

```text
ezbx init --list-profiles
ezbx init --profile web-dev
ezbx init --profile python-ml --yes
```

## Included profiles

| ID | Name | Harnesses | Tools | Network | Allowed hosts |
|---|---|---|---|---|---|
| `offline` | Offline coding | `pi` | `pack:coding` | `off` | none (default) |
| `open` | Open workspace | `pi` | `pack:coding`, `pack:web`, `pack:media` | `on` | all public hosts (LAN/host still blocked) |
| `python-ml` | Python and ML | `pi` | `pack:coding` | `allow` | `files.pythonhosted.org`, `github.com`, `huggingface.co`, `pypi.org` |
| `research` | Research | `pi` | `pack:coding`, `pack:web` | `allow` | `arxiv.org`, `en.wikipedia.org`, `github.com`, `raw.githubusercontent.com` |
| `web-dev` | Web development | `pi` | `pack:coding`, `pack:web` | `allow` | `api.github.com`, `github.com`, `raw.githubusercontent.com`, `registry.npmjs.org` |

Notes on what is built into the image:

- `pack:web` adds headless Chromium (several hundred MB; see [build.md](build.md)). If you want a smaller `research` box, pick `--tools pack:coding` during setup.
- `open` builds all three tool packs (`pack:coding`, `pack:web`, `pack:media`) and sets `network = "on"` for full public internet access while keeping the box non-root and isolated from your LAN, host loopback services and files outside `/workspace`.
- `python3` in `pack:coding` is Debian's Python 3 without `pip` or `venv` yet (`python-ml` still sets the Python/PyPI/Hugging Face allowlist and everyday coding tools).

## How they work

- **Explicit settings, no hidden state.** Each profile is a data file in `catalog/profiles/<id>.json`, validated against the tool catalogue, verified harnesses and hostname rules (`config.HostName`). When `ezbx init` writes `ezbx.toml`, it writes plain `harnesses`, `tools`, `network` and `allow` fields — never a `profile` key — so every allowed domain and tool stays visible in `ezbx.toml` ([threat-model.md](threat-model.md)) and future ezbx updates never silently change an existing box.
- **Defaults you can override.** On a fresh setup, `offline` is the default (`1`), so `ezbx init --yes` stays completely offline. Any flag (`--harness`, `--tools`, `--network`) overrides the profile's default, and overriding `--network` to `off` or `on` clears the profile's `allow` list.
- **Re-running `ezbx init`.** When `ezbx.toml` already exists, `ezbx init` skips the starter profile question so existing settings aren't overwritten. Pass `--profile NAME` explicitly to apply a profile to an existing config and review the diff before saving.
