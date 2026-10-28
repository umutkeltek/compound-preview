"use client";

// GOOD SHAPE (the 9c75d29 fix): a client entry that only `import type`s the
// server-only module. A type-only import is erased at build time and is NOT a
// value edge, so this entry must NOT be flagged — it proves the guard
// discriminates type imports from value imports.
import type { ActItem } from "./data-model";

export function SafeClient({ item }: { item: ActItem }) {
  return item.label;
}
