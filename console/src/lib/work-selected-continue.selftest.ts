import { specFor } from "@/lib/decision-registry";
import type { Json } from "@/lib/compound";
import { selectedContinueRevision, selectedContinueView, selectedWorkLease } from "@/lib/work-selected-continue";

export function runSelectedContinueSelfTest(): { ok: boolean; details: string[] } {
  const details: string[] = [];
  const check = (name: string, pass: boolean) => details.push(`${pass ? "ok" : "FAIL"} ${name}`);
  const id = "work_selected_fixture";
  const lease = { work_id: id, by: "actor:exact", work_sha256: "sha256:lease" };
  const base: Json = {
    id, state: "claimed", desired_outcome: "Repair the task", success_predicate: "Proof accepted",
    ids: { context_pack_id: "ctx_fixture" },
    git_delivery: { target_worktree_root: "/fixture" },
    execution_selection: { launch_profile_id: "codex", harness: "codex", revision: 2 },
    policy_decision_id: "pol_fixture", run_attempt_ids: [], selected_lease: lease,
  };
  const spec = specFor("work-selected-continue");
  if (!spec?.optionsFor || !spec.buildArgv || !spec.revisionFor) return { ok: false, details: ["FAIL registry entry missing"] };
  const options = spec.optionsFor(base);
  const argv = spec.buildArgv({ itemId: id, item: base, option: "continue", fields: {} });
  check("claimed exact work and server lease actor", options.length === 1 && options[0].value === "continue" &&
    JSON.stringify(argv) === JSON.stringify(["work", "continue", "--selected", "--expect-work", id, "--actor", "actor:exact", "--json"]));
  const verifying = { ...base, state: "verifying", run_attempt_ids: ["ratt_prior"] };
  const resumeOptions = spec.optionsFor(verifying);
  const resumeArgv = spec.buildArgv({ itemId: id, item: verifying, option: "resume", fields: { actor: "attacker" } });
  check("verifying requires explicit resume and ignores client actor", resumeOptions.length === 1 &&
    resumeOptions[0].value === "resume" && resumeArgv?.includes("--resume") === true &&
    resumeArgv?.includes("actor:exact") === true && !resumeArgv?.includes("attacker") &&
    spec.buildArgv({ itemId: id, item: verifying, option: "continue", fields: {} }) === null);
  const noLease = { ...base, selected_lease: null };
  check("missing lease offers no launch", spec.optionsFor(noLease).length === 0 &&
    spec.buildArgv({ itemId: id, item: noLease, option: "continue", fields: {} }) === null);
  const noSelection = { ...base, execution_selection: null };
  check("missing saved selection offers no launch", spec.optionsFor(noSelection).length === 0);
  const running = selectedContinueView({ ...base, state: "running" }, lease);
  const needsProof = selectedContinueView(verifying, lease);
  check("status does not conflate run and completion", running.status === "Running" &&
    needsProof.status === "Needs proof review" && needsProof.detail.includes("never automatic"));
  const completed = selectedContinueView({ ...base, state: "completed" }, lease);
  check("completed view points to proof without attesting it", completed.status === "Completed" &&
    completed.detail.includes("Inspect its receipt") && !completed.detail.includes("proof transition"));
  check("lease lookup exact and drift-sensitive", selectedWorkLease({ leases: [lease] }, id)?.by === "actor:exact" &&
    selectedWorkLease({ leases: [lease] }, "work_other") === null &&
    selectedContinueRevision(base, lease) !== selectedContinueRevision(base, { ...lease, by: "actor:changed" }));
  return { ok: details.every((line) => line.startsWith("ok")), details };
}
