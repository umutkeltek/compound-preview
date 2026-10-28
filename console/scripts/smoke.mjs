import assert from "node:assert/strict";

import { replaceReviewDecisionFields } from "../src/lib/review-yaml.ts";

const fixture = `schema_version: compound.capability.external_repo_intake_review.v0.1
id: err_d1372870f522
plan_id: extrepo_0e038992f33e
repo: Panniantong/Agent-Reach
decision: keep_needs_review
owner: "knowledge intake edges / distribution research"
rationale: ""
confidence: 2.00
next_action: "..."
evidence_refs: [...]
boundaries: [...]
`;

const changed = replaceReviewDecisionFields(fixture, {
  decision: "merge_existing",
  rationale: "Useful as reference evidence; merge only through the existing owner.",
  confidence: 0.85,
});

const before = fixture.split("\n");
const after = changed.split("\n");

assert.equal(after[4], "decision: merge_existing");
assert.equal(
  after[6],
  'rationale: "Useful as reference evidence; merge only through the existing owner."',
);
assert.equal(after[7], "confidence: 0.85");

for (let i = 0; i < before.length; i++) {
  if ([4, 6, 7].includes(i)) continue;
  assert.equal(after[i], before[i], `line ${i + 1} changed unexpectedly`);
}

console.log("smoke ok: review YAML replacer only changed decision/rationale/confidence");
