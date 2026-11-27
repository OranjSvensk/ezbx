"""Run the Docker runtime acceptance test with ezbx's fake engine."""

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from ipaddress import IPv4Address
import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timezone
from uuid import uuid4


ROOT = Path(__file__).resolve().parents[1]
FAKE_REPLY = "Hello from the fake engine."
CHAT_COMPLETIONS = "/v1/chat/completions"
HARNESS_SMOKES = {
    "pi": (["pi", "-p"], CHAT_COMPLETIONS),
    "opencode": (["opencode", "run"], CHAT_COMPLETIONS),
    "omp": (["omp", "-p"], CHAT_COMPLETIONS),
    "codex": (["codex", "exec", "--skip-git-repo-check"], "/v1/responses"),
    "claude": (["claude", "-p"], "/v1/messages"),
    "hermes": (["hermes", "chat", "-q"], CHAT_COMPLETIONS),
    "openclaw": (["openclaw", "agent", "exec"], CHAT_COMPLETIONS),
}


def run(args, env=None, expected=0, capture=True):
    result = subprocess.run(
        args, cwd=ROOT, env=env, capture_output=capture, text=True,
        encoding="utf-8", timeout=600,
    )
    if result.returncode != expected:
        raise RuntimeError(
            f"{args[:4]}: exit {result.returncode}, expected {expected}\n"
            f"{result.stdout or ''}{result.stderr or ''}"
        )
    return result.stdout


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def image_tags():
    return {
        tag for tag in run(["docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"]).splitlines()
        if tag.startswith(("ezbx-box:", "ezbx-gateway:"))
    }


def engine_address():
    if sys.platform == "win32":
        return "127.0.0.1"
    info = run(["docker", "info", "--format", "{{.OperatingSystem}}"]).strip()
    check("Docker Desktop" not in info, "the Linux test needs native Docker Engine")
    bridge = json.loads(run(["docker", "network", "inspect", "bridge"]))[0]
    addresses = [IPv4Address(item["Gateway"]) for item in bridge["IPAM"]["Config"]
                 if item.get("Gateway") and ":" not in item["Gateway"]]
    check(len(addresses) == 1, "the default Docker bridge must have one IPv4 gateway")
    return str(addresses[0])


def test_workspace(path):
    if sys.platform == "linux" and os.geteuid() != 1000:
        # Share only this fixture with the agent inside the container.
        # The containing temporary folder remains private to the runner.
        path.mkdir(mode=0o700)
        path.chmod(0o707)
    return path


class EventStream:
    def __init__(self, args, env):
        self.process = subprocess.Popen(
            args, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, encoding="utf-8",
        )
        self.lines = queue.Queue()
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()

    def read(self):
        for line in self.process.stdout:
            self.lines.put(line)
        self.lines.put(None)

    def expect(self, event, box, match=None):
        deadline = time.monotonic() + 15
        while True:
            line = self.lines.get(timeout=max(0, deadline - time.monotonic()))
            if line is None:
                code = self.process.wait(timeout=5)
                raise AssertionError(
                    f"event stream exited {code} before {event}: {self.process.stderr.read().strip()}"
                )
            record = json.loads(line)
            matches = record["event"] == event and record["box"] == box
            if match is None:
                check(matches, f"unexpected event: {record}")
                return record
            if matches and match(record["data"]):
                return record

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
        self.process.wait(timeout=10)
        self.reader.join(timeout=10)
        self.process.stdout.close()
        self.process.stderr.close()


def exercise_gateway_consumer(ezbx, env, name, result, stream, activity, request, blocked):
    command = [ezbx, "events", "--jsonl", "--gateway", "--box", name, "--since"]
    seen = {request["id"]}
    reconnect = EventStream([*command, request["ts"]], env)
    try:
        replayed = [
            reconnect.expect("gateway.request", name, lambda data: data["requestId"] == request["data"]["requestId"]),
            reconnect.expect("gateway.blocked_host", name, lambda data: data["requestId"] == blocked["data"]["requestId"]),
        ]
        check(replayed == [request, blocked], "gateway replay did not include the reconnect boundary")
        check([event for event in replayed if event["id"] not in seen] == [blocked],
              "consumer did not deduplicate the reconnect boundary by ID")
    finally:
        reconnect.close()

    def send_requests(count):
        probe = """
(async () => {
  const ids = [];
  for (let i = 0; i < Number(process.argv[1]); i++) {
    const response = await fetch('http://gateway:8000/v1/models');
    if (!response.ok) throw new Error('model list returned ' + response.status);
    await response.json();
    ids.push(response.headers.get('x-ezbx-request-id'));
  }
  console.log(JSON.stringify(ids));
})().catch(error => { console.error(error); process.exitCode = 1; });
"""
        reply = result("exec", "--", "node", "-e", probe, str(count))
        check(reply["data"]["exitCode"] == 0, "model requests failed: " + reply["data"]["stderr"])
        stream.expect("exec.started", name)
        stream.expect("exec.exited", name)
        ids = set(json.loads(reply["data"]["stdout"]))
        check(len(ids) == count and None not in ids, "model responses need distinct request IDs")
        return ids

    # Exceed the journal's 256-record limit without making the live reader fall behind.
    for _ in range(4):
        ids = send_requests(65)
        while ids:
            latest = activity.expect("gateway.request", name, lambda data: data["requestId"] in ids)
            ids.remove(latest["data"]["requestId"])

    gap = subprocess.run(
        [*command, request["ts"]], cwd=ROOT, env=env, capture_output=True, text=True,
        encoding="utf-8", timeout=15,
    )
    check(gap.returncode == 1 and not gap.stdout and "event history is no longer available" in gap.stderr,
          f"expected an explicit gateway retention gap, got {gap.returncode}: {gap.stdout}{gap.stderr}")
    check(result("status")["data"]["state"] == "running", "consumer could not refresh box state after the gap")

    reconnect = EventStream([*command, latest["ts"]], env)
    try:
        reconnect.expect("gateway.request", name, lambda data: data["requestId"] == latest["data"]["requestId"])
        new_ids = send_requests(1)
        return reconnect.expect("gateway.request", name, lambda data: data["requestId"] in new_ids)
    finally:
        reconnect.close()


def exercise_airgap_start(cli, folder, name, address):
    config = folder / "airgap.toml"

    def offline(command, *args, expected=0):
        return cli(command, "--config", config, "--box", name, *args, expected=expected)

    def model_reply():
        probe = ("fetch('http://gateway:8000/v1/models').then(async res => "
                 "console.log(JSON.stringify([res.status, (await res.json()).error.code])))")
        result = json.loads(offline("exec", "--json", "--", "node", "-e", probe))
        check(result["ok"] and result["data"]["exitCode"] == 0, "exec failed in the airgap box")
        return json.loads(result["data"]["stdout"])

    offline("create", "--workspace", test_workspace(folder / "offline"), "--harness", "pi",
            "--tools", "none", "--memory", "2g", "--network", "airgap")
    try:
        settings = config.read_text(encoding="utf-8")
        check("log = true" in settings, "expected default gateway logging")
        config.write_text(settings.replace("log = true", "log = false", 1), encoding="utf-8")
        offline("events", "--jsonl", "--gateway", expected=1)
        state = json.loads(offline("start", "--doctor", "--json"))["data"]
        check(state["state"] == "running" and state["networkMode"] == "airgap",
              "engine-free start did not report a running airgap box")
        check(model_reply() == [403, "network_airgap"], "airgap model route did not refuse the request")
        check(not (folder / "home" / "state" / name / "logs" / "gateway-events.json").exists(),
              "gateway wrote events with logging disabled")
        offline("net", "off")
        check(model_reply() == [503, "engine_unconfigured"], "model route without an engine was not unavailable")
        offline("stop", "--json")

        requests = queue.Queue()

        class Decoy(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.put(self.path)
                payload = b'{"data":[{"id":"fake-model"}]}'
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_POST = do_GET

        with ThreadingHTTPServer((address, 0), Decoy) as server:
            port = server.server_address[1]
            config.write_text(config.read_text(encoding="utf-8").replace(
                "[engine]\n", f"[engine]\nurl = 'http://{address}:{port}'\n", 1
            ), encoding="utf-8")
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                offline("up")
                check(model_reply() == [403, "network_airgap"], "airgap up opened the model route")
                box = f"ezbx-{name}-box"
                run(["docker", "stop", box])
                report = json.loads(offline("doctor", "--json", expected=1))
                checks = {item["name"]: item for item in report["checks"]}
                check(checks[box]["status"] == "fail" and checks[box]["detail"] == "stopped" and
                      checks["inside checks"]["status"] == "skip", "doctor did not diagnose the stopped box")
                offline("down")
                offline("up")
                check(model_reply() == [403, "network_airgap"], "recreated airgap box did not recover")
                check(requests.empty(), "airgap up or its doctor contacted the configured model server")
            finally:
                server.shutdown()
                thread.join(timeout=10)
    finally:
        offline("destroy", "--yes", "--remove-config", "--json")


def exercise(folder, harness):
    linux = sys.platform == "linux"
    env = {**os.environ, "EZBX_HOME": str(folder / "home"), "EZBX_CONFIG": ""}
    suffix = ".exe" if sys.platform == "win32" else ""
    ezbx, fake = folder / ("ezbx" + suffix), folder / ("fakeengine" + suffix)
    run(["go", "build", "-o", ezbx, "./cmd/ezbx"])
    run(["go", "build", "-o", fake, "./tools/fakeengine"])
    version = json.loads(run([ezbx, "version", "--json"], env))
    check(version["ok"] and version["contractVersion"] == 1, "unsupported runtime contract")
    check({"events-jsonl", "gateway-events-jsonl"} <= set(version["features"]), "missing event features")
    address = engine_address()
    with socket.socket() as listener:
        listener.bind((address, 0))
        port = listener.getsockname()[1]
    engine_args = [fake, "--listen", f"{address}:{port}"]
    key_args = []
    if linux:
        key_env = "EZBX_RUNTIME_TEST_KEY"
        env[key_env] = uuid4().hex
        engine_args += ["--key-env", key_env, "--key-on-models"]
        key_args += ["--key-env", key_env]
    engine = subprocess.Popen(engine_args, cwd=ROOT, env=env)
    name = f"vt-{uuid4().hex[:8]}"
    other = f"{name}-other"
    stream = None
    activity = None
    built = set()
    needs_cleanup = False

    def cli(*args, expected=0, capture=True):
        return run([ezbx, *args], env, expected, capture)

    def result(command, *args, expected=0):
        return json.loads(cli(command, "--box", name, "--json", *args, expected=expected))

    try:
        for _ in range(50):
            check(engine.poll() is None, "fake engine exited")
            try:
                with socket.create_connection((address, port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("fake engine didn't listen")
        since = datetime.now(timezone.utc).isoformat()
        create = ["--tools", "none", "--harness", harness, "--network", "off", "--memory", "2g"]
        reference = folder / "reference"
        reference.mkdir()
        (reference / "visible.txt").write_text("reference\n", encoding="utf-8")
        (reference / ".env").write_text("example placeholder\n", encoding="utf-8")
        workspace = test_workspace(folder / "work")
        check(result("create", "--workspace", workspace, "--url", f"http://{address}:{port}",
                     "--api", "openai", "--readonly", reference, *key_args, *create)["ok"], "create failed")
        cli("create", "--box", other, "--workspace", folder / "other", *create)
        config = folder / "home" / "ezbx.toml"
        config.write_text(config.read_text(encoding="utf-8").replace(
            "[engine]\n", '[engine]\ndefault_model = "fake-model"\n', 1
        ), encoding="utf-8")
        cli("events", "--jsonl", expected=2)
        stream = EventStream([ezbx, "events", "--jsonl", "--box", name, "--since", since], env)
        created = stream.expect("box.created", name)
        activity = EventStream([ezbx, "events", "--jsonl", "--gateway", "--box", name, "--since", since], env)

        before = image_tags()
        print(f"Building the pinned {harness} and gateway images", flush=True)
        try:
            cli("build", "--box", name, capture=False)
        finally:
            image_record = folder / "home" / "images.json"
            if image_record.exists():
                images = json.loads(image_record.read_text(encoding="utf-8"))["images"]
                built = {image["tag"] for image in images.values()} - before
        needs_cleanup = True
        try:
            started = result("start")
            check(started["ok"] and started["data"]["state"] == "running", "box didn't start")
        except (RuntimeError, AssertionError):
            gateway = f"ezbx-{name}-gateway"
            if linux:
                print(f"Host account: uid={os.getuid()} gid={os.getgid()}", flush=True)
                state = folder / "home" / "state" / name
                for filename in ("admin-token", "engine-key", "approved-hosts", "logs"):
                    try:
                        details = (state / filename).stat()
                    except FileNotFoundError:
                        continue
                    print(f"{filename}: uid={details.st_uid} gid={details.st_gid} mode={details.st_mode & 0o777:o}",
                          flush=True)
            for args in (
                ["inspect", "--format", "Gateway: user={{.Config.User}} state={{.State.Status}} exit={{.State.ExitCode}}", gateway],
                ["logs", "--tail", "30", gateway],
            ):
                try:
                    subprocess.run(["docker", *args], cwd=ROOT, check=False, timeout=10)
                except (OSError, subprocess.TimeoutExpired) as error:
                    print(f"Could not collect gateway diagnostics: {error}", file=sys.stderr)
            raise
        check(result("status")["data"]["state"] == "running", "status didn't report running")
        stream.expect("box.started", name)

        isolation = json.loads(run(["docker", "network", "inspect", f"ezbx-{name}-inside"]))[0]
        check(isolation["Internal"] and not isolation["EnableIPv6"] and
              isolation["Options"]["com.docker.network.bridge.gateway_mode_ipv4"] == "isolated",
              "inside network lost its isolation settings")
        check(set(isolation["Containers"]) and
              {item["Name"] for item in isolation["Containers"].values()} ==
              {f"ezbx-{name}-box", f"ezbx-{name}-gateway"}, "unexpected inside network members")

        cli("doctor", "--box", name, capture=False)
        probe = """
const fs = require('node:fs');
const home = process.env.HOME;
const read = file => JSON.parse(fs.readFileSync(home + file));
let model = 'default';
if (process.argv[1] === 'pi') model = read('/.pi/agent/settings.json').defaultModel;
if (process.argv[1] === 'opencode') model = read('/.config/opencode/opencode.json').model.replace('ezbx/', '');
if (fs.readFileSync('/readonly/reference/visible.txt', 'utf8').trim() !== 'reference' ||
    fs.readFileSync('/readonly/reference/.env', 'utf8') !== '') {
  throw new Error('unexpected read-only mount contents');
}
fs.writeFileSync('/workspace/created.txt', 'workspace is writable');
(async () => {
  const url = 'http://gateway:8000/v1/';
  const modelList = await (await fetch(url + 'models')).json();
  const reply = await fetch(url + 'chat/completions', {
    method: 'POST', headers: {'content-type': 'application/json'},
    body: JSON.stringify({model, messages: [{role: 'user', content: 'Hi'}]})
  });
  console.log(JSON.stringify({model, ids: modelList.data.map(x => x.id),
    requestId: reply.headers.get('x-ezbx-request-id'), status: reply.status,
    reply: (await reply.json()).choices[0].message.content}));
})().catch(e => { console.error(e); process.exitCode = 1; });
"""
        command = result("exec", "--", "node", "-e", probe, harness)
        value = json.loads(command["data"]["stdout"])
        request_id = value.pop("requestId")
        check(value == {"model": "default", "ids": ["fake-model", "default"], "status": 200,
                        "reply": FAKE_REPLY}, f"unexpected model result: {value}")
        first = stream.expect("exec.started", name)["data"]
        last = stream.expect("exec.exited", name)["data"]
        check(first == {"execId": last["execId"], "program": "node", "argCount": 3} and
              last["exitCode"] == 0 and not last["timedOut"], "incorrect exec metadata")
        request = activity.expect("gateway.request", name, lambda data: data["requestId"] == request_id)
        check(request["data"]["endpoint"] == CHAT_COMPLETIONS and request["data"]["status"] == 200 and
              request["data"]["tokensIn"] == 1 and request["data"]["tokensOut"] == 1,
              "incorrect gateway request metadata")

        refused = result("exec", "--", "node", "-e",
                         "require('node:http').get({host:'gateway',port:3128,path:'http://example.com/'},"
                         "r=>{console.log(r.statusCode);r.resume()})")
        check(refused["data"]["stdout"].strip() == "403", "proxy did not refuse the request")
        stream.expect("exec.started", name)
        stream.expect("exec.exited", name)
        blocked = activity.expect("gateway.blocked_host", name, lambda data: data["host"] == "example.com"
                                  and not data.get("doctor"))
        check(blocked["data"]["status"] == 403 and blocked["data"]["reason"] == "network_off",
              "incorrect blocked host metadata")

        command, endpoint = HARNESS_SMOKES[harness]
        reply = result("exec", "--timeout", "90s", "--", *command, "say hi")["data"]
        check(reply["exitCode"] == 0 and FAKE_REPLY in reply["stdout"],
              f"{harness} did not answer through the gateway: {reply}")
        stream.expect("exec.started", name)
        stream.expect("exec.exited", name)
        activity.expect("gateway.request", name, lambda data: data["endpoint"] == endpoint and data["status"] == 200)
        print(f"Passed {harness} completion through the fake engine via {endpoint}", flush=True)

        if harness == "pi":
            request = exercise_gateway_consumer(ezbx, env, name, result, stream, activity, request, blocked)
            print("Passed gateway consumer reconnect, deduplication, and retention recovery", flush=True)
        timed = result("exec", "--timeout", "200ms", "--", "sleep", "3", expected=1)
        check(timed["error"]["code"] == "timeout", "exec did not time out")
        first = stream.expect("exec.started", name)["data"]
        last = stream.expect("exec.exited", name)["data"]
        check(first["execId"] == last["execId"] and last["timedOut"], "timeout was not correlated")

        cli("net", "--box", name, "airgap")
        check(stream.expect("net.changed", name)["data"] ==
              {"previousMode": "off", "networkMode": "airgap"}, "incorrect airgap event")
        denied = result("exec", "--", "node", "-e",
                        "fetch('http://gateway:8000/v1/models').then(r => console.log(r.status))")
        check(denied["data"]["stdout"].strip() == "403", "airgap did not close the model route")
        stream.expect("exec.started", name)
        stream.expect("exec.exited", name)
        cli("net", "--box", name, "off")
        stream.expect("net.changed", name)
        cli("snapshot", "--box", name, "-m", "a note excluded from events")
        snap = stream.expect("snapshot.created", name)["data"]
        check(set(snap) == {"snapshotId", "kind"} and snap["kind"] == "manual", "incorrect snapshot metadata")
        if harness == "pi":
            exercise_airgap_start(cli, folder, f"{name}-offline", address)
        check(result("stop")["data"]["state"] == "stopped", "box didn't stop")
        stream.expect("box.stopped", name)

        replay = EventStream([ezbx, "events", "--jsonl", "--box", name, "--since", created["ts"]], env)
        try:
            check(replay.expect("box.created", name)["id"] == created["id"], "replay changed event ID")
        finally:
            replay.close()

        gateway_replay = EventStream(
            [ezbx, "events", "--jsonl", "--gateway", "--box", name, "--since", request["ts"]], env
        )
        try:
            check(gateway_replay.expect("gateway.request", name)["id"] == request["id"],
                  "gateway replay changed event ID")
        finally:
            gateway_replay.close()

        check(result("destroy", "--yes", "--remove-config")["ok"], "destroy failed")
        needs_cleanup = False
        destroyed = stream.expect("box.destroyed", name)
        replay = cli("events", "--jsonl", "--box", name, "--since", since, expected=1)
        check([json.loads(line) for line in replay.splitlines()] == [destroyed], "destroy left prior history in replay")
        check(not (folder / "home" / "state" / name / "logs" / "gateway-events.json").exists(),
              "destroy left gateway history on disk")
        check(json.loads(cli("status", "--box", other, "--json"))["data"]["state"] == "stopped", "second box changed")
        print(f"Passed {harness} runtime lifecycle, gateway events, replay, alias routing, and isolation", flush=True)
        if harness == "pi":
            print("Passed engine-free airgap startup with gateway logging disabled", flush=True)
            print("Passed doctor recovery of a stopped airgap box", flush=True)
    finally:
        try:
            if activity is not None:
                activity.close()
            if stream is not None:
                stream.close()
            if needs_cleanup:
                result("stop")
                result("destroy", "--yes", "--remove-config")
            for tag in sorted(built & image_tags()):
                run(["docker", "image", "rm", tag])
        finally:
            engine.terminate()
            engine.wait(timeout=10)


if __name__ == "__main__":
    if sys.platform not in ("win32", "linux"):
        raise SystemExit("Use Docker Desktop on Windows or native Docker Engine on Linux.")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harness", choices=HARNESS_SMOKES, default="pi")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="ezbx-runtime-") as scratch:
        exercise(Path(scratch), args.harness)
