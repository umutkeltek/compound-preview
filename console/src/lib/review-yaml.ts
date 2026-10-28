export const REVIEW_DECISIONS = [
  "keep_needs_review",
  "merge_existing",
  "reference_only",
  "reject",
] as const;

export type ReviewDecision = (typeof REVIEW_DECISIONS)[number];

export type ReviewDraftFields = {
  decision: string;
  rationale: string;
  confidence: string;
};

export function isReviewDecision(value: unknown): value is ReviewDecision {
  return typeof value === "string" && REVIEW_DECISIONS.includes(value as ReviewDecision);
}

function readScalar(source: string, field: keyof ReviewDraftFields): string {
  const match = source.match(new RegExp(`^${field}:\\s*(.*)$`, "m"));
  if (!match) return "";
  const raw = match[1].trim();
  if (raw.startsWith("\"") && raw.endsWith("\"")) {
    try {
      return JSON.parse(raw);
    } catch {
      return raw.slice(1, -1);
    }
  }
  if (raw.startsWith("'") && raw.endsWith("'")) return raw.slice(1, -1).replace(/''/g, "'");
  return raw;
}

export function parseReviewDraftYaml(source: string): ReviewDraftFields {
  return {
    decision: readScalar(source, "decision"),
    rationale: readScalar(source, "rationale"),
    confidence: readScalar(source, "confidence"),
  };
}

function quoteYamlString(value: string): string {
  return JSON.stringify(value);
}

function formatConfidence(value: number): string {
  if (!Number.isFinite(value)) throw new Error("confidence must be a finite number");
  return value.toFixed(2);
}

export function replaceReviewDecisionFields(
  source: string,
  fields: { decision: ReviewDecision; rationale: string; confidence: number },
): string {
  const newline = source.includes("\r\n") ? "\r\n" : "\n";
  const lines = source.split(/\r?\n/);
  const seen = { decision: false, rationale: false, confidence: false };
  const rationale = fields.rationale.trim();

  const out = lines.map((line) => {
    const decisionMatch = line.match(/^(\s*)decision:\s*.*$/);
    if (decisionMatch) {
      seen.decision = true;
      return `${decisionMatch[1]}decision: ${fields.decision}`;
    }

    const rationaleMatch = line.match(/^(\s*)rationale:\s*.*$/);
    if (rationaleMatch) {
      seen.rationale = true;
      return `${rationaleMatch[1]}rationale: ${quoteYamlString(rationale)}`;
    }

    const confidenceMatch = line.match(/^(\s*)confidence:\s*.*$/);
    if (confidenceMatch) {
      seen.confidence = true;
      return `${confidenceMatch[1]}confidence: ${formatConfidence(fields.confidence)}`;
    }

    return line;
  });

  const missing = Object.entries(seen)
    .filter(([, ok]) => !ok)
    .map(([field]) => field);
  if (missing.length) throw new Error(`review YAML missing field(s): ${missing.join(", ")}`);

  return out.join(newline);
}
