#!/usr/bin/env python3
"""Drive one pinned Pi RPC process. Makez decides whether a prompt may be sent."""

import json
import os
import signal
import subprocess
import sys
import threading
import time

MAX_CHECKPOINT = 1 << 20
IMPORTANT = {"response", "agent_settled"}
# RPC bash runs even when --no-tools is set. Only these commands are written.
ALLOWED_COMMANDS = {"prompt", "get_messages", "clear_queue", "abort"}


def fail(message):
    sys.stderr.write(message + "\n")
    return 1


def load_instruction(path):
    with open(path, "rb") as handle:
        raw = handle.read(1 << 20)
        if handle.read(1):
            raise ValueError("instruction exceeds 1 MiB")
    inst = json.loads(raw)
    required = [
        "binary",
        "prompt",
        "mode",
        "session_dir",
        "checkpoint",
        "disposition_path",
        "history_path",
        "result_path",
        "forward_env",
        "offline",
        "package_version",
        "settle_seconds",
        "restore_checkpoint",
    ]
    if any(key not in inst for key in required):
        raise ValueError("instruction is missing a field")
    if inst["mode"] not in ("send", "reconcile_only", "reconcile_then_send"):
        raise ValueError("instruction mode is not supported")
    release = inst.get("release_path", "")
    if not isinstance(release, str) or (release != "" and not absolute_path(release)):
        raise ValueError("instruction path must be absolute")
    for key in ("binary", "session_dir", "checkpoint", "disposition_path", "history_path", "result_path"):
        if not absolute_path(inst[key]):
            raise ValueError("instruction path must be absolute")
    if not isinstance(inst["forward_env"], list) or not isinstance(inst["offline"], bool):
        raise ValueError("instruction environment is invalid")
    if not isinstance(inst["restore_checkpoint"], bool) or not isinstance(inst["prompt"], str):
        raise ValueError("instruction prompt is invalid")
    if not isinstance(inst["settle_seconds"], int) or not 1 <= inst["settle_seconds"] <= 7200:
        raise ValueError("instruction deadline is invalid")
    if inst["package_version"] == "" or len(inst["package_version"]) > 32:
        raise ValueError("instruction package version is invalid")
    return inst


def absolute_path(value):
    if not isinstance(value, str) or not value.startswith("/") or "\x00" in value or "\n" in value:
        return False
    parts = value.split("/")
    return ".." not in parts


def child_env(inst, include_secret):
    allowed = {}
    for key in ("PATH", "HOME"):
        value = os.environ.get(key)
        if not value:
            raise ValueError("Pi environment is missing " + key)
        allowed[key] = value
    if "LANG" in os.environ:
        allowed["LANG"] = os.environ["LANG"]
    if inst["offline"]:
        allowed["PI_OFFLINE"] = "1"
    if not include_secret:
        return allowed
    for key in inst["forward_env"]:
        if key in ("PATH", "HOME", "LANG", "PI_OFFLINE") or not isinstance(key, str) or key.startswith("MAKEZ_"):
            raise ValueError("forwarded environment name is reserved")
        if key not in os.environ:
            raise ValueError("forwarded environment value is missing")
        allowed[key] = os.environ[key]
    return allowed


def fsync_file(path, payload):
    temporary = path + ".tmp"
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    try:
        os.write(descriptor, payload)
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    os.replace(temporary, path)
    directory = os.open(os.path.dirname(path) or "/", os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def write_result(path, succeeded, summary):
    body = json.dumps(
        {"succeeded": succeeded, "summary": summary[:4000], "artifacts": []},
        separators=(",", ":"),
    ).encode()
    fsync_file(path, body)


def write_disposition(path, disposition, prompt_hash):
    body = json.dumps(
        {"disposition": disposition, "prompt_sha256": prompt_hash},
        separators=(",", ":"),
    ).encode()
    fsync_file(path, body)


def prompt_hash(prompt):
    import hashlib

    return hashlib.sha256(prompt.encode()).hexdigest()


def pi_argv(inst):
    args = [
        inst["binary"],
        "--mode",
        "rpc",
        "--session-dir",
        inst["session_dir"],
        "--no-tools",
        "--no-extensions",
        "--no-mcp",
        "--no-skills",
        "--no-prompt-templates",
        "--no-context-files",
        "--no-themes",
    ]
    if inst["offline"]:
        args.append("--offline")
    checkpoint = inst["checkpoint"]
    if inst["restore_checkpoint"] and os.path.isfile(checkpoint):
        size = os.path.getsize(checkpoint)
        if 0 < size <= MAX_CHECKPOINT:
            args.extend(["--session", checkpoint])
    return args


class Session:
    def __init__(self, proc):
        self.proc = proc
        self.pending = []
        self.lock = threading.Lock()
        self.ready = threading.Condition(self.lock)
        self.dead = False
        self.saw_text = False
        self.next_id = 0
        self.stop = threading.Event()
        self.stdout = sys.stdout.buffer

    def emit(self, record):
        encoded = json.dumps({"kind": "pi", "record": record}, separators=(",", ":")).encode()
        if len(encoded) > 4096:
            encoded = b'{"kind":"pi","truncated":true}'
        self.stdout.write(encoded + b"\n")
        self.stdout.flush()

    def read_stdout(self):
        # os.read returns available bytes. A buffered read(n) waits until n bytes or EOF,
        # which stalls a short RPC response until the process exits.
        stream = self.proc.stdout.fileno()
        buffer = b""
        while True:
            try:
                chunk = os.read(stream, 4096)
            except OSError:
                break
            if not chunk:
                break
            buffer += chunk
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                if line.endswith(b"\r"):
                    line = line[:-1]
                if not line:
                    continue
                try:
                    record = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(record, dict):
                    continue
                self.emit(record)
                self.note(record)
        with self.lock:
            self.dead = True
            self.ready.notify_all()

    def note(self, record):
        if record.get("type") == "message_update":
            event = record.get("assistantMessageEvent") or {}
            if event.get("type") == "text_delta" and event.get("delta"):
                with self.lock:
                    self.saw_text = True
        if record.get("type") == "message_end":
            message = record.get("message") or {}
            if message.get("role") == "assistant" and message.get("content"):
                with self.lock:
                    self.saw_text = True
        with self.lock:
            if record.get("type") in IMPORTANT or len(self.pending) < 1000:
                self.pending.append(record)
            self.ready.notify_all()

    def command(self, body, seconds, interruptible=True):
        if body.get("type") not in ALLOWED_COMMANDS:
            raise RuntimeError("Pi command is not allowed")
        self.next_id += 1
        command_id = "m%d" % self.next_id
        payload = dict(body)
        payload["id"] = command_id
        encoded = json.dumps(payload, separators=(",", ":")).encode() + b"\n"
        self.proc.stdin.write(encoded)
        self.proc.stdin.flush()
        deadline = time.monotonic() + seconds
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            with self.lock:
                for index, record in enumerate(self.pending):
                    if record.get("type") == "response" and record.get("id") == command_id:
                        del self.pending[index]
                        return record
                if self.dead or (interruptible and self.stop.is_set()):
                    return None
                self.ready.wait(timeout=min(0.2, remaining))

    def wait_settled(self, seconds):
        deadline = time.monotonic() + seconds
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return "timeout"
            with self.lock:
                for record in self.pending:
                    if record.get("type") == "agent_settled":
                        return "settled"
                if self.stop.is_set():
                    return "stop"
                if self.dead:
                    return "dead"
                self.ready.wait(timeout=min(0.2, remaining))


def messages_of(response):
    data = response.get("data")
    if isinstance(data, dict) and isinstance(data.get("messages"), list):
        return data["messages"]
    if isinstance(response.get("messages"), list):
        return response["messages"]
    return []


def cancel_run(session, inst):
    # abort continues a queue that clear_queue did not remove.
    cleared = session.command({"type": "clear_queue"}, 5, False)
    if cleared and cleared.get("success"):
        session.command({"type": "abort"}, 5, False)
        write_result(inst["result_path"], False, "Canceled after clear_queue and abort.")
        return 0
    write_result(inst["result_path"], False, "Canceled without abort because the queue was not cleared.")
    return 0


def read_history(session, inst):
    response = session.command({"type": "get_messages"}, min(30, inst["settle_seconds"]))
    if not response or not response.get("success"):
        return False
    fsync_file(
        inst["history_path"],
        json.dumps({"messages": messages_of(response)}, separators=(",", ":")).encode(),
    )
    return True


def wait_for_release(session, inst):
    path = inst.get("release_path") or ""
    if path == "":
        return True
    while not os.path.isfile(path):
        if session.stop.is_set() or session.proc.poll() is not None:
            return False
        time.sleep(0.05)
    return not session.stop.is_set()


def send_prompt(session, inst, digest):
    if session.stop.is_set():
        write_result(inst["result_path"], False, "Canceled before the prompt was sent.")
        return 0
    # The process can be running while the prompt is still unsent. Prompt bytes
    # wait until Makez has recorded that this process started.
    if not wait_for_release(session, inst):
        write_result(inst["result_path"], False, "Canceled before the prompt was sent.")
        return 0
    # Durable "sending" is written and flushed before any prompt byte reaches Pi.
    write_disposition(inst["disposition_path"], "sending", digest)
    if session.stop.is_set():
        # Leave "sending" in place. The prompt bytes were not written, and a
        # later "none" would erase an unknown earlier accept.
        write_result(inst["result_path"], False, "Canceled before the prompt was sent.")
        return 0
    response = session.command({"type": "prompt", "message": inst["prompt"]}, min(30, inst["settle_seconds"]))
    if session.stop.is_set():
        if response is not None and response.get("success"):
            write_disposition(inst["disposition_path"], "accepted", digest)
        return cancel_run(session, inst)
    if response is None:
        write_result(inst["result_path"], False, "Pi did not accept the prompt before the process ended.")
        return 75
    if not response.get("success"):
        write_disposition(inst["disposition_path"], "rejected", digest)
        write_result(inst["result_path"], False, "Pi rejected the prompt before acceptance.")
        return 1
    write_disposition(inst["disposition_path"], "accepted", digest)
    data = response.get("data") if isinstance(response.get("data"), dict) else {}
    if data.get("disposition") == "handled":
        write_disposition(inst["disposition_path"], "settled", digest)
        write_result(inst["result_path"], False, "Pi handled the prompt without starting a run.")
        return 0
    outcome = session.wait_settled(inst["settle_seconds"])
    if outcome == "stop":
        return cancel_run(session, inst)
    if outcome != "settled":
        write_result(inst["result_path"], False, "Pi accepted the prompt and did not settle.")
        return 75
    write_disposition(inst["disposition_path"], "settled", digest)
    with session.lock:
        saw_text = session.saw_text
    if saw_text:
        write_result(
            inst["result_path"],
            True,
            "Pi settled after assistant text. This record does not prove a provider invoice.",
        )
        return 0
    write_result(inst["result_path"], False, "Pi settled without assistant text.")
    return 0


def run(inst):
    # The version process does not receive the task secret.
    try:
        version = subprocess.check_output([inst["binary"], "--version"], env=child_env(inst, False), timeout=15)
    except (OSError, subprocess.SubprocessError):
        write_result(inst["result_path"], False, "Pi version command failed.")
        return 1
    if version.decode(errors="replace").strip() != inst["package_version"]:
        write_result(inst["result_path"], False, "Pi version does not match the registered package.")
        return 1
    os.makedirs(inst["session_dir"], mode=0o700, exist_ok=True)
    proc = subprocess.Popen(
        pi_argv(inst),
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=child_env(inst, True),
    )
    session = Session(proc)
    signal.signal(signal.SIGTERM, lambda _signum, _frame: session.stop.set())
    signal.signal(signal.SIGINT, lambda _signum, _frame: session.stop.set())
    threading.Thread(target=session.read_stdout, daemon=True).start()
    threading.Thread(target=drain, args=(proc.stderr,), daemon=True).start()
    code = 1
    try:
        digest = prompt_hash(inst["prompt"])
        if inst["mode"] == "reconcile_only":
            if not read_history(session, inst):
                write_result(inst["result_path"], False, "Native history could not be read; the prompt was not sent.")
                return 75
            # An empty read is not a restored session file. It still must not resend.
            write_disposition(inst["disposition_path"], "reconciled", digest)
            write_result(inst["result_path"], False, "Read native history and did not send the prompt again.")
            return 0
        if inst["mode"] == "reconcile_then_send" and not read_history(session, inst):
            write_result(inst["result_path"], False, "Native history could not be read; the prompt was not sent.")
            return 75
        code = send_prompt(session, inst, digest)
        return code
    finally:
        if proc.stdin:
            proc.stdin.close()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def drain(stream):
    try:
        while os.read(stream.fileno(), 4096):
            pass
    except OSError:
        return


def main():
    if len(sys.argv) != 2:
        return fail("usage: makez-pi-run.py instruction.json")
    try:
        inst = load_instruction(sys.argv[1])
    except (OSError, ValueError, json.JSONDecodeError):
        return fail("invalid Pi instruction")
    try:
        return run(inst)
    except Exception as exc:  # noqa: BLE001 - the result file is the activity's error surface.
        try:
            write_result(inst["result_path"], False, "Pi runner failed before a durable disposition.")
        except OSError:
            pass
        sys.stderr.write(type(exc).__name__ + "\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
