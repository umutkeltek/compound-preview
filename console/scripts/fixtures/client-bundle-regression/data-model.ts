// VALUE-imports the server-only helper, so this module's graph reaches a
// `node:` builtin. Mirrors the real `@/lib/decide-model` node in regression
// 9c75d29 (the module that value-imported `@/lib/compound`).
import { runHostCommand } from "./host-bridge";

export type ActItem = { id: string; label: string };

export function deriveActs(id: string): ActItem[] {
  void runHostCommand;
  return [{ id, label: `act:${id}` }];
}
