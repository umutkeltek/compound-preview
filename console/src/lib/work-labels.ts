export function formatBlockedRef(ref: string): string {
  const [kind, id = ""] = ref.split(":");
  const workId = id.trim();
  switch (kind) {
    case "work.run_evidence":
      return workId ? `run evidence for ${workId}` : "run evidence";
    case "work.close_proof":
      return workId ? `close proof ${workId}` : "close proof";
    case "work.review":
      return workId ? `review for ${workId}` : "review";
    case "work.claimed_by_other_actor":
      return workId ? `another executor on ${workId}` : "another executor";
    case "work.ready_state":
      return workId ? `preparation for ${workId}` : "preparation";
    case "work.admission_preview":
      return workId ? `suggested task ${workId}` : "suggested task";
    case "work.terminal":
      return workId ? `terminal work ${workId}` : "terminal work";
    case "work.ready_or_source_packet":
      return "ready work or source packet";
    case "work.target_changed":
      return workId ? `changed target ${workId}` : "changed target";
    default:
      return ref.replace(/[_.:]/g, " ");
  }
}

export function shortBlockedRefs(refs?: string[], limit = 3): string {
  if (!refs?.length) return "";
  const shown = refs.slice(0, limit).map(formatBlockedRef);
  const more = refs.length - shown.length;
  return more > 0 ? `${shown.join(" / ")} / +${more} more` : shown.join(" / ");
}
