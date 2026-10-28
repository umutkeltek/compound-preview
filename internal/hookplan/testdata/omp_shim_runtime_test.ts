import compoundHook from "../omp_hook_shim.ts";
import { writeFileSync } from "node:fs";
import { homedir } from "node:os";
const handlers = new Map<string, (payload?: unknown) => unknown>();
compoundHook({ on: (event, handler) => handlers.set(event, handler) });
const toolCall = handlers.get("tool_call");
if (!toolCall) throw new Error("tool_call handler missing");
function response(document: unknown, status = 0): void {
  writeFileSync(homedir()+"/response.json", JSON.stringify(document)+"\n");
  writeFileSync(homedir()+"/exit-status", String(status)+"\n");
}
async function invoke(): Promise<any> { return await toolCall({toolName:"Bash",toolCallId:"runtime",input:{command:"true"}}); }
function pre(nested: Record<string,unknown>, top: Record<string,unknown> = {}): unknown {
  return {...top,hookSpecificOutput:{hookEventName:"PreToolUse",...nested}};
}
response(pre({})); if ((await invoke()) !== undefined) throw new Error("omitted allow blocked");
response(pre({permissionDecision:"allow"})); if ((await invoke()) !== undefined) throw new Error("allow blocked");
response(pre({permissionDecision:"deny",permissionDecisionReason:"nested deny"}),3);
let result=await invoke(); if(!result?.block||result.reason!=="nested deny") throw new Error("deny reason lost");
for(const invalid of [false,"",0,null,{},[]]) {
  response(pre({permissionDecision:invalid})); result=await invoke();
  if(!result?.block) throw new Error("invalid present decision allowed "+JSON.stringify(invalid));
}
response(pre({permissionDecision:"deny",permissionDecisionReason:"canonical deny"},{permissionDecision:"allow"}));
result=await invoke(); if(!result?.block||result.reason!=="canonical deny") throw new Error("top allow overrode deny");
response(pre({permissionDecision:"allow"},{permissionDecision:"deny",permissionDecisionReason:"top deny"}));
result=await invoke(); if(!result?.block||result.reason!=="top deny") throw new Error("nested allow overrode deny");
response(pre({permissionDecision:"deny",permissionDecisionReason:"canonical deny"},{permissionDecision:false}));
result=await invoke(); if(!result?.block||result.reason!=="canonical deny") throw new Error("invalid projection overrode deny");
response(pre({permissionDecision:"allow"}),3); result=await invoke();
if(!result?.block||result.reason!=="compound policy gate unavailable") throw new Error("nonzero allow accepted");
