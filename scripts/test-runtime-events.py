"""Run the Windows Docker runtime acceptance test with ezbx's fake engine."""

import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import tempfile
import threading
import time
from datetime import datetime, timezone
from uuid import uuid4


ROOT = Path(__file__).resolve().parents[1]


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

    def expect(self, event, box):
        line = self.lines.get(timeout=15)
        check(line is not None, "stream closed before the expected event")
        record = json.loads(line)
        check(record["event"] == event and record["box"] == box, f"unexpected event: {record}")
        return record

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
        self.process.wait(timeout=10)
        self.reader.join(timeout=10)
        self.process.stdout.close()
        self.process.stderr.close()


def exercise(folder):
    env = {**os.environ, "EZBX_HOME": str(folder / "home"), "EZBX_CONFIG": ""}
    ezbx, fake = folder / "ezbx.exe", folder / "fakeengine.exe"
    run(["go", "build", "-o", ezbx, "./cmd/ezbx"])
    run(["go", "build", "-o", fake, "./tools/fakeengine"])
    check("events-jsonl" in json.loads(run([ezbx, "version", "--json"], env))["features"], "missing feature")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    engine = subprocess.Popen([fake, "--listen", f"127.0.0.1:{port}"], cwd=ROOT)
    name = f"vt-{uuid4().hex[:8]}"
    other = f"{name}-other"
    stream = None
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
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("fake engine didn't listen")
        since = datetime.now(timezone.utc).isoformat()
        create = ["--tools", "none", "--harness", "pi", "--network", "off", "--memory", "2g"]
        check(result("create", "--workspace", folder / "work", "--url", f"http://127.0.0.1:{port}",
                     "--api", "openai", *create)["ok"], "create failed")
        cli("create", "--box", other, "--workspace", folder / "other", *create)
        config = folder / "home" / "ezbx.toml"
        config.write_text(config.read_text(encoding="utf-8").replace(
            "[engine]\n", '[engine]\ndefault_model = "fake-model"\n', 1
        ), encoding="utf-8")
        cli("events", "--jsonl", expected=2)
        stream = EventStream([ezbx, "events", "--jsonl", "--box", name, "--since", since], env)
        created = stream.expect("box.created", name)

        before = image_tags()
        print("Building the pinned Pi and gateway images", flush=True)
        try:
            cli("build", "--box", name, capture=False)
        finally:
            image_record = folder / "home" / "images.json"
            if image_record.exists():
                images = json.loads(image_record.read_text(encoding="utf-8"))["images"]
                built = {image["tag"] for image in images.values()} - before
        needs_cleanup = True
        started = result("start")
        check(started["ok"] and started["data"]["state"] == "running", "box didn't start")
        check(result("status")["data"]["state"] == "running", "status didn't report running")
        stream.expect("box.started", name)

        isolation = json.loads(run(["docker", "network", "inspect", f"ezbx-{name}-inside"]))[0]
        check(isolation["Internal"] and not isolation["EnableIPv6"] and
              isolation["Options"]["com.docker.network.bridge.gateway_mode_ipv4"] == "isolated",
              "inside network lost its isolation settings")
        check(set(isolation["Containers"]) and
              {item["Name"] for item in isolation["Containers"].values()} ==
              {f"ezbx-{name}-box", f"ezbx-{name}-gateway"}, "unexpected inside network members")

        probe = """
const fs = require('node:fs');
const settings = JSON.parse(fs.readFileSync(process.env.HOME + '/.pi/agent/settings.json'));
(async () => {
  const url = 'http://gateway:8000/v1/';
  const modelList = await (await fetch(url + 'models')).json();
  const reply = await fetch(url + 'chat/completions', {
    method: 'POST', headers: {'content-type': 'application/json'},
    body: JSON.stringify({model: settings.defaultModel, messages: [{role: 'user', content: 'Hi'}]})
  });
  console.log(JSON.stringify({model: settings.defaultModel, ids: modelList.data.map(x => x.id),
    status: reply.status, reply: (await reply.json()).choices[0].message.content}));
})().catch(e => { console.error(e); process.exitCode = 1; });
"""
        command = result("exec", "--", "node", "-e", probe)
        value = json.loads(command["data"]["stdout"])
        check(value == {"model": "default", "ids": ["fake-model", "default"], "status": 200,
                        "reply": "Hello from the fake engine."}, f"unexpected model result: {value}")
        first = stream.expect("exec.started", name)["data"]
        last = stream.expect("exec.exited", name)["data"]
        check(first == {"execId": last["execId"], "program": "node", "argCount": 2} and
              last["exitCode"] == 0 and not last["timedOut"], "incorrect exec metadata")
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
        check(result("stop")["data"]["state"] == "stopped", "box didn't stop")
        stream.expect("box.stopped", name)

        replay = EventStream([ezbx, "events", "--jsonl", "--box", name, "--since", created["ts"]], env)
        try:
            check(replay.expect("box.created", name)["id"] == created["id"], "replay changed event ID")
        finally:
            replay.close()

        check(result("destroy", "--yes", "--remove-config")["ok"], "destroy failed")
        needs_cleanup = False
        destroyed = stream.expect("box.destroyed", name)
        replay = cli("events", "--jsonl", "--box", name, "--since", since, expected=1)
        check([json.loads(line) for line in replay.splitlines()] == [destroyed], "destroy left prior history in replay")
        check(json.loads(cli("status", "--box", other, "--json"))["data"]["state"] == "stopped", "second box changed")
        print("Passed runtime events, replay, destroy, alias, timeout, and airgap checks", flush=True)
    finally:
        try:
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
    if os.name != "nt":
        raise SystemExit("Run this acceptance test on Windows with Docker Desktop.")
    with tempfile.TemporaryDirectory(prefix="ezbx-runtime-") as scratch:
        exercise(Path(scratch))
