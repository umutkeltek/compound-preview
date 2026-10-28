"""Hermes plugin: emit a SessionEnd receipt to compound at end-of-session.

Canonical copy lives in the compound repo at adapters/hermes/compound_memory/__init__.py
so a fresh install / re-install produces a byte-identical plugin to the one shipped here.
The shipped-in-place copy at ~/.hermes/profiles/compoundcanary/plugins/compound_memory/
is what hermes actually loads; the canonical copy is the source of truth.

Slice 6 of the 2026-08-26 session-end-codex-hermes work item adds the on_session_end
lifecycle hook this plugin did not previously register. The hook does TWO things:

  1. Export the session's messages from hermes's state.db (sqlite) to a JSONL file
     under ~/.compound/state/hermes-sessions/<session_id>.jsonl using the claude-code
     role/content shape, so compound's existing parser (internal/hook/scanSessionTranscript)
     reads it without learning a third format. Read-only on state.db -- never write.

  2. Invoke `compound hook session-end --live --harness hermes` with stdin
     {"session_id","cwd","transcript_path"}, 8s timeout, fail-open (log, never raise).

The SessionEnd handler (hook.go:386 -> sessionEnd) routes the event through
buildWorklessSessionReceipt, which scans the exported JSONL for skills_fired exactly
the way it scans a claude-code SessionEnd transcript. So hermes now feeds skills_fired
evidence into the capgraph the same way claude does, without faking a SessionEnd
capability the harness already advertises (harness.go:475 hermes SessionEndReceipt=true).

Fail-open: any exception (db locked, write failed, compound binary absent, hook timeout)
is logged and swallowed. The plugin MUST never break hermes's shutdown sequence; the
worst outcome of a broken plugin is a missing receipt, which the upstream registry
already treats as a capability-without-receipt -- not a crash.

Discovery: hermes scans <profile>/plugins/<name>/__init__.py for register(ctx).
"""

from __future__ import annotations

import json
import logging
import os
import sqlite3
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable


logger = logging.getLogger(__name__)

PLUGIN_NAME = "compound_memory"
SESSION_END_TIMEOUT_SECONDS = 8


def _hermes_home() -> Path:
    """Resolve hermes home the same way the installed plugin does (HERMES_HOME -> ~/.hermes)."""
    raw = os.environ.get("HERMES_HOME", "").strip()
    if raw:
        return Path(raw).expanduser().resolve()
    return (Path.home() / ".hermes").resolve()


def _state_db_path(home: Path) -> Path:
    """state.db lives at <HERMES_HOME>/state.db. Profile-specific state lives at
    <HERMES_HOME>/profiles/<profile>/state.db -- this resolver picks the profile-scoped
    db when HERMES_HOME is a profile dir, otherwise the top-level db."""
    if home.parent.name == "profiles":
        return home / "state.db"
    return home / "state.db"


def _compound_state_dir() -> Path:
    """~/.compound/state/hermes-sessions/ is the convention compound owns for
    hermes-exported session JSONLs. Falls back to $COMPOUND_HOME/state/hermes-sessions
    when set, matching compound's own home resolution (compound/internal/home)."""
    raw = os.environ.get("COMPOUND_HOME", "").strip()
    root = Path(raw).expanduser() if raw else Path.home() / ".compound"
    return root / "state" / "hermes-sessions"


def _export_session_to_jsonl(
    db_path: Path,
    session_id: str,
    out_path: Path,
) -> int:
    """Read hermes's state.db for the session's messages and write a Claude-shape JSONL.

    Hermes schema (live state.db, 2026.08): messages(id, session_id, role, content,
    tool_name, timestamp, ...). The compound parser only needs (role, content,
    timestamp), so we map tool_name into a tool_use block when present (best-effort --
    not every hermes message carries a structured tool_call payload, and the parser's
    skillsFromLine rule catches a tool_use input that mentions skills/<name>/SKILL.md
    regardless of how it is shaped). Returns the number of lines written.

    Read-only sqlite: opened via URI mode with mode=ro so a misbehaving export can never
    corrupt hermes's own state. The connection is closed before we return -- no held
    locks on shutdown.
    """
    if not db_path.exists():
        logger.warning(
            "compound_memory: hermes state.db not found at %s; SessionEnd receipt skipped",
            db_path,
        )
        return 0
    out_path.parent.mkdir(parents=True, exist_ok=True)
    # mode=ro refuses any write attempt; the URI form is hermes-friendly because
    # hermes itself uses the same DB and we do not want to compete for write locks
    # during hermes's own shutdown flush.
    uri = f"file:{db_path}?mode=ro"
    try:
        conn = sqlite3.connect(uri, uri=True, timeout=2.0)
    except sqlite3.Error as exc:
        logger.warning("compound_memory: cannot open %s read-only: %s", db_path, exc)
        return 0
    try:
        cur = conn.cursor()
        # Hermes writes messages with active=1 (not compacted). tool_calls is the
        # structured JSON column hermes populates for assistant tool invocations; we
        # ignore compacted=1 rows. finished_reason / reasoning / platform_message_id
        # are kept aside -- the compound parser does not consume them today.
        cur.execute(
            "SELECT role, content, tool_name, tool_calls, timestamp "
            "FROM messages WHERE session_id = ? AND active = 1 "
            "ORDER BY timestamp ASC, id ASC",
            (session_id,),
        )
        rows = cur.fetchall()
    except sqlite3.Error as exc:
        logger.warning(
            "compound_memory: state.db query failed for session %s: %s",
            session_id, exc,
        )
        return 0
    finally:
        conn.close()

    written = 0
    tmp = out_path.with_suffix(out_path.suffix + ".tmp")
    try:
        with tmp.open("w", encoding="utf-8") as fh:
            for role, content, tool_name, tool_calls, timestamp in rows:
                if not isinstance(role, str) or not role:
                    continue
                if not isinstance(content, str):
                    content = "" if content is None else str(content)
                entry: dict[str, Any] = {
                    "type": "message",
                    "role": role,
                    "content": [{"type": "text", "text": content}],
                    "timestamp": _ts_to_iso(timestamp),
                }
                blocks = _hermes_tool_blocks(tool_calls, tool_name)
                if blocks:
                    entry["content"].extend(blocks)
                fh.write(json.dumps(entry, ensure_ascii=False) + "\n")
                written += 1
    except OSError as exc:
        logger.warning("compound_memory: cannot write %s: %s", out_path, exc)
        return 0
    try:
        tmp.replace(out_path)
    except OSError as exc:
        logger.warning("compound_memory: cannot rename %s: %s", out_path, exc)
        return 0
    return written


def _hermes_tool_blocks(tool_calls_raw: Any, tool_name: Any) -> list[dict[str, Any]]:
    """Convert Hermes tool_calls into Claude-shaped tool_use blocks.

    Current Hermes stores OpenAI calls as
    ``{"type": "function", "function": {"name": ..., "arguments": "..."}}``.
    Older rows put name and arguments directly on the entry. Accept both, plus
    bare ``tool_name`` rows, without dropping structured arguments.
    """
    blocks: list[dict[str, Any]] = []
    parsed: Any = None
    if isinstance(tool_calls_raw, str) and tool_calls_raw.strip():
        try:
            parsed = json.loads(tool_calls_raw)
        except (TypeError, ValueError):
            parsed = None
    elif isinstance(tool_calls_raw, list):
        parsed = tool_calls_raw

    if isinstance(parsed, list):
        for entry in parsed:
            if not isinstance(entry, dict):
                continue

            function = entry.get("function")
            if not isinstance(function, dict):
                function = {}
            name = (
                function.get("name")
                or entry.get("name")
                or entry.get("tool_name")
                or entry.get("type")
            )
            if not isinstance(name, str) or not name:
                continue

            input_obj = function.get("arguments")
            if input_obj is None:
                input_obj = entry.get("arguments")
            if input_obj is None:
                input_obj = entry.get("args")
            if input_obj is None:
                input_obj = entry.get("input")
            if isinstance(input_obj, str):
                try:
                    input_obj = json.loads(input_obj)
                except (TypeError, ValueError):
                    input_obj = {}
            if not isinstance(input_obj, dict):
                input_obj = {}
            blocks.append({"type": "tool_use", "name": name, "input": input_obj})

    if not blocks and isinstance(tool_name, str) and tool_name:
        blocks.append({"type": "tool_use", "name": tool_name, "input": {}})
    return blocks


def _ts_to_iso(ts: Any) -> str:
    """Hermes stores REAL unix timestamps (seconds since epoch). Render as RFC3339 so
    compound's receipt carries the same shape it carries for a claude-code SessionEnd.
    Failure to parse yields an empty string -- the parser does not require it."""
    if not isinstance(ts, (int, float)):
        return ""
    try:
        from datetime import datetime, timezone
        return datetime.fromtimestamp(float(ts), tz=timezone.utc).isoformat()
    except (ValueError, OverflowError, OSError):
        return ""


def _invoke_compound_session_end(session_id: str, cwd: str, transcript_path: str) -> None:
    """Spawn compound hook session-end --live --harness hermes with a JSON stdin.

    Timeout is bounded (8s) so a stalled compound binary can never block hermes's
    shutdown sequence. Failures are logged, never raised -- the plugin's contract
    is "log and continue", not "fail loud and stop the harness"."""
    binary = os.environ.get("COMPOUND_BIN", "compound")
    payload = {
        "session_id": session_id,
        "cwd": cwd,
        "transcript_path": transcript_path,
    }
    try:
        stdin_bytes = json.dumps(payload).encode("utf-8")
    except (TypeError, ValueError) as exc:
        logger.warning("compound_memory: cannot encode stdin payload: %s", exc)
        return
    try:
        proc = subprocess.run(
            [binary, "hook", "session-end", "--live", "--harness", "hermes"],
            input=stdin_bytes,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            timeout=SESSION_END_TIMEOUT_SECONDS,
            check=False,
        )
    except FileNotFoundError:
        logger.warning(
            "compound_memory: %s binary not on PATH; SessionEnd receipt not delivered",
            binary,
        )
        return
    except subprocess.TimeoutExpired:
        logger.warning(
            "compound_memory: %s hook session-end timed out after %ds; receipt may be missing",
            binary, SESSION_END_TIMEOUT_SECONDS,
        )
        return
    except OSError as exc:
        logger.warning("compound_memory: spawn %s failed: %s", binary, exc)
        return
    if proc.returncode != 0:
        logger.warning(
            "compound_memory: %s hook session-end returned %d: %s",
            binary, proc.returncode,
            proc.stderr.decode("utf-8", errors="replace").strip(),
        )


def _resolve_cwd(session_id: str, home: Path) -> str:
    """Hermes stores cwd on the sessions row. Read it best-effort so the receipt's
    Scope.Surface lands at the right path (otherwise we fall back to the hermes home)."""
    db_path = _state_db_path(home)
    if not db_path.exists():
        return str(home)
    uri = f"file:{db_path}?mode=ro"
    try:
        conn = sqlite3.connect(uri, uri=True, timeout=2.0)
    except sqlite3.Error:
        return str(home)
    try:
        cur = conn.cursor()
        cur.execute("SELECT cwd FROM sessions WHERE id = ?", (session_id,))
        row = cur.fetchone()
        if row and isinstance(row[0], str) and row[0].strip():
            return row[0].strip()
        return str(home)
    except sqlite3.Error:
        return str(home)
    finally:
        conn.close()


def _on_session_end(**kwargs: Any) -> None:
    """Lifecycle hook: emit SessionEnd receipt at end-of-session.

    Triggered by hermes's lifecycle.invoke_hook("on_session_end", session_id=...,,
    completed=True/False, interrupted=True/False, model=..., platform=...,). Any other
    kwargs are tolerated and ignored -- hermes has reserved the right to add more
    fields without breaking first-party observers.
    """
    session_id = str(kwargs.get("session_id") or "").strip()
    if not session_id:
        logger.debug("compound_memory: on_session_end called without session_id; skipping")
        return

    home = _hermes_home()
    db_path = _state_db_path(home)
    out_dir = _compound_state_dir()
    out_path = out_dir / f"{session_id}.jsonl"

    try:
        written = _export_session_to_jsonl(db_path, session_id, out_path)
    except Exception as exc:  # noqa: BLE001 -- fail-open by contract
        logger.warning("compound_memory: export raised %s; receipt skipped", exc)
        return
    if written == 0:
        # No messages exported -- nothing for the parser to scan. Skip the compound
        # call so we do not pay its startup cost on a session that produced no work.
        # (Compound's sessionEnd() handles empty path by no-op receipt build.)
        logger.debug(
            "compound_memory: session %s exported 0 lines; skipping compound hook call",
            session_id,
        )
        return

    cwd = _resolve_cwd(session_id, home)
    try:
        _invoke_compound_session_end(session_id, cwd, str(out_path))
    except Exception as exc:  # noqa: BLE001 -- fail-open by contract
        logger.warning("compound_memory: compound hook call raised %s", exc)


def register(ctx: Any) -> None:
    """Plugin entry point. Herme calls this once at plugin load.

    ctx.register_hook accepts a name in VALID_HOOKS (hermes_cli.plugins.VALID_HOOKS)
    plus a callable; dispatch is run-all, so other plugins' on_session_end callbacks
    also fire and we do not need to coordinate with them.
    """
    ctx.register_hook("on_session_end", _on_session_end)