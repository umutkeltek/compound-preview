import { headers } from "next/headers";

import { INSTANCE_PIN } from "@/lib/instance-pin";

/**
 * Always-visible instance identity (home leaf + short home hash + serving port)
 * so an operator/playtester can always tell WHICH compound instance they are
 * driving. This is the visible half of the 2026-07-11 origin-pin fix: a scratch
 * clone on another port reads distinctly from the canonical console, and an
 * instance that was started without an explicit home pin (which cannot mutate)
 * is flagged in the danger tone rather than masquerading as the real home.
 *
 * Server component: the port is read from the request Host header, so the badge
 * reflects the port the browser is actually talking to, not a build-time guess.
 */
export async function InstanceBadge() {
  const host = (await headers()).get("host") ?? "";
  const port = host.includes(":") ? host.slice(host.lastIndexOf(":") + 1) : "";
  const { homeBasename, homeHash, home, explicit, buildKind } = INSTANCE_PIN;

  const tone = explicit
    ? "border-hair bg-soft text-muted"
    : "border-crit-line bg-crit-soft text-crit";
  const title = explicit
    ? `serving instance — home ${home}${port ? ` · port ${port}` : ""}`
    : `UNPINNED instance — this server was started without an explicit home and would fall back to ${home}; mutations are refused. Launch via 'compound console --operator'.`;

  return (
    <span
      title={title}
      className={`inline-flex shrink-0 items-center gap-1 rounded-pill border px-2 py-0.5 text-2xs font-semibold tabular-nums ${tone}`}
      data-instance-home-hash={homeHash}
      data-instance-explicit={explicit ? "1" : "0"}
      data-instance-build-kind={buildKind}
    >
      <span
        aria-hidden
        className={`h-1.5 w-1.5 rounded-full ${explicit ? "bg-good" : "bg-crit"}`}
      />
      <span className="max-w-[9rem] truncate">{homeBasename}</span>
      <span className="text-faint">#{homeHash}</span>
      {port ? <span className="text-faint">:{port}</span> : null}
      {explicit ? null : <span className="uppercase">unpinned</span>}
    </span>
  );
}
