#!/usr/bin/env python3
"""Bounded, read-only GitHub evidence scout.

The scout only stages source evidence. It does not clone, execute, install,
write COMPOUND_HOME, or decide whether an upstream idea is useful.
"""
from __future__ import annotations
import argparse, hashlib, json, os, re, sys, time
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import Request, urlopen

CONFIG_SCHEMA = "compound.private.upstream_learning_config.v0.1"
OBS_SCHEMA = "compound.source_observation.v0.1"
STATE_SCHEMA = "compound.private.upstream_learning_state.v0.2"
GITHUB_WEB_HOST = "github.com"
GITHUB_API_HOST = "api.github.com"
GITHUB_RAW_HOST = "raw.githubusercontent.com"
MAX_REPOS, MAX_FILES, MAX_BYTES, MAX_TEXT, MAX_WALL = 12, 24, 1_000_000, 200_000, 30.0
SAFE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")

class ScoutError(Exception): pass

def sha(data: bytes) -> str: return "sha256:" + hashlib.sha256(data).hexdigest()

def safe_rel(value: str) -> str:
    if not isinstance(value, str) or not value or value.startswith("/") or "\\" in value:
        raise ScoutError("inspection path must be relative")
    p = PurePosixPath(value)
    if ".." in p.parts or not SAFE.fullmatch(value): raise ScoutError(f"unsafe inspection path: {value!r}")
    return str(p)

def repo_parts(url: str) -> tuple[str, str]:
    p = urlparse(url)
    if p.scheme != "https" or p.netloc.lower() != GITHUB_WEB_HOST or p.username or p.password or p.query or p.fragment:
        raise ScoutError("repo_url must be an uncredentialed https://github.com/<owner>/<repo>")
    bits = [x for x in p.path.split("/") if x]
    if len(bits) != 2 or not all(re.fullmatch(r"[A-Za-z0-9_.-]+", x) for x in bits): raise ScoutError("repo_url must identify one repository")
    return bits[0], bits[1].removesuffix(".git")

def validate_config(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    if cfg.get("schema_version") != CONFIG_SCHEMA: raise ScoutError("unsupported schema_version")
    sources = cfg.get("sources")
    if not isinstance(sources, list) or not sources or len(sources) > MAX_REPOS: raise ScoutError(f"sources must contain 1..{MAX_REPOS} entries")
    result = []
    for src in sources:
        if not isinstance(src, dict): raise ScoutError("each source must be an object")
        owner, frontier = src.get("existing_compound_owner"), src.get("frontier")
        if not isinstance(owner, str) or not owner.strip() or not isinstance(frontier, str) or not frontier.strip(): raise ScoutError("owner and frontier are required")
        who, repo = repo_parts(src.get("repo_url")); branch = src.get("branch", "main")
        if not isinstance(branch, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", branch): raise ScoutError("branch must be a simple name")
        paths = src.get("inspection_paths")
        if not isinstance(paths, list) or not paths or len(paths) > MAX_FILES: raise ScoutError(f"inspection_paths must contain 1..{MAX_FILES} entries")
        for key in ("success_criterion", "falsifier", "cadence"):
            if not isinstance(src.get(key), str) or not src[key].strip(): raise ScoutError(f"{key} is required")
        result.append({"repo_url": f"https://{GITHUB_WEB_HOST}/{who}/{repo}", "owner": who, "repo": repo, "branch": branch,
                       "existing_compound_owner": owner.strip(), "frontier": frontier.strip(), "inspection_paths": [safe_rel(x) for x in paths],
                       "success_criterion": src["success_criterion"].strip(), "falsifier": src["falsifier"].strip(), "cadence": src["cadence"].strip()})
    return result

@dataclass
class Fetch:
    get: Callable[[str, int, float], bytes]

def http_get(url: str, limit: int, timeout: float) -> bytes:
    req = Request(url, headers={"User-Agent": "compound-upstream-learning-scout/0.2", "Accept": "application/json,text/plain"})
    with urlopen(req, timeout=timeout) as response:
        if int(response.headers.get("Content-Length", "0") or 0) > limit: raise ScoutError("response exceeds byte bound")
        body = response.read(limit + 1)
    if len(body) > limit: raise ScoutError("response exceeds byte bound")
    return body

def atomic_json(path: str, value: Any) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f: json.dump(value, f, sort_keys=True, indent=2); f.write("\n"); f.flush(); os.fsync(f.fileno())
    os.replace(tmp, path)

def reject_symlink_tree(path: str) -> None:
    absolute = os.path.abspath(path)
    if os.path.lexists(absolute) and os.path.islink(absolute): raise ScoutError("output path contains a symlink")
    parent = os.path.dirname(absolute)
    components = parent.split(os.path.sep)
    cur = os.path.sep
    for part in components:
        if not part: continue
        cur = os.path.join(cur, part)
        # macOS exposes /var and /tmp as system compatibility symlinks. They
        # are outside caller custody; user-controlled descendants are still
        # checked and rejected.
        if os.path.islink(cur) and cur not in ("/var", "/tmp"):
            raise ScoutError("output ancestor contains a symlink")
    if os.path.isdir(absolute):
        for root, dirs, files in os.walk(absolute, followlinks=False):
            if any(os.path.islink(os.path.join(root, x)) for x in dirs + files):
                raise ScoutError("output tree contains a symlink")

def ensure_output(out_dir: str) -> str:
    home = os.environ.get("COMPOUND_HOME") or os.path.expanduser("~/.compound"); out = os.path.abspath(out_dir)
    if home:
        h = os.path.abspath(home)
        if out == h or out.startswith(h + os.path.sep): raise ScoutError("staged output must be outside COMPOUND_HOME")
    reject_symlink_tree(out); os.makedirs(out, mode=0o700, exist_ok=True); reject_symlink_tree(out); return out

def observation(src: dict[str, Any], commit: str, revision: str, body_hashes: dict[str, str], now: str, old: dict[str, Any] | None) -> dict[str, Any]:
    key = f"{src['repo_url']}#{revision}#{src['frontier']}"; oid = "srcobs_" + hashlib.sha256(key.encode()).hexdigest()[:24]
    refs = ["pending_body:" + h for h in body_hashes.values()]
    obs = {"schema_version": OBS_SCHEMA, "id": oid, "adapter_id": "compound.private.upstream-learning-scout.v0.2", "source_system": "github", "observed_at": now,
           "source_ref": {"path": src["repo_url"], "kind": "github_repository", "owner": src["existing_compound_owner"], "source_class": "evidence", "revision": commit},
           "maps_to": [{"object_family": "capability_observation", "candidate_kind": "capability_observation", "entity_hint": src["existing_compound_owner"], "candidate_revision": revision, "promote_allowed": False}],
           "evidence_refs": sorted(set(refs + ["git:" + commit, "content:" + revision])), "content_revision": revision,
           "scout_context": {"frontier": src["frontier"], "inspection_paths": src["inspection_paths"], "success_criterion": src["success_criterion"], "falsifier": src["falsifier"], "cadence": src["cadence"],
                             "assessment": {"evidence_availability": "bounded_text_available", "relevance": "unassessed", "novelty": "unknown", "actionability": "unknown", "priority_band": "unranked"}}}
    if old and old.get("observation_id") != oid: obs["invalidates_observation_id"] = old["observation_id"]
    return obs

def run(cfg: dict[str, Any], out_dir: str, fetch: Fetch = Fetch(http_get), ack: str | None = None, now: str | None = None) -> dict[str, Any]:
    sources = validate_config(cfg); out = ensure_output(out_dir); content = os.path.join(out, "content", "sha256"); pending_dir = os.path.join(out, "pending"); os.makedirs(content, mode=0o700, exist_ok=True); os.makedirs(pending_dir, mode=0o700, exist_ok=True)
    state_path = os.path.join(out, ".state.json")
    try:
        with open(state_path, encoding="utf-8") as f: state = json.load(f)
    except FileNotFoundError: state = {"schema_version": STATE_SCHEMA, "observations": {}}
    if state.get("schema_version") != STATE_SCHEMA or not isinstance(state.get("observations"), dict):
        raise ScoutError("invalid scout state; history must not be reset")
    if ack:
        item = state["observations"].get(ack)
        if not item or item.get("status") not in ("pending", "acked"):
            raise ScoutError("unknown or inactive observation id for ack")
        pending_path = item.get("pending_path")
        if pending_path:
            pending_abs = os.path.abspath(pending_path)
            pending_root = os.path.abspath(os.path.join(out, "pending"))
            if not pending_abs.startswith(pending_root + os.path.sep) or os.path.islink(pending_abs):
                raise ScoutError("ack path escaped pending directory")
        item["status"] = "acked"
        item["acked_at"] = now or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        atomic_json(state_path, state)
        if pending_path and os.path.isfile(pending_abs):
            os.unlink(pending_abs)
        return {"status": "acked", "observation_id": ack}
    started, stamp = time.monotonic(), now or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    consumed_bytes = 0
    def read(url, requested_limit, ignored_timeout):
        nonlocal consumed_bytes
        remaining_time = MAX_WALL - (time.monotonic() - started)
        remaining_bytes = MAX_BYTES - consumed_bytes
        if remaining_time <= 0:
            raise ScoutError("whole-run wall deadline exceeded")
        limit = min(requested_limit, remaining_bytes)
        if limit <= 0:
            raise ScoutError("whole-run metadata/content byte budget exhausted")
        body = fetch.get(url, limit, remaining_time)
        if len(body) > limit:
            raise ScoutError("response exceeds whole-run metadata/content byte budget")
        consumed_bytes += len(body)
        return body
    report: dict[str, Any] = {"schema_version": "compound.private.upstream_learning_run.v0.2", "status": "ok", "staged": [], "pending": [x["observation_id"] for x in state["observations"].values() if x.get("status") == "pending"], "failures": []}
    for src in sources:
        if time.monotonic() - started >= MAX_WALL: report["failures"].append({"status": "wall_deadline", "source": src["repo_url"]}); break
        source_key = f"{src['repo_url']}#{src['branch']}#{src['frontier']}"
        try:
            left = lambda: max(0.1, MAX_WALL - (time.monotonic()-started))
            commit_raw = read(f"https://{GITHUB_API_HOST}/repos/{src['owner']}/{src['repo']}/commits/{src['branch']}", MAX_BYTES, left()); commit = json.loads(commit_raw.decode())["sha"]
            if not isinstance(commit, str) or not re.fullmatch(r"[0-9a-fA-F]{40}", commit): raise ScoutError("metadata lacks pinned commit SHA")
            body_hashes, bodies, used = {}, {}, len(commit_raw)
            for path in src["inspection_paths"]:
                listing = json.loads(read(f"https://{GITHUB_API_HOST}/repos/{src['owner']}/{src['repo']}/contents/{path}?ref={commit}", MAX_BYTES, left()).decode())
                if listing.get("type") != "file" or listing.get("path") != path: raise ScoutError(f"inspection path is not a regular file: {path}")
                raw = read(f"https://{GITHUB_RAW_HOST}/{src['owner']}/{src['repo']}/{commit}/{path}", min(MAX_TEXT, MAX_BYTES-used), left()); used += len(raw)
                if used > MAX_BYTES: raise ScoutError("aggregate metadata/content exceeds byte bound")
                h = sha(raw); body_hashes[path], bodies[h] = h, raw
            revision = "sha256:" + hashlib.sha256((source_key + json.dumps(body_hashes, sort_keys=True)).encode()).hexdigest()
            old = next((x for x in state["observations"].values() if x.get("source_key") == source_key and x.get("status") != "invalidated"), None)
            if old and old.get("revision") == revision:
                old["latest_head"] = commit; old["last_checked_at"] = stamp; atomic_json(state_path, state)
                if old.get("status") == "pending" and old["observation_id"] not in report["pending"]: report["pending"].append(old["observation_id"])
                continue
            obs = observation(src, commit, revision, body_hashes, stamp, old); oid = obs["id"]; obs_path = os.path.join(out, oid + ".json")
            raw_path = os.path.join(content, hashlib.sha256((oid + ":aggregate").encode()).hexdigest() + ".txt")
            if not os.path.exists(obs_path):
                for h, raw in bodies.items():
                    path = os.path.join(content, h.removeprefix("sha256:") + ".txt")
                    if not os.path.exists(path):
                        with open(path + ".tmp", "wb") as f: f.write(raw); f.flush(); os.fsync(f.fileno())
                        os.replace(path + ".tmp", path)
                aggregate = b"".join((b"\n--- inspected file: " + path.encode() + b" ---\n" + bodies[h] for path, h in body_hashes.items()))
                if len(aggregate) > MAX_BYTES: raise ScoutError("aggregate custody body exceeds byte bound")
                with open(raw_path + ".tmp", "wb") as f: f.write(aggregate); f.flush(); os.fsync(f.fileno())
                os.replace(raw_path + ".tmp", raw_path)
                atomic_json(obs_path, obs)
            if old: old["status"], old["invalidated_by"] = "invalidated", oid
            pending_record = {"observation_id": oid, "source_key": source_key, "source_head": commit, "content_revision": revision, "raw_body_file": raw_path, "body_files": {path: os.path.join(content, h.removeprefix("sha256:") + ".txt") for path, h in body_hashes.items()}, "source_observation_file": obs_path, "topic": "upstream-learning"}
            atomic_json(os.path.join(pending_dir, oid + ".json"), pending_record)
            state["observations"][oid] = {"observation_id": oid, "source_key": source_key, "revision": revision, "latest_head": commit, "status": "pending", "path": obs_path, "pending_path": os.path.join(pending_dir, oid + ".json")}
            atomic_json(state_path, state); report["staged"].append(oid); report["pending"].append(oid)
        except (ScoutError, HTTPError, URLError, ValueError, UnicodeDecodeError, TimeoutError, KeyError) as e:
            report["failures"].append({"source": src["repo_url"], "status": "source_failure", "error": str(e)}); atomic_json(state_path, state)
    atomic_json(os.path.join(out, "outbox.json"), {"schema_version": "compound.private.upstream_learning_outbox.v0.2", "pending": report["pending"]}); atomic_json(os.path.join(out, "last-run.json"), report); return report

def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__); p.add_argument("config"); p.add_argument("output_dir"); p.add_argument("--ack"); a = p.parse_args(argv)
    try:
        with open(a.config, encoding="utf-8") as f: cfg = json.load(f)
        r = run(cfg, a.output_dir, ack=a.ack)
    except (OSError, json.JSONDecodeError, ScoutError) as e: print(json.dumps({"status": "invalid_config", "error": str(e)})); return 2
    print(json.dumps({"status": r["status"], "staged": len(r.get("staged", [])), "pending": len(r.get("pending", [])), "failures": len(r.get("failures", []))})); return 1 if r.get("failures") else 0

if __name__ == "__main__": sys.exit(main())
