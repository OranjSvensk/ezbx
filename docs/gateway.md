# Gateway design (draft 0.1)

The gateway is the only door between the box and everything else. This page describes what it does, how it is controlled, and how we will test it. Items marked **verify** depend on engine behavior I still need to confirm.

## 1. Where it sits

```text
 box (inside network) ──► gateway ──► engine on the host   (model route)
                              └─────► the internet           (egress proxy, by network mode)

 host CLI ──docker exec──► gateway admin API (the gateway container's own loopback)
```

The gateway runs as its own container, as an unprivileged user, with all capabilities dropped and a read-only root filesystem. It mounts only a read-only config file and a writable state folder for logs.

## 2. Listeners

| Listener | Reachable from | Purpose |
|---|---|---|
| **Model** (`:8000`) | the box | OpenAI-compatible API, forwarded to the selected engine. |
| **Proxy** (`:3128`) | the box | HTTP forward proxy for internet access, governed by the network mode. |
| **Admin** (`127.0.0.1:PORT` inside the gateway container) | the host, through `docker exec` | Control and status for the CLI. |

The box shares a network with the gateway container, so it can reach any port the gateway listens on there, whatever is published to the host. Publishing the admin port to the host loopback would therefore not keep the box out.

Decided: the admin listener binds only to the loopback address **inside the gateway container**, and no admin port is published at all. Each container has its own network namespace, so the gateway's loopback can't be reached from the box or the host network. The CLI reaches it by running `docker exec ezbx-<box>-gateway ezbx gateway admin <call>`, which starts the `ezbx` binary inside the gateway container (no shell needed) and talks to that loopback. A token from the state folder is still required as a second layer. The Docker healthcheck uses the same pattern.

Options considered and set aside: binding to the gateway's outside-network address only (needs the gateway to find its own address, depends on how Docker publishes ports for a container on two networks, and relies on the box being unable to add routes), and a separate admin container joined to the gateway by a shared socket (more parts for the same result). If `docker exec` turns out too slow, the outside-address option is the fallback.

The code refuses an admin address that isn't a loopback address, so a typo can't put it on the box's network. In the Docker test, a full port scan of the gateway from the box found only the model and proxy ports, and the proxy refuses the gateway's own loopback addresses (see [isolation-test.md](isolation-test.md)). Ports are set with flags (`--listen`, `--proxy`, `--admin`).

## 3. The model route

### Forwarding

- Paths are passed straight through: `/v1/models`, `/v1/chat/completions`, `/v1/embeddings` and any other `/v1/...` path. Against an OpenAI-compatible server, harnesses that speak OpenAI's Responses API (`POST /v1/responses`, used by Codex CLI) or Anthropic's Messages API (`POST /v1/messages` and `/v1/messages/count_tokens`, used by Claude Code) have their requests and replies (including streaming SSE and tool calls) translated to and from `/v1/chat/completions`. How a reply ended is kept: an engine's `finish_reason: length` (the reply hit the token limit) becomes `stop_reason: max_tokens` for Messages, and `status: incomplete` with `incomplete_details.reason: max_output_tokens` for Responses (`content_filter` becomes `incomplete` too), ending a stream with `response.incomplete` instead of `response.completed`.
- Against an Anthropic-compatible server (`anthropic`), `POST /v1/messages` passes straight through, `POST /v1/messages/count_tokens` passes through and falls back to the local estimate if the server answers 404, and `POST /v1/chat/completions` and `POST /v1/responses` translate to `/v1/messages` and back (plain and streamed SSE, tools included, defaulting `max_tokens` to 4096 when omitted; `stop_reason: max_tokens` and `refusal` map to `length`/`content_filter` and `incomplete`). The gateway doesn't log request or reply content.
- Paths that an engine might resolve to somewhere outside `/v1/` are refused with 400: `.` or `..` segments (also when encoded, such as `%2e%2e`), `//`, backslashes and control characters. An encoded slash inside a name, such as `/v1/models/org%2Fmodel`, is allowed.
- Only `GET`, `HEAD` and `POST` are forwarded (others get 405), and protocol upgrades such as WebSocket are refused.
- The upstream address is fixed by configuration, so the route can't be turned into a general-purpose relay.
- Request size is capped (32 MiB by default, enough for several images). All bodies held at once share one memory budget (256 MiB by default). A request that would go over it gets a 503 "busy" reply with `Retry-After`, rather than waiting. A client has 2 minutes to send its body.

### Streaming

Responses that stream (server-sent events) are forwarded as they arrive, with no buffering. If the box closes the connection, the gateway cancels the upstream request so the engine stops generating and frees the GPU. There is no overall timeout on a stream. There are two generous limits instead:

- **Waiting for the reply to start:** 30 minutes by default, then 504. A reply that is not streamed only starts once the whole answer is ready, so this has to be long.
- **Idle:** if the engine sends nothing for 10 minutes in the middle of a reply, the gateway ends it and cancels the engine request.

### Authentication

- **Box to gateway:** a placeholder key. It is not a real credential and exists because some harnesses insist on one.
- **Gateway to engine:** if the engine needs a key, the gateway adds it (`Authorization: Bearer` for OpenAI-compatible servers, `x-api-key` for `anthropic`; `anthropic-version: 2023-06-01` is also sent on Anthropic requests if unset). You name an environment variable holding it with `key_env` in `ezbx.toml` (environment variables only in 0.1, decided in `init-flow.md`). The key is never written into an image, a config the box can read, or a log.
- **How the key reaches the gateway** (design decision 18). `ezbx up` reads the variable and writes the key to `engine-key` in the box's state folder, readable only by you, and mounts that one file read-only into the gateway at `/run/ezbx/engine-key` (never into the box). The gateway reads it once at start with `--key-file`. The gateway's environment and command line hold no key, so `docker inspect`, which people tend to paste into bug reports, doesn't show it; an earlier version passed it as an environment variable, which did. `ezbx down` removes the file with the containers, so the key is on disk only while the box exists, and a restart of Docker Desktop still finds it. A key that is empty, over 4 KB, or holds a line break (one trailing line break is dropped) stops `up` before anything is made, and the gateway refuses such a file too. `ezbx gateway serve --key-env NAME` still works for running the gateway by hand. `ezbx engine use` still refuses a server with a key: set it under `[engine]` in `ezbx.toml`, then `ezbx down` and `ezbx up`. `ezbx doctor` warns if the gateway has a key in its environment (a box started by an older version).

### Model list

`GET /v1/models` returns what the engine reports, as is. The CLI reads the same list to generate harness configs (see `harness-manifest.md`).

**The `default` alias** (design decision 26). With `default_model` under `[engine]` in `ezbx.toml` (`--default-model` on `ezbx gateway serve`), a request for the model `default` is sent to the engine with that model's name instead, on every route, translated or not; every other field of the body is kept. The log and `X-Ezbx-Model` name the real model. Without it, `default` reaches the engine as it is. The name follows the header rule: up to 200 printable ASCII characters, and not `default` itself. `ezbx engine use` sends the setting again with the new upstream, and a new address drops it, since another server may not have that model.

### Response headers

Every reply that comes from the engine carries:

- `X-Ezbx-Engine`: the server's API kind (for example `openai` or `anthropic`).
- `X-Ezbx-Request-Id`: the id of the matching log line.
- `X-Ezbx-Model`: the model name from the request, only when the request named one.

Any `X-Ezbx-*` header the engine sends itself is removed first, so only the gateway can set them. Errors the gateway writes itself (400, 405, 413, 502, 503, 504) don't carry these headers.

These let `ezbx doctor` prove which engine answered (open question 4 in `design.md`).

## 4. Switching servers

1. `ezbx engine use` (with `--url ADDRESS` or `--api KIND` to change them) checks the server again and calls the admin API. The change lasts until the next `ezbx up`, which uses `ezbx.toml`.
2. The gateway swaps its upstream. Requests already in flight finish on the old server, and new requests go to the new one.
3. The command rewrites the harness configs in the box from the server's model list.

Reaching the host: on Docker Desktop the gateway uses the host alias, and this works for a server bound to the host's loopback address (tested). Other setups come later (decision 4).

**The Host header.** Some servers protect themselves against DNS rebinding by refusing any request whose `Host` header isn't a local name. One tested server did, and refused `host.docker.internal:PORT` with 403. When the upstream is Docker Desktop's host alias (`host.docker.internal` or `gateway.docker.internal`), the gateway therefore sends `Host: localhost:PORT`. It still connects through the alias; only the name it presents changes. That is accurate, since the engine really is on the host's loopback. `--upstream-host` sets the header by hand for other setups. It takes only a host name or address with an optional port.

## 5. The egress proxy

The proxy is how the box reaches the internet, if at all.

| Mode | Behavior |
|---|---|
| `airgap` | Every proxy request is refused, and so is every model request (403, `network_airgap`). Switching to it ends model requests in flight, streams included, and cancels them at the engine; they count with the closed tunnels. `/healthz` still answers. |
| `off` | Every proxy request is refused. Only the model route works. |
| `allow` | Requests are allowed only to hosts on the allowlist. |
| `on` | Requests to the public internet are allowed. |

Rules for every mode:

- **Hostnames, not content.** HTTPS goes through a plain tunnel and is never decrypted, so filtering is by domain. There is no path-level filtering.
- **Local networks stay blocked**, even in `on`. The proxy refuses loopback, private, link-local (which includes the `169.254.169.254` cloud metadata address), carrier-grade NAT, multicast, documentation and reserved ranges, and the IPv6 forms that can carry an IPv4 address (`::ffff:`, NAT64, 6to4). It also refuses `localhost` and Docker Desktop's `*.docker.internal` names by name. Without this, the box could reach other services on your PC or LAN, or the gateway's own admin API, through the gateway. There is no setting to allow a local address yet.
- **Resolve once, then connect.** The gateway resolves the name itself. If **any** address it gets back is local, the request is refused. Otherwise it connects to the checked address itself, not the name, so a second lookup can't return something else.
- **Ports 80 and 443 only.** Other ports are refused.
- **HTTPS and plain HTTP.** HTTPS uses `CONNECT`. A plain `http://` request is forwarded under the same rules. Protocol upgrades are refused.
- **A limit on open connections** (256 by default). Past it, the reply is 503 with `Retry-After`.
- **Blocked requests are logged** with the host and the reason, so `ezbx net allow <domain>` is easy to use. The 403 reply says what to run on the host.
- **Tools that ignore proxy settings** simply fail, because the box has no other route. That is the desired result.

**The allowlist** holds host names, never addresses. Besides `--allow`, `--allow-file FILE` adds the hosts in a file, one per line (`#` starts a comment), read when the gateway starts; a missing or broken file stops the gateway rather than starting it with a shorter list. `ezbx up` uses it for the hosts approved with `ezbx net allow`. `example.com` matches only that name. `*.example.com` matches names under it, such as `api.example.com`, but not `example.com` itself, so each is approved on purpose. A wildcard over a top-level domain (`*.com`) is refused.

The mode is changed through the admin API and takes effect for new connections at once. Open tunnels the new rules don't allow are closed, and their log line says `"closed": "network_tightened"`.

## 6. Logging

One JSON object per line, written to the per-user state folder (decided in `init-flow.md`). `ezbx gateway serve` writes to standard output or to the file given with `--log`. A gateway started by `ezbx up` writes to `logs/gateway.log` in the box's state folder, unless `log = false` under `[gateway]`.

| Field | Meaning |
|---|---|
| `time`, `id` | Timestamp and request id. |
| `route` | `model`, `proxy` or `admin`. |
| `method`, `path` or `host` | What was requested. Proxy lines have `host` (with the port). |
| `engine`, `model` | Model route: the engine, and the model name from the request when present. |
| `status` | The status code sent back. For a tunnel, 200 means it opened. |
| `blocked`, `reason` | Proxy: `blocked` is true when the network rules refused the request, and `reason` says which rule (`network_off`, `not_on_allowlist`, `local_address`, `port_not_allowed`). A request that failed for another reason has a `reason` (`lookup_failed`, `connect_failed`, `bad_host`, `proxy_busy`) but isn't `blocked`. |
| `closed` | `network_tightened` when a mode change ended the tunnel, or (in `airgap`) the model request. |
| `doctor` | Proxy: true for `ezbx doctor`'s own probes, which carry `X-Ezbx-Doctor: 1`. `ezbx net` leaves them out of its list of blocked hosts; the proxy applies its rules to them as to anything else, and drops the header from what it forwards. |
| `ms`, `ttfb_ms` | Total time and, on the model route, time to the first byte. For a tunnel, `ms` is how long it stayed open. |
| `end` | Only when a reply was cut short: `client_gone` (the box left; the status is then 499) or `engine_idle` (the engine went quiet). |
| `tokens_in`, `tokens_out` | Model route: the token counts the reply reported, read from what the gateway sent the box (a JSON body up to 4 MiB, or a stream's `data:` lines up to 1 MiB each), in OpenAI's or Anthropic's shape. Left out when the reply reported none. |

Built today: everything above. When the gateway translates a stream from an OpenAI-compatible server, it asks for the counts with `stream_options.include_usage` and asks again without it if the server answers 400; the real numbers then reach the harness too (they used to be placeholders). A plain stream passed through is left as the harness asked for it, so its counts are logged only when the harness asked for usage. A proxy line is written when a tunnel ends, with `time` set to when it started. The admin token never appears in the log.

Prompts and responses are **not** logged. A switch to log them, and rotation by size, are planned.

## 7. Health and status

| Endpoint | Meaning |
|---|---|
| `/healthz` | The gateway process is alive. |
| admin `/admin/ready` | The upstream engine answered and listed at least one model. Each call asks the engine, so it is on the admin API only, and the box's side has just `/healthz`. When it isn't ready, the reason says what happened: not reachable, the status code the engine answered with, a list that isn't JSON, or no models. |
| admin `/admin/status` | Engine, upstream address, network mode and allowlist, proxy counters (allowed, blocked, failed, open) and uptime. |

The gateway image has no shell or curl, so its Docker healthcheck runs `ezbx gateway health`, which exits 0 when `/healthz` answers.

### What `ezbx doctor` checks

`ezbx doctor [--config FILE] [--box NAME] [--json]` (built). Every failure or warning comes with what to do about it, and the command exits 1 if anything failed.

On the host:

1. **Docker** answers, and its engine is version 28 or later (needed for the isolated network option).
2. **An engine** is running (detection).
3. **The inside network** is internal, its bridge has no address (`gateway_mode_ipv4=isolated`) and IPv6 is off. **The outside network** exists and isn't internal.
4. **The gateway container** is running and healthy; has no capabilities, `no-new-privileges`, a non-root user and a read-only root filesystem; is attached to exactly the two networks, as `gateway` on the inside one; and publishes no port beyond this computer's loopback, and never the admin port.
5. **Through the admin API** (`docker exec`): it answers, the engine is ready, and the gateway forwards to the port the running engine is on.
5a. **Images are current** (a warning, not a failure, since an older image still works): the gateway and the box each run the image that the current `ezbx` files and the box's `ezbx.toml` call for (the same names `ezbx up` works out), that image carries the matching `ezbx.inputs` label, and it is still the build with that name (`ezbx reset --rebuild` builds a new one under the same name). The fix is `ezbx build`, then `ezbx down` and `ezbx up`, which keeps the workspace, the agent's home and snapshots. Without a config for the box, the check is skipped and says why.
6. **The box container**, if there is one: no capabilities, `no-new-privileges`, not root, attached only to the inside network, and no Docker socket mounted. While it runs, the agent can write to `/workspace` (asked through `docker exec`, as the agent). If Docker made the workspace folder itself, for example after it was deleted while the box existed, the folder is owned by root; the fix is `ezbx admin shell -- chown 1000:1000 /workspace`. `ezbx up` checks this too and warns, since the box starts fine but the agent can't change anything.

From inside, in a throwaway container started from the gateway's own image (which holds `ezbx`), attached only to the inside network, with the box's restrictions:

7. The model list comes back through the gateway and names the expected engine. A one-token completion succeeds and names the model. The completion may take up to 2 minutes, and taking longer is a warning rather than a failure: the model list already proved the route, and a model that was just loaded, or that shares the GPU with a game, can be slow to answer the first time (seen live, where the 30-second limit it had before failed a freshly loaded model).
8. A full port scan of the gateway finds only the model and proxy ports.
9. If the inside network's bridge has an address, a full port scan of it finds nothing.
10. No direct route to public addresses, the gateway's outside address, the outside network's bridge, Docker Desktop's host address, or this computer's own addresses. Outside names and Docker Desktop's host names don't resolve.
11. The proxy refuses local addresses (the host alias, loopback including the admin port, cloud metadata, this computer's addresses), and applies the network mode (`off` refuses a public site; `on` reaches one).

This computer's own addresses are only passed to the check container. They are never written anywhere. The results of running it are in [isolation-test.md](isolation-test.md).

## 8. Failures

Errors use the OpenAI-style error body so harnesses display them sensibly.

| Situation | Response |
|---|---|
| Engine not reachable | 502, with a message telling you to run `ezbx doctor`. |
| Engine didn't start answering in time | 504. |
| Too many large requests held at once | 503 with `Retry-After`. |
| Body too large | 413. |
| Path not allowed, or a protocol upgrade | 400. |
| Method other than GET, HEAD or POST | 405. |
| Engine still loading a model | The engine's own error, passed through. |
| Proxy blocked | 403 with the reason and what to run in the body, and a log line. |
| Proxy can't resolve or reach the host | 502. |
| Too many open proxy connections | 503 with `Retry-After`. |

## 9. Admin API (first set)

Every call needs `Authorization: Bearer <token>`. The token is at least 32 characters, read from a file (`--admin-token-file`, by default `/run/ezbx/admin-token`, mounted read-only from the host state folder). Request bodies are small JSON objects, and unknown fields are refused, so a typo is an error rather than ignored.

| Call | Body | Purpose |
|---|---|---|
| `GET /admin/status` | | Current state (section 7). |
| `GET /admin/ready` | | Whether the engine answers with at least one model. |
| `PUT /admin/engine` | `{"upstream": "http://…", "engine": "id", "default_model": "name"}` | Switch the upstream engine. `default_model` sets the `default` alias; left out, the alias is off, since it belonged to the old server. |
| `PUT /admin/network` | `{"mode": "allow", "allow": ["registry.npmjs.org"]}` | Set the mode and replace the allowlist. The reply says how many tunnels were closed. |
| `POST /admin/network/allow` | `{"domain": "github.com"}` | Add one entry and keep the mode. |
| `POST /admin/engine/unload` | | Planned: ask the engine to unload its model, if the adapter supports it ("gaming mode"). |

From the host, each call is one `docker exec`:

```text
docker exec ezbx-<box>-gateway /ezbx gateway admin status
docker exec ezbx-<box>-gateway /ezbx gateway admin network allow registry.npmjs.org
docker exec ezbx-<box>-gateway /ezbx gateway admin allow github.com
docker exec ezbx-<box>-gateway /ezbx gateway admin engine http://host.docker.internal:PORT openai [DEFAULT_MODEL]
```

`ezbx net` and `ezbx engine use` will wrap these.

## 10. Implementation notes

- Go standard library: `net/http` with `httputil.ReverseProxy` (flush immediately for streams) for the model route, and a small CONNECT handler for the proxy.
- One binary shared with the CLI.
- Image: minimal, no shell, runs as a non-root user.

## 11. Test plan

- A fake engine server that serves normal responses, streams and errors, and can drop connections.
- Tests for streaming without buffering, upstream cancellation on client disconnect, key injection, header stamping and the error bodies.
- Table tests for the allowlist and the blocked ranges, including hostnames that resolve to private addresses.
- An end-to-end run with Docker: the box can reach the model route and nothing else when the mode is `off`.

## 11a. Implementation status

Built and tested (`internal/gateway`, run with `ezbx gateway serve`):

- Model route for `/v1/...` only, to one fixed upstream, with address checks.
- Path checks (no `.`/`..` segments, `//`, backslashes or control characters), only `GET`/`HEAD`/`POST`, and no protocol upgrades.
- Streaming without buffering, and cancellation of the engine request when the client leaves.
- Limits for waiting on the engine to start a reply and for an engine that goes quiet mid-reply.
- Key injection (the box's placeholder is dropped), and no `X-Forwarded-For` or `Forwarded`.
- `X-Ezbx-Engine` and `X-Ezbx-Request-Id` on every reply from the engine, `X-Ezbx-Model` when the request named a model, and engine-set `X-Ezbx-*` headers removed.
- `/healthz` on the model route and readiness on the admin API, OpenAI-style errors, a request size limit, a shared memory budget for request bodies, and JSON logging with no prompts, replies or keys (499 and `end` when a reply is cut short).
- Switching the upstream at run time, through the admin API.
- The egress proxy (`--proxy`) with the `off`, `allow` and `on` modes, the allowlist, local-address blocking after one lookup, the port limit, plain HTTP, the connection limit, closing tunnels when the mode tightens, and logging of every request with its reason.
- The admin API (`--admin`, loopback only, token required) and `ezbx gateway admin` to call it. `ezbx gateway health` for the Docker healthcheck.

Each proxy protection was mutation-checked: turning it off makes a test fail.

Checked against a real local model server, with the hardened gateway in its container: readiness, the model list, a chat reply, a streamed reply that arrived chunk by chunk, a stream cancelled by the box, the path checks, and Pi using tools through it (see [isolation-test.md](isolation-test.md)).

Not built yet: the engine unload call, a setting to allow a chosen local address, and the cloud provider routes (design decision 8). Detection sends the key only to the configured `url` (design decision 19), so a server that also wants its key for the model list works too.

Docker: the gateway image ([docker/gateway.Dockerfile](../docker/gateway.Dockerfile)) was run locked down with the proxy and admin API in the isolation test ([isolation-test.md](isolation-test.md)). `ezbx` doesn't create or start containers yet; that was done by hand.

## 12. Milestones

- **0.1 (done):** model route, streaming, key injection, response headers, `off` and `on` modes, logging, health, admin API.
- **0.2 (done):** `allow` mode, blocked-request workflow, `doctor` end-to-end checks.
- **0.4:** the `airgap` mode, the `default` alias, token counts in the log.
- **Later:** usage metering, several engines at once.

## 13. Open questions

1. ~~How each engine reports token usage in streamed responses.~~ The gateway asks for it (decision 25). Still to check live: which servers accept `stream_options`.
2. Whether the gateway should enforce one request at a time per engine, or leave queuing to the engine.
3. ~~Default location of the state folder and logs.~~ Decided in `init-flow.md`: a per-user state folder, never inside the workspace and never mounted into the box.
