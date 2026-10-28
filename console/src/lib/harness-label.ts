/** Preserve raw RunAttempt evidence while making fixture execution clear. */
export function harnessDisplayName(raw: string): string {
  if (!raw) return "Execution detail unavailable";
  if (raw.toLowerCase() === "fake") return "Fixture adapter";
  return raw.replace(/[-_]+/g, " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
}
