import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildPreflightReport } from "./second_opinion_preflight.ts";

function makeSession(meta: Record<string, unknown>, output = "Session started\n"): { root: string; id: string } {
  const root = mkdtempSync(join(tmpdir(), "compound-second-opinion-sessions-"));
  const id = "fixture-second-opinion";
  const dir = join(root, id);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "meta.json"), `${JSON.stringify(meta, null, 2)}\n`);
  writeFileSync(join(dir, "output.log"), output);
  return { root, id };
}

describe("second_opinion_preflight edge", () => {
  test("warns for running session without saved answer", () => {
    const session = makeSession({
      status: "running",
      model: "frontier-pro",
      mode: "browser",
      createdAt: "2026-07-04T00:00:00Z",
      conversationUrl: null,
      response: null,
      error: null,
    });

    const report = buildPreflightReport({
      sessionID: session.id,
      sessionsRoot: session.root,
      answerPath: join(session.root, "answer.md"),
      sourceObservationPath: join(session.root, "source-observation.yaml"),
    });

    expect(report.status).toBe("warn");
    expect(report.session.exists).toBe(true);
    expect(report.session.status).toBe("running");
    expect(report.session.has_response).toBe(false);
    expect(report.answer.exists).toBe(false);
    expect(report.checks.find((check) => check.id === "answer_file")?.next_action).toContain("reattach");
    const harvest = report.actions.find((action) => action.id === "harvest_answer");
    expect(harvest?.requires_operator).toBe(true);
    expect(harvest?.stop_conditions).toContain("no live tab");
    const sourceObserve = report.actions.find((action) => action.id === "import_no_promote_evidence");
    expect(sourceObserve?.command).toContain("compound source observe");
    expect(sourceObserve?.boundary).toContain("NoPromoteDecision");
  });

  test("marks answer and source observation path ready without reading answer body", () => {
    const session = makeSession(
      {
        status: "completed",
        model: "frontier-pro",
        mode: "browser",
        createdAt: "2026-07-04T00:00:00Z",
        updatedAt: "2026-07-04T00:10:00Z",
        conversationUrl: "https://example.invalid/c/fixture",
        response: { id: "response-fixture" },
        error: null,
      },
      "Answer: saved elsewhere\n",
    );
    const answerPath = join(session.root, "private-answer.md");
    const sourceObservationPath = join(session.root, "source-observation.yaml");
    writeFileSync(answerPath, "private answer body should not be surfaced by preflight\n");
    writeFileSync(sourceObservationPath, "schema_version: compound.source_observation.v0.1\n");

    const report = buildPreflightReport({
      sessionID: session.id,
      sessionsRoot: session.root,
      answerPath,
      sourceObservationPath,
    });

    expect(report.status).toBe("ok");
    expect(report.session.has_conversation_url).toBe(true);
    expect(report.session.has_response).toBe(true);
    expect(report.session.output_has_answer_marker).toBe(true);
    expect(report.answer.exists).toBe(true);
    expect(report.answer.bytes).toBeGreaterThan(0);
    expect(JSON.stringify(report)).not.toContain("private answer body");
    expect(report.checks.find((check) => check.id === "source_observation")?.status).toBe("ok");
  });

  test("fails when session directory is missing", () => {
    const root = mkdtempSync(join(tmpdir(), "compound-second-opinion-missing-"));
    const report = buildPreflightReport({ sessionID: "missing", sessionsRoot: root });

    expect(report.status).toBe("fail");
    expect(report.session.exists).toBe(false);
    expect(report.checks.find((check) => check.id === "session_dir")?.status).toBe("fail");
  });
});
