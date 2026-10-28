"use client";

// BAD SHAPE (regression 9c75d29): a client entry VALUE-imports a module whose
// import graph transitively reaches a `node:` builtin. The guard must flag this
// chain: entry-client.tsx -> data-model.ts -> host-bridge.ts -> node:child_process.
import { deriveActs } from "./data-model";

export function EntryClient({ id }: { id: string }) {
  return deriveActs(id).map((act) => act.label).join(",");
}
