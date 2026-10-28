"use client";

// BAD SHAPE: the client entry itself contains no capability identifier, but
// its value-import graph reaches one in capability-bridge.ts. A direct-entry
// scan misses this leak; the graph guard must reject it.
import { capabilityHeaderName } from "./capability-bridge";

export function EntryClient() {
  return capabilityHeaderName;
}
