/**
 * Client-safe operator-input specs.
 *
 * This module is imported by client components (the Decide confirm dialog), so
 * it must stay free of server-only imports. It lives apart from
 * generic-actions.ts for exactly that reason: that module resolves and executes
 * actions and therefore imports @/lib/compound, which reaches node:child_process.
 * The specs themselves are inert data — a flag name and its prompt copy — and a
 * client needs them to render the input, so they own their own boundary.
 */

/**
 * Areas whose CLI REQUIRES --rationale: the backend deliberately omits it
 * from the projected argv so no placeholder text can ever run. The operator
 * supplies the rationale in the confirmation UI; it is appended as its own
 * argv element (["--rationale", <text>]) — an exec argv array, never a
 * shell string, so arbitrary rationale text is safe by construction.
 * Resolution REJECTS ("rationale-required") when the operator supplied none.
 */
export type OperatorInputSpec = {
  /** The CLI flag the operator's text is appended as, e.g. "--rationale". */
  flag: string;
  label: string;
  placeholder: string;
};

/**
 * Areas whose CLI REQUIRES an operator-written input the projected argv must
 * never carry (a pre-baked placeholder must not be runnable). The operator
 * supplies the text in the confirmation UI; it is appended as its own argv
 * element ([flag, text]) — an exec argv array, never a shell string, so
 * arbitrary operator text is safe by construction. Resolution REJECTS when
 * the operator supplied none.
 */
export const OPERATOR_INPUTS: Record<string, OperatorInputSpec> = {
  "route-promotions": { flag: "--rationale", label: "Rationale", placeholder: "Why is this the right reviewed action? This becomes decision provenance." },
  "panel-verdicts": { flag: "--rationale", label: "Rationale", placeholder: "Why is this the right reviewed action? This becomes decision provenance." },
  "harness-edit-proposals": { flag: "--rationale", label: "Rationale", placeholder: "Why is this the right reviewed action? This becomes decision provenance." },
  // The hypothesis approve verb refuses the "OPERATOR MUST WRITE" placeholder
  // and requires the operator's own expected-effect sentence; the projected
  // argv (internal/product/review_queue.go productHypothesisApproveAction)
  // never carries it.
  "product-hypotheses": { flag: "--expected-effect", label: "Expected effect", placeholder: "What should the metric do after the change? Your sentence becomes the hypothesis." },
};

/** Legacy view of OPERATOR_INPUTS: the areas whose operator input is --rationale. */
export const RATIONALE_REQUIRED_AREAS: ReadonlySet<string> = new Set(
  Object.entries(OPERATOR_INPUTS)
    .filter(([, spec]) => spec.flag === "--rationale")
    .map(([area]) => area),
);
