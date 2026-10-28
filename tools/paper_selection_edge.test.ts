import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import {
  buildPaperSelectionExport,
  normalizeHFCandidates,
  normalizeSemanticCandidates,
} from "./paper_selection_edge.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

function buildCompound(): string {
  const bin = join(temporary.dir("compound-paper-selection-bin-"), "compound");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], {
    cwd: process.cwd(),
    encoding: "utf8",
    timeout: 120_000,
  });
  if (build.status !== 0) throw new Error(build.stderr || build.stdout);
  return bin;
}

// Keep compilation in setup, outside the per-test timeout, so failed builds get teardown.
let COMPOUND_BIN: string;
beforeAll(() => {
  COMPOUND_BIN = buildCompound();
}, 120_000);

const hfFixture = [
  {
    paper: {
      id: "2607.00001",
      title: "Agent Memory Systems for Tool Use",
      summary: "A fresh daily paper about agents using memory and tools in long-running tasks.",
      ai_keywords: ["agent memory", "tool use", "planning"],
      authors: [{ name: "Ada Agent" }],
      publishedAt: "2026-07-03T00:00:00.000Z",
      submittedOnDailyAt: "2026-07-04T00:00:00.000Z",
      upvotes: 20,
    },
  },
  {
    paper: {
      id: "2607.00002",
      title: "Video Foundation Models for Robotics",
      summary: "A daily paper connecting video foundation models with embodied robot control.",
      ai_keywords: ["video", "robotics", "foundation models"],
      authors: [{ name: "Rhea Robot" }],
      publishedAt: "2026-07-02T00:00:00.000Z",
    },
  },
];

const semanticFixture = {
  data: [
    {
      paperId: "s2-attention",
      externalIds: { ArXiv: "1706.03762" },
      title: "Attention Is All You Need",
      abstract: "The Transformer replaces recurrence with attention and became a foundation for language models.",
      authors: [{ name: "A. Vaswani" }],
      publicationDate: "2017-06-12",
      citationCount: 100000,
      fieldsOfStudy: ["Computer Science"],
      url: "https://www.semanticscholar.org/paper/s2-attention",
    },
    {
      paperId: "s2-second-language",
      title: "Another Transformer Paper",
      abstract: "A second language model paper that should lose to the diversity cap.",
      authors: [{ name: "L. Model" }],
      year: 2019,
      citationCount: 90000,
      fieldsOfStudy: ["Computer Science"],
    },
    {
      paperId: "s2-social",
      title: "Resilience in Social Systems",
      abstract: "A highly cited non-computer-science paper that keeps the batch diverse.",
      authors: [{ name: "C. Systems" }],
      publicationDate: "2006-08-01",
      citationCount: 7400,
      fieldsOfStudy: ["Economics", "Sociology"],
    },
  ],
};

describe("paper_selection_edge", () => {
  test("normalizes both rented paper sources into scored candidates", () => {
    const hf = normalizeHFCandidates(hfFixture);
    const semantic = normalizeSemanticCandidates(semanticFixture);

    expect(hf).toHaveLength(2);
    expect(hf[0].sourceLane).toBe("trending");
    expect(hf[0].ref).toBe("arxiv:2607.00001");
    expect(semantic).toHaveLength(3);
    expect(semantic[0].sourceLane).toBe("fundamental");
    expect(semantic[0].fundamentalScore).toBe(1);
  });

  test("emits a valid knowledge export with score and diversity outside the core schema", () => {
    const report = buildPaperSelectionExport(hfFixture, semanticFixture, {
      topic: "agent memory",
      maxItems: 4,
      maxPerCategory: 1,
      capturedAt: "2026-07-04T00:00:00Z",
    });

    expect(report.export.schema_version).toBe("compound.knowledge_export.v0.1");
    expect(report.export.source).toEqual({
      kind: "paper-selection",
      ref: "topic:agent-memory",
      captured_at: "2026-07-04T00:00:00Z",
    });
    expect(report.export.producer.name).toBe("paper-selection-edge");
    expect(report.export.items).toHaveLength(4);
    expect(report.export.items.map((item) => item.ref)).toContain("arxiv:1706.03762");
    expect(report.export.items.map((item) => item.ref)).not.toContain("semanticscholar:s2-second-language");
    for (const item of report.export.items) {
      expect(Object.keys(item).sort()).toEqual(
        ["authors", "content", "lang", "published_at", "ref", "title", "topics", "url"].sort(),
      );
      expect(item.content.length).toBeGreaterThan(20);
      expect(item.topics).toContain("agent memory");
    }
  expect(report.selected[0]).toHaveProperty("score");
  expect(report.selected[0]).toHaveProperty("primary_category");
});

test("cli export imports idempotently through generic Compound knowledge import", () => {
  const root = temporary.dir("compound-paper-selection-test-");
  const hfPath = join(root, "hf-daily.json");
  const semanticPath = join(root, "semantic-scholar.json");
  const exportPath = join(root, "paper-selection-export.json");
  const homePath = join(root, "compound-home");

  writeFileSync(hfPath, JSON.stringify(hfFixture), "utf8");
  writeFileSync(semanticPath, JSON.stringify(semanticFixture), "utf8");

  initializeCompoundHome(COMPOUND_BIN, homePath);

  const edge = spawnSync(
    "bun",
    [
      "tools/paper_selection_edge.ts",
      "--topic",
      "agent memory",
      "--hf-json",
      hfPath,
      "--semantic-json",
      semanticPath,
      "--out",
      exportPath,
      "--max",
      "4",
      "--max-per-category",
      "1",
      "--captured-at",
      "2026-07-04T00:00:00Z",
    ],
    { cwd: process.cwd(), encoding: "utf8" },
  );
  expect(edge.status).toBe(0);
  const edgeReport = JSON.parse(edge.stdout);
  expect(edgeReport.status).toBe("ok");
  expect(edgeReport.items).toBe(4);

  const exported = JSON.parse(readFileSync(exportPath, "utf8"));
  expect(exported.schema_version).toBe("compound.knowledge_export.v0.1");
  expect(exported.source.kind).toBe("paper-selection");
  expect(exported.privacy).toBe("public");
  expect(exported.items).toHaveLength(4);

  const first = spawnSync(COMPOUND_BIN, ["knowledge", "import", exportPath, "--json"], {
    cwd: process.cwd(),
    encoding: "utf8",
    env: { ...process.env, COMPOUND_HOME: homePath },
  });
  expect(first.status).toBe(0);
  const firstReport = JSON.parse(first.stdout);
  expect(firstReport.items).toBe(4);
  expect(firstReport.imported).toBe(4);
  expect(firstReport.extraction_ids).toHaveLength(4);
  expect(firstReport.candidate_ids.length).toBeGreaterThan(4);

  const second = spawnSync(COMPOUND_BIN, ["knowledge", "import", exportPath, "--json"], {
    cwd: process.cwd(),
    encoding: "utf8",
    env: { ...process.env, COMPOUND_HOME: homePath },
  });
  expect(second.status).toBe(0);
  const secondReport = JSON.parse(second.stdout);
  expect(secondReport.items).toBe(4);
  expect(secondReport.imported).toBe(0);
  expect(secondReport.reused).toBe(4);
  expect(secondReport.extraction_ids).toHaveLength(0);
});
});
