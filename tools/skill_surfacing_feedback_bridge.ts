#!/usr/bin/env bun
/**
 * skill_surfacing_feedback_bridge.ts — compound-native door for skill surfacing feedback.
 *
 * Replaces an external skill_surfacing_detector.ts *import path* (not the optional LLM batch
 * body): normalizes feedback reports into the shape consumed by
 *   compound capability import-surfacing-misses <file>
 * and writes under COMPOUND_HOME/reviews/skill-surfacing/ (compound-owned, never an external
 * personal notes tree).
 *
 * Propose-only: never mutates CapabilityGraph by itself — import-surfacing-misses does.
 *
 *   bun tools/skill_surfacing_feedback_bridge.ts --from-proposals PATH [--out PATH]
 *   bun tools/skill_surfacing_feedback_bridge.ts --from-usage
 */
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

const HOME = process.env.COMPOUND_HOME?.trim() || join(homedir(), ".compound");
const DEFAULT_OUT_DIR = join(HOME, "reviews", "skill-surfacing");

function arg(name: string, fallback = ""): string {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 ? process.argv[i + 1] ?? fallback : fallback;
}
function has(name: string): boolean {
  return process.argv.includes(`--${name}`);
}

type Entry = { skill: string; confidence: number; reason: string };
type Receipt = {
  dir: string;
  kind: string;
  lanes?: string;
  real: Entry[];
  false_triggered?: Entry[];
  falseTriggered?: Entry[];
};

type Report = {
  id: string;
  generated: string;
  scanned: number;
  skipped: number;
  laneFails: number;
  remaining: number;
  proposals: unknown[];
  all_misses: unknown[];
  false_triggers: Entry[];
  perReceipt: Receipt[];
  source: string;
  provenance: string[];
};

function loadJSON(path: string): any {
  return JSON.parse(readFileSync(path, "utf8"));
}

function normalizeEntry(e: any): Entry | null {
  const skill = String(e?.skill ?? "").trim();
  const confidence = Number(e?.confidence ?? e?.maxConf ?? 0);
  const reason = String(e?.reason ?? e?.reasons?.[0] ?? "").trim();
  if (!skill || confidence < 0.7 || !reason) return null;
  return { skill, confidence, reason };
}

function fromProposals(path: string): { receipts: Receipt[]; falseTriggers: Entry[] } {
  if (!existsSync(path)) return { receipts: [], falseTriggers: [] };
  const src = loadJSON(path);
  const receipts: Receipt[] = [];
  const falseTriggers: Entry[] = [];
  for (const rec of src.perReceipt ?? []) {
    const real = (rec.real ?? []).map(normalizeEntry).filter(Boolean) as Entry[];
    const ft = [...(rec.false_triggered ?? []), ...(rec.falseTriggered ?? [])]
      .map(normalizeEntry)
      .filter(Boolean) as Entry[];
    falseTriggers.push(...ft);
    if (real.length === 0 && ft.length === 0) continue;
    receipts.push({
      dir: String(rec.dir ?? "imported"),
      kind: String(rec.kind ?? "human"),
      lanes: String(rec.lanes ?? "imported"),
      real,
      false_triggered: ft,
    });
  }
  for (const m of src.false_triggers ?? src.falseTriggers ?? []) {
    const e = normalizeEntry(m);
    if (e) falseTriggers.push(e);
  }
  return { receipts, falseTriggers };
}

function fromUsage(): Entry[] {
  const bin = process.env.COMPOUND_BIN?.trim() || "compound";
  const r = spawnSync(bin, ["capability", "usage", "--json"], {
    encoding: "utf8",
    env: process.env,
  });
  if (r.status !== 0 || !r.stdout) return [];
  let doc: any;
  try {
    doc = JSON.parse(r.stdout);
  } catch {
    return [];
  }
  const caps = Array.isArray(doc.capabilities)
    ? doc.capabilities
    : Array.isArray(doc.items)
      ? doc.items
      : [];
  const out: Entry[] = [];
  for (const c of caps) {
    const ref = String(c.ref ?? c.id ?? c.capability ?? "");
    const offered = Number(c.offered ?? c.times_offered ?? c.offered_count ?? 0);
    const used = Number(c.used ?? c.times_used ?? c.used_count ?? 0);
    const ignored = Number(c.ignored ?? c.times_ignored ?? c.ignored_count ?? 0);
    if (!(offered >= 5 && used === 0) && !(ignored >= 5 && used === 0)) continue;
    let skill = ref.includes(":") ? ref.split(":").pop()! : ref;
    skill = skill.replace(/^agents\./, "").replace(/^codex\./, "");
    if (!skill) continue;
    const conf = Math.min(0.9, 0.7 + Math.min(ignored, offered) / 50);
    out.push({
      skill,
      confidence: Number(conf.toFixed(2)),
      reason: `offered repeatedly but never consumed (offered=${offered} used=${used} ignored=${ignored})`,
    });
  }
  return out.sort((a, b) => b.confidence - a.confidence).slice(0, 20);
}

function main() {
  const fromPath = arg("from-proposals", "");
  const useUsage = has("from-usage") || !fromPath;
  const outPath =
    arg("out") ||
    join(
      DEFAULT_OUT_DIR,
      `skill-surfacing-feedback-${new Date().toISOString().slice(0, 10)}.json`,
    );

  const receipts: Receipt[] = [];
  const falseTriggers: Entry[] = [];

  if (fromPath) {
    const got = fromProposals(resolve(fromPath));
    receipts.push(...got.receipts);
    falseTriggers.push(...got.falseTriggers);
  }
  if (useUsage || has("from-usage")) {
    const ft = fromUsage();
    if (ft.length) {
      falseTriggers.push(...ft);
      receipts.push({
        dir: "usage-projection-false-triggers",
        kind: "usage_projection",
        lanes: "deterministic-usage",
        real: [],
        false_triggered: ft,
      });
    }
  }

  const report: Report = {
    id: `ssfb_${Date.now().toString(16)}`,
    generated: new Date().toISOString(),
    scanned: receipts.length,
    skipped: 0,
    laneFails: 0,
    remaining: 0,
    proposals: [],
    all_misses: [],
    false_triggers: falseTriggers,
    perReceipt: receipts,
    source: "compound.tools.skill_surfacing_feedback_bridge",
    provenance: [
      "compound://tools/skill_surfacing_feedback_bridge",
      fromPath ? `source_file:${fromPath}` : "source:none",
      useUsage ? "compound capability usage --json" : "",
    ].filter(Boolean),
  };

  mkdirSync(join(outPath, ".."), { recursive: true });
  writeFileSync(outPath, JSON.stringify(report, null, 2) + "\n");
  console.log(JSON.stringify({
    status: "ok",
    out: outPath,
    receipts: receipts.length,
    false_triggers: falseTriggers.length,
    next_command: ["compound", "capability", "import-surfacing-misses", outPath],
  }, null, 2));
}

main();
