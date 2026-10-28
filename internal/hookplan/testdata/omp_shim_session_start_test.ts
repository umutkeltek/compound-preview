import compoundHook from "../omp_hook_shim.ts";
import { writeFileSync } from "node:fs";
import { homedir } from "node:os";
const handlers = new Map<string, (payload?: unknown) => unknown>();
compoundHook({ on: (event, handler) => handlers.set(event, handler) });
const start = handlers.get("session_start");
const before = handlers.get("before_agent_start");
if (!start || !before) throw new Error("lifecycle handlers missing");
function respond(verb: string, ctx: string | null): void {
  const doc = ctx === null ? {} : { hookSpecificOutput: { hookEventName: "X", additionalContext: ctx } };
  writeFileSync(homedir() + "/" + verb + ".json", JSON.stringify(doc) + "\n");
}
const doorMap = "## Door map (cached, not live)\nPASS web: search-cli \"<q>\"";
respond("session-start", doorMap);
respond("user-prompt-submit", "prompt pack");
await start();
let r: any = await before({ prompt: "first" });
if (typeof r?.message !== "string" || !r.message.startsWith(doorMap) || !r.message.endsWith("prompt pack")) {
  throw new Error("first message lost the session-start door map: " + JSON.stringify(r));
}
r = await before({ prompt: "second" });
if (r?.message !== "prompt pack") throw new Error("door map repeated or prompt pack lost: " + JSON.stringify(r));
respond("user-prompt-submit", null);
await start();
r = await before({ prompt: "no pack" });
if (r?.message !== doorMap) throw new Error("door map alone did not reach the first message: " + JSON.stringify(r));
r = await before({ prompt: "nothing" });
if (r !== undefined) throw new Error("empty context produced a message: " + JSON.stringify(r));
