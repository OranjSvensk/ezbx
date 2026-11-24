# Model servers and API kinds (schema 1)

ezbx doesn't care which program runs your model. It works with any model server on your computer that speaks one of the APIs it knows, and it finds out which by asking the server, not by recognising the program. In this file the server is also called the engine, the word the commands use (`ezbx engine detect`, `ezbx engine use`).

| API kind | `api =` | What the server must answer |
|---|---|---|
| OpenAI-compatible | `openai` | `GET /v1/models` with a `data` list, and `POST /v1/chat/completions` |
| LM Studio | `lmstudio` | the OpenAI-compatible routes, plus LM Studio's own model list, `GET /api/v1/models` |

The LM Studio kind is the OpenAI-compatible one with better model facts: its own list gives each model's context size and says which models are embedding models, which can't chat and are left out. Chat still goes to `/v1/chat/completions`. A server that answers both is treated as `lmstudio`, because that kind checks more; `api = "openai"` uses it as a plain one.

Harnesses in the box don't see any of this. The gateway gives them one OpenAI-compatible address, and translates the OpenAI Responses and Anthropic Messages APIs to chat completions for harnesses that speak those ([gateway.md](gateway.md)).

## The config

```toml
[engine]
url     = "http://127.0.0.1:1234"   # the server's address
api     = "auto"                    # auto | openai | lmstudio
key_env = "ENGINE_API_KEY"          # only if the server wants a key
host    = "localhost:1234"          # only if the server checks the Host header
```

- `url` is the one address ezbx up, ezbx engine use and ezbx doctor contact. Nothing else on your computer is asked. `ezbx init` writes it from the server you pick.
- `api = "auto"` (the default) uses the most specific kind the server answers. Naming a kind makes `up` refuse a server that doesn't speak it.
- A config from ezbx 0.2 or earlier, which named an engine (`engine.use`, `[engine.<id>]` tables), is refused with a message showing this shape.

## Finding a server

`ezbx init` and `ezbx engine detect` look at a few common local ports (1234, 8080, 9931, 11434) on the loopback address only, plus any address you give with `--url` or that the config already has. These are ports many servers use by default; they are hints, not proof. Each address is checked like this:

1. Send each kind's probes, plain GET requests with a short timeout, no data about you, and no key (see Keys below for the one exception).
2. An address counts as a kind only if every required probe answers with what the kind expects.
3. When more than one kind matches (every `lmstudio` server is also `openai`), the more specific one wins: the one whose probes checked the most fields. The others are listed as `alsoMatched`.
4. An address that answered 401 or 403 and matched no kind is reported as a server that asks for a key, not as nothing found.

If your server isn't on one of those ports, give its address: `ezbx init --url http://127.0.0.1:PORT`.

## What a kind file says

Each kind is a data file, `providers/<id>/adapter.json`, so a new kind needs no code change unless it behaves in a new way.

| Section | Meaning |
|---|---|
| `detect` | `hintPorts` (the common ports above, looked at only by `init` and `engine detect`) and `probes`. |
| `openai` | Paths for the model list and chat. |
| `models` | Where to read each model fact from the server's replies (id, context size, image input, loaded state). A `null` means the kind doesn't report it, so a configured default is used. |
| `auth` | Whether the kind uses a key. The key's value is never stored here. |
| `capabilities` | What the kind can do, as far as we know. |
| `verified` | What has actually been checked, and when. |

Unknown fields are refused when the files load, so a typo is an error. (Schema 1 once had `detect.cli` and `control` for engine programs' own commands; they were removed with the per-engine adapters.)

### Probes

Each probe is a GET to one path. Its `expect` says what the JSON reply must contain:

- `hasFields`: paths that must lead to real (non-null) values. A path with `[]`, such as `data[].id`, needs a non-empty list.
- `hasLists`: plain paths, such as `data`, that must be lists. An empty list counts, so a server with no models loaded is still found.

A probe must expect at least one thing. `"optional": true` makes a probe add to the score when it matches without being needed. Two probes may use the same path; it is fetched once.

### Model facts

`models.id`, `contextWindow` and `input` point into the model list, for example `data[].meta.n_ctx`. `models.loaded` has one of two forms:

| Form | Meaning |
|---|---|
| `data[].status.value == loaded` | A field in the model list equals a value. |
| `/api/ps models[].name` | The model is loaded if another route lists its id there. If that route doesn't answer, loaded stays unknown. |

Two optional settings go with them:

- `models.list` reads the facts from another route than the OpenAI model list (`openai.modelsPath`), such as a server's own, richer list. Its ids must be the ones the chat route accepts.
- `models.skip` leaves out models that can't chat, in the form `models[].type == embedding`: a field in the list equals a value.

The shipped kinds use `list`, `contextWindow` and `skip` (LM Studio) and nothing else; the other forms stay in the schema, tested with a kind made for the tests. Facts a kind doesn't report come from `[models."<id>"]` in `ezbx.toml`.

Model ids are written into the harness config inside the box, so `ezbx engine detect`, the `ezbx init` review and `ezbx up` warn about a model named by a file path on the host (a model file's extension such as `.gguf`, a drive letter, or a leading slash), as some servers do unless the model is given a short name (an alias). Names such as `org/model` are not paths.

## Keys

If the server requires a key, you name an environment variable in the config (`key_env`). `ezbx up` copies the key into a file that only the gateway mounts, read-only, and the gateway adds it to requests. The key never appears in a repo file, an image, a log or `docker inspect` (see [gateway.md](gateway.md), section 3). Because some servers also want the key for their model list, detection sends it, but only to the exact `url` (scheme, host, port and path; a trailing slash doesn't matter). It never goes to the common ports or to any other address, and redirects are never followed. That is why `key_env` needs `url`. When `ezbx init` or `ezbx engine use` moves to a new address, the old `key_env` and `host` are dropped, so a key is never carried to another server. `ezbx up` checks that the variable has a value before it contacts anything. If the server answers 401 or 403, `up` says it refused the key (naming the variable, never the key), or, when no key was set, that it asks for one and how to give it. `ezbx doctor` sends no key; it shows such a server as running and leaves the real check to the gateway's `engine ready`, which uses the key (design decision 19).

## Reaching the server

Servers normally listen on the loopback address only, which is the safe default. The gateway reaches them through the host alias on Docker Desktop, and presents itself as `localhost`, because servers with DNS rebinding protection refuse other host names (see [gateway.md](gateway.md), section 4). Binding a server to all interfaces just to make this work isn't recommended, because many have no authentication. Setups other than Docker Desktop are covered by design decision 4.

## What has been checked

Before the API kinds replaced per-engine adapters (decision 22), four servers were checked live through `ezbx gateway serve` on the host on 2026-11-20 and 2026-11-22: the model list, chat (plain and streamed), `/v1/messages` and `/v1/responses` ([isolation-test.md](isolation-test.md)). One of them, with all seven harnesses, also through a real box. All four answer the `openai` kind's probes; the one with LM Studio's own model list answers the `lmstudio` kind's too.

## Sources

- [LM Studio REST API: list models](https://lmstudio.ai/docs/developer/rest/list) (official)
- [OpenAI API: list models](https://platform.openai.com/docs/api-reference/models/list) (official)
