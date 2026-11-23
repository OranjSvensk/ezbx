# Engine adapters (schema 1, draft)

An engine is the program that runs the model on your machine. ezbx supports several, and each has a small adapter in `providers/<id>/adapter.json`. Adapters are plain data, so adding an engine doesn't need a code change unless it behaves in a new way.

All engines are expected to offer an OpenAI-style HTTP API. That is what lets the gateway treat them alike.

## What an adapter says

| Section | Meaning |
|---|---|
| `detect` | How to find the engine: hint ports, an optional command that lists running servers, and response probes. |
| `openai` | Paths for the model list and chat. |
| `models` | Where to read each model fact from the engine's replies (id, context size, image input, loaded state). A `null` means the engine doesn't report it, so a configured default is used. |
| `auth` | Whether the engine wants a key. The key's value is never stored here. |
| `capabilities` | What the engine can do, as far as we know. |
| `control` | Optional commands or calls to start, stop, check or unload. `null` means ezbx detects the engine but doesn't control it. |
| `verified` | What has actually been checked, and when. |

## Detection

Ports are hints, never proof. Different versions and user setups use different ports; llama.cpp's default, for one, moved from 8080 to 9931. Detection therefore works like this:

1. If an adapter has a `cli` status command and that program is installed, ask it for running servers. This finds engines on custom ports.
2. Probe each adapter's hint ports on the loopback address only, with a very short timeout. Probes are plain GET requests with no data about you, and no key (see Keys below for the one exception).
3. Fingerprint each answer using the adapter's `probes`. An engine counts as detected only if every required probe answers with what it expects (see below).
4. When more than one adapter matches the same server (some engines share routes), the more specific one wins: the one whose probes checked the most fields. Strata and llama.cpp both expose a health route, for example, and Strata's includes extra fields.
5. Report what was found. If several engines are running, `ezbx init` asks which to use. An address that answered 401 or 403 and matched no adapter is reported as an engine that asks for a key, not as nothing found.

A server that offers an OpenAI-style model list but matches no specific adapter is reported as an **OpenAI-compatible server** (the `openai` adapter). It works with the gateway, but ezbx doesn't claim to know which engine it is. That adapter adds no ports of its own, and any specific adapter that matches wins over it.

If detection fails, you can give the address yourself in `ezbx.toml`, as `url` under `[engine.<id>]`. When `engine.use` names that engine, `ezbx up` checks only that address and leaves other engines on the computer alone.

### Probes

Each probe is a GET to one path. Its `expect` says what the JSON reply must contain:

- `hasFields`: paths that must lead to real (non-null) values. A path with `[]`, such as `data[].id`, needs a non-empty list.
- `hasLists`: plain paths, such as `data`, that must be lists. An empty list counts, so an engine with no models loaded is still found.

A probe must expect at least one thing. `"optional": true` makes a probe add to the score when it matches without being needed. Two probes may use the same path; it is fetched once.

### Model facts in the adapter

`models.id`, `contextWindow` and `input` point into the model list, for example `data[].meta.n_ctx`. `models.loaded` has one of two forms:

| Form | Meaning |
|---|---|
| `data[].status.value == loaded` | A field in the model list equals a value. |
| `/api/ps models[].name` | The model is loaded if another route lists its id there. If that route doesn't answer, loaded stays unknown. |

Two optional settings go with them:

- `models.list` reads the facts from another route than the OpenAI model list (`openai.modelsPath`), such as an engine's own, richer list. Its ids must be the ones the chat route accepts.
- `models.skip` leaves out models that can't chat, in the form `models[].type == embedding`: a field in the list equals a value.

Anything else is refused when adapters load, so a fact is never silently ignored.

Model ids are written into the harness config inside the box, so `ezbx engine detect`, the `ezbx init` review and `ezbx up` warn about a model named by a file path on the host (a model file's extension such as `.gguf`, a drive letter, or a leading slash), as llama.cpp's server does unless it is started with `--alias NAME`. Names such as `org/model` are not paths.

## Model facts

The gateway and CLI read the engine's model list and fill in what the engine reports.

- **Strata** reports context size and image input in its model list, and a loaded state.
- **Ollama** reports which models are loaded through `/api/ps`.
- **LM Studio**'s facts come from its own list (`models.list` is `/api/v1/models`), which gives each model's context size (`max_context_length`) and says which are embedding models; those are left out (`models.skip`), since its OpenAI list names every downloaded model alike. Its `loaded_instances` and `capabilities.vision` are not read yet.
- **Ollama and llama.cpp** report little else in the standard list. ezbx uses configured defaults (and can later ask each engine's native API) and lets you override them in `ezbx.local.toml`.

These facts feed the harness config generator (see `harness-manifest.md`).

## Keys

If an engine requires a key, you name an environment variable in your local config (`key_env`). `ezbx up` copies the key into a file that only the gateway mounts, read-only, and the gateway adds it to requests. The key never appears in a repo file, an image, a log or `docker inspect` (see [gateway.md](gateway.md), section 3). Because some engines also want the key for their model list, detection sends it, but only to the exact `url` set for that engine under `[engine.<id>]` (scheme, host, port and path; a trailing slash doesn't matter). It never goes to the hint ports, to addresses an engine's status command lists, or to another engine's `url`, and redirects are never followed. That is why `key_env` needs `url`: a config with `key_env` and no `url` is refused, and `ezbx init` writes the address where it found the engine. `ezbx up` checks that the variable has a value before it contacts anything. If the engine answers 401 or 403, `up` says the engine refused the key (naming the variable, never the key), or, when no key was set, that it asks for one and how to give it. `ezbx doctor` sends no key; it shows such an engine as running and leaves the real check to the gateway's `engine ready`, which uses the key (design decision 19).

## Reaching the engine

Engines normally listen on the loopback address only, which is the safe default. The gateway reaches them through the host alias on Docker Desktop, and presents itself as `localhost`, because engines with DNS rebinding protection (Strata, for one) refuse other host names (see [gateway.md](gateway.md), section 4). Binding an engine to all interfaces just to make this work isn't recommended, because some engines have no authentication (LM Studio's docs warn about this). Setups other than Docker Desktop are covered by design decision 4.

## Status of each adapter

| Engine | Status | Notes |
|---|---|---|
| Strata | **probed-live** (2026-11-20) | Detection fields and control commands were read from a running install. Context size and image input come from its model list. |
| LM Studio | **verified** (2026-11-22) | Recognised by its native `GET /api/v1/models`; port 1234, and `lms server start`, `lms unload --all` and `lms server stop` confirmed. One with its API token on shows up as a generic OpenAI-compatible server. Model facts come from its own list, which leaves out embedding models and gives the context size. |
| Ollama | **verified** (2026-11-22) | Port 11434, `/api/ps` for the loaded state, and the `keep_alive: 0` unload confirmed on 0.40.17. Found even with no models. |
| llama.cpp | **verified** (2026-11-22) | Build 12648 listens on 9931 by default; 8080 stays a hint for older builds. Detection only. Recognised by `/health` with a `status` field, which is a weak sign: another server with such a route would be labelled llama.cpp. Start it with `--alias NAME`, or the model is named by its file path (ezbx warns). |
| OpenAI-compatible | generic | The fallback for any server with an OpenAI-style model list. |

Do not change a status to `verified` until the adapter has been exercised against a real install and the date recorded. The three engines above were checked through the gateway run on the host, not yet from a box ([isolation-test.md](isolation-test.md)).

## Sources

- [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility) (official)
- [LM Studio local server](https://lmstudio.ai/docs/local-server) (official)
- [LM Studio REST API: list models](https://lmstudio.ai/docs/developer/rest/list) and [REST API v0](https://lmstudio.ai/docs/developer/rest/endpoints) (official)
- [llama.cpp server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md) (official, but not read in full here)
- Third-party guides were used for Ollama's `/api/ps` and `keep_alive`, and for the llama.cpp port disagreement. They are flagged in the adapters.

## Open questions

1. Whether to read richer model facts from each engine's native API in version 0.1 or leave them as defaults.
2. How to tell the user clearly when two engines are running and which one the gateway is using.
