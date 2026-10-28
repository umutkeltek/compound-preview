import { describe, expect, test } from "bun:test";
import { STATUSES, backupGateAllowsApply, type Status } from "./check_backup_freshness";

describe("backupGateAllowsApply", () => {
  test("only a fresh receipt clears a destructive apply", () => {
    expect(backupGateAllowsApply("fresh")).toBe(true);
    expect(backupGateAllowsApply("stale")).toBe(false);
    expect(backupGateAllowsApply("missing")).toBe(false);
  });

  test("every status the checker can emit has a defined verdict", () => {
    // If someone adds a Status, this forces them to decide its verdict here
    // rather than letting the gate fall through to "refuse".
    const verdicts = new Map<Status, boolean>(STATUSES.map((s) => [s, backupGateAllowsApply(s)]));
    expect([...verdicts.keys()].sort()).toEqual(["fresh", "missing", "stale"]);
    expect([...verdicts.values()].filter(Boolean)).toHaveLength(1);
  });

  test("refuses a status the checker never emits", () => {
    // The original bug: the gate compared against "ok", so a fresh backup
    // read as not-ok and every apply run aborted while logging green.
    expect(backupGateAllowsApply("ok")).toBe(false);
    expect(backupGateAllowsApply("")).toBe(false);
  });
});
