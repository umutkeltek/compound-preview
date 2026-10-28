/**
 * The barrel. This is the only import path anything outside `src/ai-console`
 * may use — never a deep file inside this directory (enforced by
 * `scripts/ai-boundary-smoke.mjs`). See README.md for the full boundary.
 */
export type { AiConsole, ViewContext, ViewProposal } from "./types";
export { stubAiConsole } from "./stub";
export { browserAiConsole } from "./client";
