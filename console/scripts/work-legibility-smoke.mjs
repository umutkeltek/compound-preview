// Legibility guard for the Work page's empty / degraded copy.
//
// Operator prose must speak plain language — a raw CLI command belongs in a
// CopyCmd affordance (WORK_COPY.*.command), never inside a sentence the operator
// reads (console legibility spec §2 G3: no raw commands / flags / paths / ids in
// the sentence). This smoke imports the copy model — the real seam — and fails
// if any non-`command` string reads as a CLI invocation, or if a command that
// was lifted out of prose has gone missing (moved, not deleted).
//
// Run directly with `bun scripts/work-legibility-smoke.mjs`, or through the
// Bun-gated console-work-legibility-smoke step in the portable proof-check.

import { WORK_COPY } from "../src/lib/work-copy.ts";

const CLI_PATTERNS = [
  { name: "compound command", re: /\bcompound\s+[a-z][\w-]*/i },
  { name: "backtick-wrapped command", re: /`[^`]*\b(?:compound|bun|npm|npx|git|sb)\b[^`]*`/i },
  { name: "cli flag", re: /(?:^|\s)--[a-z][\w-]*/ },
];

// The lane-traffic command the packet lifted out of prose must still exist as an
// affordance somewhere in the model — proof it was MOVED, not deleted.
const REQUIRED_COMMANDS = ["compound route import-account-projection"];

// Recursively collect every string leaf with its dotted key-path. Leaves whose
// final key is `command` are the copy affordances (CLI allowed, by design);
// every other string is operator prose (CLI forbidden).
function walk(node, path, prose, commands) {
  if (typeof node === "string") {
    const key = path[path.length - 1];
    if (key === "command") commands.push({ path: path.join("."), value: node });
    else prose.push({ path: path.join("."), value: node });
    return;
  }
  if (node && typeof node === "object") {
    for (const [k, v] of Object.entries(node)) walk(v, [...path, k], prose, commands);
  }
}

const prose = [];
const commands = [];
walk(WORK_COPY, ["WORK_COPY"], prose, commands);

const failures = [];

// 1. No operator-prose string may read as a CLI invocation.
for (const { path, value } of prose) {
  for (const { name, re } of CLI_PATTERNS) {
    if (re.test(value)) {
      failures.push(`prose reads as a ${name}: ${path} = ${JSON.stringify(value)}`);
    }
  }
}

// 2. Moved-not-deleted: each lifted command must still be reachable as an affordance.
for (const required of REQUIRED_COMMANDS) {
  if (!commands.some((c) => c.value === required)) {
    failures.push(`command affordance missing (moved, not deleted?): ${JSON.stringify(required)}`);
  }
}

// 3. Guard against an empty seam — a model reshape that leaves nothing to check.
if (prose.length === 0) {
  failures.push("no operator-prose strings found in WORK_COPY — model shape changed, smoke checks nothing");
}

if (failures.length) {
  console.error("work-legibility-smoke FAILED\n" + failures.map((f) => "  - " + f).join("\n"));
  process.exit(1);
}
console.log(
  `work-legibility-smoke ok: ${prose.length} operator-prose strings CLI-free; ` +
    `${commands.length} command(s) live only in copy affordances`,
);
