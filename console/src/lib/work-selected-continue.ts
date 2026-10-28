import { arr, type Json, str } from "@/lib/compound";

export type SelectedContinueView = {
  state: string;
  status: string;
  detail: string;
  actor: string;
  goal: string;
  successPredicate: string;
  contextPackId: string;
  selection: string;
  runAttemptIds: string[];
  canStart: boolean;
  canResume: boolean;
};

export function selectedWorkLease(leases: Json | null, workId: string): Json | null {
  return (arr(leases?.leases).find((entry) => str(entry.work_id) === workId) ?? null) as Json | null;
}

export function selectedContinueView(work: Json, lease: Json | null): SelectedContinueView {
  const state = str(work.state);
  const actor = str(lease?.by);
  const selection = (work.execution_selection ?? {}) as Json;
  const delivery = (work.git_delivery ?? {}) as Json;
  const ids = (work.ids ?? {}) as Json;
  const runAttemptIds = arr(work.run_attempt_ids).map(String);
  const hasSelection = Boolean(str(selection.launch_profile_id) && str(selection.harness));
  const hasWorkspace = Boolean(str(delivery.target_worktree_root));
  const ready = Boolean(actor && hasSelection && hasWorkspace);
  const canStart = state === "claimed" && ready;
  const canResume = state === "verifying" && ready;
  let status = "Needs me";
  let detail = "This work is waiting for a decision or proof; no task command was started from this view.";
  if (state === "claimed") {
    status = ready ? "Ready to run" : "Needs setup";
    detail = ready
      ? "The saved selection, workspace, and current lease holder are present. Execution still requires an authorized client."
      : "A saved selection, workspace, or current lease holder is missing. No run can be admitted yet.";
  } else if (state === "running") {
    status = "Running";
    detail = "The governed run is in progress. Its outcome is not proof of task completion.";
  } else if (state === "verifying") {
    status = "Needs proof review";
    detail = "Execution returned to verification. Review the attempt and proof before closing; a fresh resume is explicit, never automatic.";
  } else if (state === "completed") {
    status = "Completed";
    detail = "Closed as completed. Inspect its receipt and proof trail for the recorded evidence.";
  } else if (state === "failed") {
    status = "Failed";
    detail = "This work was closed as failed. A run result alone did not complete it.";
  }
  const selectedRoute = [str(selection.launch_profile_id), str(selection.harness), str(selection.requested_model_id)]
    .filter(Boolean).join(" · ");
  return {
    state, status, detail, actor,
    goal: str(work.desired_outcome),
    successPredicate: str(work.success_predicate),
    contextPackId: str(ids.context_pack_id),
    selection: selectedRoute,
    runAttemptIds, canStart, canResume,
  };
}

export function selectedContinueRevision(work: Json, lease: Json | null): string {
  const selection = (work.execution_selection ?? {}) as Json;
  return JSON.stringify([
    str(work.state), str(work.policy_decision_id), str(lease?.by), str(lease?.work_sha256),
    selection.revision ?? null, arr(work.run_attempt_ids).map(String),
  ]);
}
