/**
 * Types for the ai-console sandbox (Plans/i-am-not-happy-rosy-floyd.md Part 6 / Phase 5).
 * Doctrine: the LLM composes the view; the registry (src/lib/view-registry.ts) owns
 * the act. Nothing here mutates anything — `actionProposals` render as normal
 * governed DecisionCards a human clicks through `/api/decision/execute`; this
 * boundary never calls that endpoint itself.
 */

/** What the caller knows about where the proposal will render. */
export type ViewContext = {
  route: string;
  freshness?: string;
};

/**
 * What an ai-console adapter hands back: view invocations that resolve against
 * `view-registry.ts` (never raw components), and action proposals that resolve
 * against the existing decision-registry — both are references, not authority.
 */
export type ViewProposal = {
  components: Array<{ view: string; props: unknown }>;
  actionProposals: Array<{ area: string; itemId: string; actionId: string }>;
};

/** The one interface any future real adapter implements. v1 ships only `stubAiConsole`. */
export interface AiConsole {
  proposeView(query: string, ctx: ViewContext): Promise<ViewProposal>;
}
