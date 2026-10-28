import type { AiConsole, ViewContext, ViewProposal } from "./types";

/** Browser adapter seam. A future Tambo adapter can replace this transport; it still returns registry references only. */
export const browserAiConsole: AiConsole = {
  async proposeView(query: string, ctx: ViewContext): Promise<ViewProposal> {
    const response = await fetch("/api/ask", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ query, route: ctx.route }),
    });
    if (!response.ok) throw new Error(`Ask Compound failed (${response.status})`);
    return response.json() as Promise<ViewProposal>;
  },
};
