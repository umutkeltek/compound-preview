#!/usr/bin/env bun
/**
 * audit_switchback_capture.ts — empirical capture-coverage report for Switchback.
 *
 * Cross-references:
 *   1. ~/.switchback/config/switchback.yaml + mode-d.yaml + omp-tap.yaml (configured taps)
 *   2. ~/.switchback/state/body/index-v2.sqlite             (live capture state)
 *   3. ~/.switchback/state/body/pressure-state.json          (live degradation mode)
 *   4. ~/.codex/config.toml + ~/.prime/agent/models.json     (client base URLs)
 *   5. live listening ports via lsof                        (runtime topology)
 *
 * Reports, per lane (harness / client / upstream):
 *   - configured: does any active tap cover this path with capture_bodies=true?
 *   - observed:   is the live bodylog recording events for this upstream?
 *   - degraded:   is the PressureController currently in metadata_only mode?
 *
 * The operator asked for capture coverage across all harnesses/models/providers.
 * This script is the verification. Run it after any switchback config change.
 *
 * Output: JSON to stdout. --markdown emits a human-readable report.
 */

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { Database } from "bun:sqlite";

const HOME = homedir();
const MODE_DASH = process.argv.includes("--markdown");

const SWITCHBACK_YAML = join(HOME, ".switchback/config/switchback.yaml");
const MODE_D_YAML = join(HOME, ".switchback/config/mode-d.yaml");
const OMP_TAP_YAML = join(HOME, ".switchback/config/omp-tap.yaml");
const PRESSURE_STATE = join(HOME, ".switchback/state/body/pressure-state.json");
const BODY_INDEX = join(HOME, ".switchback/state/body/index-v2.sqlite");
const CODEX_CONFIG = join(HOME, ".codex/config.toml");
const PRIME_AGENT_MODELS = join(HOME, ".prime/agent/models.json");

type Tap = { id: string; bind: string; capture_bodies: boolean; upstream: string | undefined };
type Lane = {
  harness: string;
  client_url: string;
  tap: Tap | null;
  observed_event_count_24h: number;
  observed_body_bytes_24h: number;
  last_event_at: string | null;
  coverage: "captured" | "metadata_only" | "uncovered" | "n/a";
};
type Observed = { upstream: string; count_24h: number; bytes_24h: number; last_at_unix_ms: number | null };

function readTaps(path: string): Tap[] {
  if (!existsSync(path)) return [];
  const text = readFileSync(path, "utf8");
  const taps: Tap[] = [];
  const lines = text.split("\n");
  let current: Partial<Tap> | null = null;
  let inBlock = false;
  let blockIndent = -1;
  for (const line of lines) {
    const leading = line.match(/^(\s*)/)?.[1].length ?? 0;
    const trimmed = line.trimStart();
    if (trimmed === "taps:" || trimmed === "forward_proxies:") {
      inBlock = true;
      blockIndent = leading;
      continue;
    }
    if (inBlock && leading < blockIndent && trimmed.length > 0 && !trimmed.startsWith("#")) {
      inBlock = false;
    }
    if (!inBlock) continue;
    const mBind = trimmed.match(/^-\s+bind:\s*"?([^"\s]+)"?\s*$/);
    const mId = trimmed.match(/^-\s+id:\s*"?([^"\s]+)"?\s*$/);
    if (mBind || mId) {
      if (current && current.bind) {
        taps.push({ id: current.id ?? "?", bind: current.bind, capture_bodies: current.capture_bodies ?? false, upstream: current.upstream });
      }
      current = mId ? { id: mId[1] } : { id: current?.id, bind: mBind?.[1] ?? "" };
      continue;
    }
    if (current && trimmed.startsWith("bind: ")) current.bind = trimmed.slice("bind: ".length).trim().replace(/^"|"$/g, "");
    else if (current && trimmed.startsWith("id: ")) current.id = trimmed.slice("id: ".length).trim().replace(/^"|"$/g, "");
    else if (current && trimmed.startsWith("capture_bodies: ")) current.capture_bodies = trimmed.slice("capture_bodies: ".length).trim() === "true";
    else if (current && trimmed.startsWith("upstream: ")) current.upstream = trimmed.slice("upstream: ".length).trim().replace(/^"|"$/g, "");
  }
  if (current && current.bind) {
    taps.push({ id: current.id ?? "?", bind: current.bind, capture_bodies: current.capture_bodies ?? false, upstream: current.upstream });
  }
  return taps;
}

function readPressureMode(): { mode: string; reasons: string[]; unbacked_bytes: number; queue_drops: number } {
  if (!existsSync(PRESSURE_STATE)) return { mode: "unknown", reasons: [], unbacked_bytes: 0, queue_drops: 0 };
  try {
    const raw = JSON.parse(readFileSync(PRESSURE_STATE, "utf8"));
    return {
      mode: raw.mode ?? "unknown",
      reasons: raw.reasons ?? [],
      unbacked_bytes: raw.unbacked_bytes ?? 0,
      queue_drops: raw.queue_drops ?? 0,
    };
  } catch {
    return { mode: "invalid", reasons: [], unbacked_bytes: 0, queue_drops: 0 };
  }
}

function readCodexProviderUrl(): string {
  if (!existsSync(CODEX_CONFIG)) return "";
  const text = readFileSync(CODEX_CONFIG, "utf8");
  const m = text.match(/^openai_base_url\s*=\s*"([^"]+)"/m);
  return m ? m[1] : "";
}

function readPrimeAgentBaseUrl(): string {
  if (!existsSync(PRIME_AGENT_MODELS)) return "";
  try {
    const j = JSON.parse(readFileSync(PRIME_AGENT_MODELS, "utf8"));
    return j?.providers?.switchback?.baseUrl ?? "";
  } catch {
    return "";
  }
}

function readListeningPorts(): string[] {
  const r = spawnSync("lsof", ["-nP", "-iTCP", "-sTCP:LISTEN"], { stdio: "pipe", timeout: 5000 });
  const out = r.stdout?.toString() ?? "";
  const seen = new Set<string>();
  for (const line of out.split("\n")) {
    const m = line.match(/127\.0\.0\.1:(\d+)\s+\(LISTEN\)/);
    if (m) seen.add(`127.0.0.1:${m[1]}`);
  }
  return [...seen].sort();
}

function readObserved(): { map: Map<string, Observed>; total: number } {
  const map = new Map<string, Observed>();
  let total = 0;
  if (!existsSync(BODY_INDEX)) return { map, total };
  // The capture writer rolls WAL segments continuously. A plain readonly
  // handle can fail at PREPARE ("unable to open database file") while the
  // -shm is being recovered, and bun:sqlite opens lazily — so the retry must
  // wrap the whole open+query, and the handle must be read-write (guarded by
  // query_only) to recover the WAL-index. Mutating the body store from here
  // is structurally impossible: only a SELECT runs.
  let rows: Array<{ upstream: string; c: number; b: number; last_ms: number | null }> | null = null;
  for (let attempt = 0; attempt < 5 && rows === null; attempt++) {
    let db: Database | null = null;
    try {
      db = new Database(BODY_INDEX);
      db.query("PRAGMA query_only = ON").run();
      db.query("PRAGMA busy_timeout = 2000").run();
      const cutoff = Date.now() - 24 * 60 * 60 * 1000;
      rows = db
        .query("SELECT upstream, COUNT(*) AS c, COALESCE(SUM(body_bytes), 0) AS b, MAX(observed_at_unix_ms) AS last_ms FROM body_events WHERE observed_at_unix_ms > ? GROUP BY upstream")
        .all(cutoff) as Array<{ upstream: string; c: number; b: number; last_ms: number | null }>;
    } catch {
      rows = null;
      Bun.sleepSync(150);
    } finally {
      try { db?.close(); } catch { /* already closed */ }
    }
  }
  if (!rows) return { map, total };
  for (const r of rows) {
    map.set(r.upstream, { upstream: r.upstream, count_24h: r.c, bytes_24h: r.b, last_at_unix_ms: r.last_ms });
    total += r.c;
  }
  return { map, total };
}

function buildLane(harness: string, clientUrl: string, tapByPort: Map<string, Tap>, observed: Map<string, Observed>, pressure: { mode: string }, observedUpstreamOverride?: string): Lane {
  const m = clientUrl.match(/127\.0\.0\.1:(\d+)/);
  const port = m ? `127.0.0.1:${m[1]}` : null;
  const tap = port ? tapByPort.get(port) ?? null : null;
  let count = 0;
  let bytes = 0;
  let lastMs: number | null = null;
  if (tap && (tap.upstream || observedUpstreamOverride)) {
    const obs = observed.get(observedUpstreamOverride ?? tap.upstream!);
    if (obs) {
      count = obs.count_24h;
      bytes = obs.bytes_24h;
      lastMs = obs.last_at_unix_ms;
    }
  }
  let coverage: Lane["coverage"];
  if (clientUrl === "n/a") coverage = "n/a";
  else if (pressure.mode !== "segmented_full_wire") coverage = "metadata_only";
  else if (!tap) coverage = "uncovered";
  else if (!tap.capture_bodies) coverage = "metadata_only";
  else if (count === 0) coverage = "uncovered";
  else coverage = "captured";
  return {
    harness,
    client_url: clientUrl,
    tap: tap ? { id: tap.id, bind: tap.bind, capture_bodies: tap.capture_bodies, upstream: tap.upstream ?? "" } : null,
    observed_event_count_24h: count,
    observed_body_bytes_24h: bytes,
    last_event_at: lastMs ? new Date(lastMs).toISOString() : null,
    coverage,
  };
}

const allTaps = [...readTaps(SWITCHBACK_YAML), ...readTaps(MODE_D_YAML), ...readTaps(OMP_TAP_YAML)];
const tapByPort = new Map<string, Tap>();
for (const t of allTaps) tapByPort.set(t.bind, t);

const pressure = readPressureMode();
const observed = readObserved();
const codexUrl = readCodexProviderUrl();
const primeAgentUrl = readPrimeAgentBaseUrl();
const listening = readListeningPorts();

// Each lane represents one (harness, model/provider combo) → tap → bodylog chain.
const lanes: Lane[] = [
  buildLane("claude (mode-d)", "127.0.0.1:18780", tapByPort, observed.map, pressure),
  // Prime-agent routes its switchback calls through this tap (per its
  // ~/.prime/agent/models.json baseUrl); bodies captured here flow into
  // bodylog as "switchback/<model>".
  buildLane("prime-agent (switchback)", primeAgentUrl || "127.0.0.1:18774", tapByPort, observed.map, pressure),
  // oh-my-pi: interactive omp (~/.omp/agent/models.yml switchback provider) and
  // the governed headless profile (sb profile omp-qwen) both ride qwen-openai-tap
  // :18787 → relay route qwen/qwen3.8-max. The retired forward-proxy experiment
  // (:18790, omp-tap.yaml, no LaunchAgent — dead since 2026-08-23) is archived.
  buildLane("omp (interactive, models.yml switchback)", "127.0.0.1:18787", tapByPort, observed.map, pressure, "http://127.0.0.1:18765"),
  buildLane("omp-qwen (headless, governed)", "127.0.0.1:18787", tapByPort, observed.map, pressure, "http://127.0.0.1:18765"),
  buildLane("claude-tap (bare)", "127.0.0.1:18770", tapByPort, observed.map, pressure),
  buildLane("codex-tap (bare)", "127.0.0.1:18771", tapByPort, observed.map, pressure),
  buildLane("claude-zai", "127.0.0.1:18772", tapByPort, observed.map, pressure),
  buildLane("claude-zai-direct", "127.0.0.1:18773", tapByPort, observed.map, pressure),
  buildLane("claude-gpt56-ultra", "127.0.0.1:18774", tapByPort, observed.map, pressure),
  buildLane("claude-minimax", "127.0.0.1:18775", tapByPort, observed.map, pressure),
  buildLane("claude-minimax-direct", "127.0.0.1:18776", tapByPort, observed.map, pressure),
  buildLane("claude-qwen", "127.0.0.1:18777", tapByPort, observed.map, pressure),
  buildLane("claude-neuralwatt", "127.0.0.1:18778", tapByPort, observed.map, pressure),
  buildLane("claude-wpcom", "127.0.0.1:18781", tapByPort, observed.map, pressure),
  buildLane("claude-codex-oauth", "127.0.0.1:18782", tapByPort, observed.map, pressure),
  buildLane("claude-wpcom-headroom", "127.0.0.1:18783", tapByPort, observed.map, pressure),
  buildLane("claude-kimi", "127.0.0.1:18784", tapByPort, observed.map, pressure),
  buildLane("qwen-openai-tap", "127.0.0.1:18787", tapByPort, observed.map, pressure),
  buildLane("deepseek-openai-tap", "127.0.0.1:18788", tapByPort, observed.map, pressure),
  // The overlay-era forward-proxy ports (18791-18795) were never defined as
  // taps in any served config; they are omitted — relay routes + taps are the
  // only governed capture topology now.
  // Hermes is the orchestrator; it doesn't make LLM API calls of its own.
  buildLane("hermes (orchestrator, not an LLM harness)", "n/a", tapByPort, observed.map, pressure),
  // Codex default headroom direct — bypasses switchback tap entirely.
  // Anthropic direct — bare switchback gateway uses the Claude Code OAuth token
  // from the keychain (provider: anthropic, ANTHROPIC_OAUTH_TOKEN env). All
  // claude-haiku-4-5, claude-sonnet-4-5, claude-opus-4-1, claude-opus-4-5,
  // claude-sonnet-4-6 routes through this.
  buildLane("anthropic-direct", "127.0.0.1:18765", tapByPort, observed.map, pressure),
  buildLane("codex (default headroom)", codexUrl || "n/a", tapByPort, observed.map, pressure),
];

const summary = {
  schema_version: "compound.ssd-guardian.audit_switchback_capture.v2",
  generated_at: new Date().toISOString(),
  pressure_mode: pressure.mode,
  pressure_reasons: pressure.reasons,
  unbacked_bytes: pressure.unbacked_bytes,
  queue_drops: pressure.queue_drops,
  total_events_24h: observed.total,
  distinct_upstreams_24h: observed.map.size,
  listening_local_ports: listening,
  configured_tap_count: allTaps.length,
  configured_taps_capturing: allTaps.filter((t) => t.capture_bodies).length,
  lanes,
};

if (MODE_DASH) {
  const lines: string[] = [];
  lines.push(`# Switchback capture coverage`);
  lines.push(``);
  lines.push(`Generated: ${summary.generated_at}`);
  lines.push(`Pressure mode: **${pressure.mode}**${pressure.reasons.length ? ` (reasons: ${pressure.reasons.join(", ")})` : ""}`);
  lines.push(`Bodylog events (last 24 h): ${observed.total} across ${observed.map.size} distinct upstreams`);
  lines.push(`Configured taps: ${allTaps.length} (${summary.configured_taps_capturing} with capture_bodies=true)`);
  lines.push(`Unbacked bytes: ${pressure.unbacked_bytes}  |  Queue drops (lifetime): ${pressure.queue_drops}`);
  lines.push(``);
  lines.push(`| Lane | Tap | Capture | 24h events | 24h bytes | Last event | Status |`);
  lines.push(`|---|---|---|---|---|---|---|`);
  for (const l of lanes) {
    const status = l.coverage === "captured" ? "✓ captured" : l.coverage === "metadata_only" ? "⚠ metadata-only" : l.coverage === "n/a" ? "— n/a" : "✗ uncovered";
    const tap = l.tap ? `${l.tap.id} (${l.tap.bind})` : "—";
    const cap = l.tap ? (l.tap.capture_bodies ? "yes" : "no") : "—";
    lines.push(`| ${l.harness} | ${tap} | ${cap} | ${l.observed_event_count_24h} | ${Math.round(l.observed_body_bytes_24h / 1024 / 1024)} MB | ${l.last_event_at ?? "—"} | ${status} |`);
  }
  lines.push(``);
  lines.push(`## Live bodylog upstreams (last 24 h)`);
  lines.push(``);
  lines.push(`| Upstream | Events | Bytes | Last at |`);
  lines.push(`|---|---|---|---|`);
  const sortedUp = [...observed.map.values()].sort((a, b) => b.count_24h - a.count_24h);
  for (const u of sortedUp) {
    lines.push(`| ${u.upstream} | ${u.count_24h} | ${Math.round(u.bytes_24h / 1024 / 1024)} MB | ${u.last_at_unix_ms ? new Date(u.last_at_unix_ms).toISOString() : "—"} |`);
  }
  console.log(lines.join("\n"));
} else {
  console.log(JSON.stringify(summary, null, 2));
}
