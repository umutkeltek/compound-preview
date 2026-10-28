"""Bounded stdio bridge to the pinned jev-ultrafast library.

Only content-free step metadata leaves this process. Page text, generated field
values, screenshots, and credentials stay inside the rented browser process.
"""

import base64
import hashlib
import json
import ipaddress
import os
import pathlib
import re
import select
import shutil
import signal
import socket
import socketserver
import subprocess
import sys
import threading
import time
import uuid
from contextlib import contextmanager
from urllib.parse import urlsplit

STAGE = "setup"
MODEL_SECRET_NAMES = ("TYPESAFE_API_KEY", "TEXT_MODEL_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY")


class ScopedProxy(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, host):
        self.allowed_host = host.lower()
        super().__init__(("127.0.0.1", 0), ProxyHandler)


class ProxyHandler(socketserver.StreamRequestHandler):
    def handle(self):
        # Browser HTTPS is opaque inside CONNECT. Only the selected public
        # origin can be reached; link/button actions are separately gated.
        request_line = self.rfile.readline(4096).decode("ascii", "replace").split()
        if len(request_line) != 3 or request_line[0] != "CONNECT":
            self.deny()
            return
        try:
            target = urlsplit("http://" + request_line[1])
            if target.hostname is None or target.hostname.lower() != self.server.allowed_host or target.port != 443:
                self.deny()
                return
        except ValueError:
            self.deny()
            return
        for _ in range(64):
            if self.rfile.readline(4096) in (b"\r\n", b"\n", b""):
                break
        else:
            self.deny()
            return
        remote = None
        try:
            for family, kind, proto, _, address in socket.getaddrinfo(target.hostname, 443, type=socket.SOCK_STREAM):
                if not ipaddress.ip_address(address[0]).is_global:
                    continue
                candidate = socket.socket(family, kind, proto)
                candidate.settimeout(5)
                try:
                    candidate.connect(address)
                    remote = candidate
                    break
                except OSError:
                    candidate.close()
            if remote is None:
                self.deny()
                return
            self.wfile.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            self.wfile.flush()
            self.connection.settimeout(None)
            remote.settimeout(None)
            while True:
                readable, _, _ = select.select((self.connection, remote), (), (), 15)
                if not readable:
                    break
                for source in readable:
                    data = source.recv(65536)
                    if not data:
                        return
                    (remote if source is self.connection else self.connection).sendall(data)
        except (OSError, ValueError):
            return
        finally:
            if remote is not None:
                remote.close()

    def deny(self):
        self.wfile.write(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
        self.wfile.flush()


def receive():
    line = sys.stdin.readline()
    if not line:
        raise EOFError("controller closed")
    value = json.loads(line)
    if not isinstance(value, dict):
        raise ValueError("invalid controller message")
    return value


def send(**value):
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def verify(agent, request):
    page = agent.browser.observe(screenshot=False)
    url = page.get("url", "")
    text = page.get("text", "")
    expected_url = request.get("expect_url", "")
    expected_text = request.get("expect_text", "")
    return (not expected_url or url == expected_url) and (not expected_text or expected_text in text), url


def chrome_binary():
    """Return the Chrome executable for the isolated session, or None.

    This is the one host lookup isolated_chrome makes. It is a separate seam so
    the synthetic handoff tests can substitute a stand-in on any platform; the
    darwin branch is a fixed app path that a PATH lookup cannot reach.
    """
    chrome_path = (
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
        if sys.platform == "darwin"
        else shutil.which("google-chrome") or shutil.which("chromium")
    )
    if not chrome_path or not pathlib.Path(chrome_path).is_file():
        return None
    return chrome_path


@contextmanager
def isolated_chrome(allowed_url):
    """Never let Browser Harness auto-attach to an existing logged-in Chrome."""
    chrome_path = chrome_binary()
    if not chrome_path:
        raise RuntimeError("isolated Chrome unavailable")
    # Browser Harness uses AF_UNIX paths; macOS's default TMPDIR is too deep.
    allowed_host = urlsplit(allowed_url).hostname
    if not allowed_host:
        raise ValueError("missing public host")
    temp = pathlib.Path(os.environ["COMPOUND_JEV_SESSION_DIR"])
    if not temp.is_dir() or not str(temp).startswith("/tmp/cj-"):
        raise RuntimeError("invalid controller-owned browser session")
    with ScopedProxy(allowed_host) as proxy:
        proxy_thread = threading.Thread(target=proxy.serve_forever, daemon=True)
        proxy_thread.start()
        profile = temp / "chrome"
        os.environ["BH_HOME"] = str(temp / "bh")
        os.environ["BH_RUNTIME_DIR"] = str(temp / "runtime")
        os.environ["BH_TMP_DIR"] = str(temp / "tmp")
        name = "compound_jev_" + uuid.uuid4().hex[:12]
        os.environ["BU_NAME"] = name
        chrome = None
        pending_term = False
        original_term = signal.getsignal(signal.SIGTERM)

        def defer_term(_signum, _frame):
            nonlocal pending_term
            pending_term = True

        # Python can deliver SIGTERM after Popen spawns the detached child but
        # before its return value is assigned. Defer only this ownership handoff;
        # a blocked signal mask would be inherited by Chrome.
        signal.signal(signal.SIGTERM, defer_term)
        handler_deferred = True
        try:
            chrome = subprocess.Popen(
                [chrome_path, "--headless=new", "--no-first-run", "--no-default-browser-check",
                 "--disable-background-networking", "--disable-quic", "--remote-debugging-port=0",
                 f"--proxy-server=http://127.0.0.1:{proxy.server_address[1]}",
                 "--proxy-bypass-list=<-loopback>",
                 f"--user-data-dir={profile}", "about:blank"],
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                start_new_session=True,
                env={key: value for key, value in os.environ.items() if key not in MODEL_SECRET_NAMES},
            )
            # The controller may reap after this leader exits. Bind the group to
            # this session's Chrome profile, not just a reusable numeric PID.
            (temp / "chrome.pid").write_text(json.dumps({
                "pid": chrome.pid,
                "pgid": chrome.pid,
                "marker": f"--user-data-dir={profile}",
            }))
            signal.signal(signal.SIGTERM, original_term)
            handler_deferred = False
            if pending_term:
                raise SystemExit(143)
            port_file = profile / "DevToolsActivePort"
            deadline = time.monotonic() + 15
            while not port_file.exists():
                if chrome.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError("isolated Chrome did not start")
                time.sleep(0.05)
            port, path = port_file.read_text().splitlines()[:2]
            if not port.isdecimal() or not path.startswith("/devtools/browser/"):
                raise RuntimeError("invalid isolated Chrome endpoint")
            os.environ["BU_CDP_WS"] = f"ws://127.0.0.1:{port}{path}"
            yield chrome.pid
        finally:
            try:
                # Reap Chrome first: daemon shutdown can be interrupted by the
                # cancellation signal, but the owned browser must not survive it.
                if chrome is not None and chrome.poll() is None:
                    os.killpg(chrome.pid, signal.SIGTERM)
                    try:
                        chrome.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(chrome.pid, signal.SIGKILL)
                        chrome.wait(timeout=5)
                try:
                    from browser_harness.admin import restart_daemon
                    restart_daemon(name, require_clean=False)
                except BaseException:
                    # Cleanup must finish even when SIGTERM lands in this call.
                    pass
                proxy.shutdown()
                proxy_thread.join(timeout=2)
            finally:
                if handler_deferred:
                    signal.signal(signal.SIGTERM, original_term)


def main():
    global STAGE
    send(event="ready")
    request = receive()
    contract_bytes = base64.b64decode(request["contract_json"], validate=True)
    if hashlib.sha256(contract_bytes).hexdigest() != request["contract_digest"]:
        raise RuntimeError("browser contract digest mismatch")
    contract = json.loads(contract_bytes)
    for saved, transmitted in (
        ("WorkID", "work_id"), ("Actor", "actor"), ("URL", "url"),
        ("Goal", "goal"), ("ExpectText", "expect_text"),
        ("NonGoals", "non_goals"), ("MaxSteps", "max_steps"),
        ("MaxProviderUSD", "max_provider_usd"),
        ("ProviderModel", "provider_model"), ("ProviderBound", "provider_bound"),
    ):
        if contract[saved] != request[transmitted]:
            raise RuntimeError("browser contract input mismatch")
    provider_key = request.pop("typesafe_api_key", "")
    if not provider_key:
        raise RuntimeError("missing provider credential")
    STAGE = "chrome"
    with isolated_chrome(request["url"]):
        STAGE = "import"
        os.environ["TYPESAFE_API_KEY"] = provider_key
        from jev_ultrafast import Agent
        execute_goal(Agent, request)


def execute_goal(Agent, request):
    global STAGE
    STAGE = "agent"
    # Agent construction starts Browser Harness's daemon. It has no need for
    # provider credentials; restore them only after the browser is connected.
    secrets = {key: os.environ.pop(key) for key in MODEL_SECRET_NAMES if key in os.environ}
    try:
        non_goals = request.get("non_goals", [])
        goal = request["goal"]
        if non_goals:
            goal += "\nConstraints: " + "; ".join(non_goals)
        agent_instance = Agent(request["url"], goal, screenshots=False)
    finally:
        os.environ.update(secrets)
    with agent_instance as agent:
        for step in range(1, request["max_steps"] + 1):
            send(event="predict_request", step=step)
            if receive().get("allow") is not True:
                send(event="result", status="held", verified=False, steps=step - 1)
                return
            STAGE = "predict"
            state = agent.command("predict")
            page = state["page"]
            decision = state["decision"]
            choice = decision["choice"]
            if choice in {"DONE", "BLOCKED"}:
                action = {"kind": choice.lower(), "role": "terminal"}
            else:
                action = next(item for item in page["actions"] if item["id"] == choice)
            send(
                event="proposal",
                step=step,
                kind=action["kind"],
                role=action.get("role", ""),
                url=page["url"],
            )
            reply = receive()
            if reply.get("allow") is not True:
                send(event="result", status="held", verified=False, steps=step - 1)
                return
            STAGE = "act"
            state = agent.command("act", {"fingerprint": page["fingerprint"]})
            send(
                event="acted",
                step=step,
                status=state["status"],
                url=state["page"]["url"],
                elapsed_ms=state["elapsed_ms"],
            )
            if state["status"] in {"done", "blocked"}:
                STAGE = "verify"
                verified, url = verify(agent, request) if state["status"] == "done" else (False, state["page"]["url"])
                send(
                    event="result",
                    status="verified" if verified else ("unverified" if state["status"] == "done" else "blocked"),
                    verified=verified,
                    url=url,
                    steps=step,
                )
                return
        send(event="result", status="budget_exhausted", verified=False, steps=request["max_steps"])


if __name__ == "__main__":
    # Go cancels the whole uv/Python process group with SIGTERM. Raising from
    # the signal unwinds isolated_chrome's finally before the hard wait limit.
    def terminate(_signum, _frame):
        raise SystemExit(143)

    signal.signal(signal.SIGTERM, terminate)
    try:
        main()
    except Exception as exc:  # No raw exception text: upstream messages may contain page or input data.
        code = STAGE + "_" + type(exc).__name__
        if STAGE == "predict" and isinstance(exc, RuntimeError):
            match = re.fullmatch(r"Model provider returned HTTP (\d{3}); no action executed\.", str(exc))
            if match:
                code = "provider_HTTP_" + match.group(1)
            elif str(exc) == "Model connection failed; no action executed.":
                code = "provider_connection_failed"
        send(event="error", code=code)
        raise SystemExit(1)
