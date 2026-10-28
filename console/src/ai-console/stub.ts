import type { AiConsole, ViewContext, ViewProposal } from "./types";

/**
 * Static fixture — deterministic, no network, no LLM, no gateway call. Proves
 * the `ViewProposal` shape resolves through `view-registry.ts` before any real
 * adapter exists. `actionProposals` stays empty: this phase never proposes a
 * mutation, only views.
 */
const FIXTURE_PROPOSAL: ViewProposal = {
  components: [
    { view: "stat-tile", props: { label: "Projection", value: "Unavailable", sub: "No operational value was invented." } },
    { view: "state-badge", props: { state: "unknown" } },
  ],
  actionProposals: [],
};

export const stubAiConsole: AiConsole = {
  proposeView(_query: string, _ctx: ViewContext): Promise<ViewProposal> {
    return Promise.resolve(FIXTURE_PROPOSAL);
  },
};
