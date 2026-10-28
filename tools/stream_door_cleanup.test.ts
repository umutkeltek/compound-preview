import { afterAll, expect, test } from "bun:test";
import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { createTestTempScope } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());
const smoke = resolve(import.meta.dir, "../console/scripts/stream-door-smoke.mjs");

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error: any) {
    if (error.code === "ESRCH") return false;
    throw error;
  }
}

for (const delayedStartup of [false, true]) {
  test(`stream smoke joins owned children after ${delayedStartup ? "late startup" : "ignored cancellation"}`, async () => {
    const workspace = temporary.dir("stream-cleanup-fixture-");
    const roots = join(workspace, "roots");
    const neighbor = join(roots, "stream-door-home-neighbor");
    mkdirSync(neighbor, { recursive: true });
    const sentinel = join(neighbor, "keep");
    writeFileSync(sentinel, "preserved neighbor");
    const witness = join(workspace, "children.jsonl");
    writeFileSync(witness, "");
    const binary = join(workspace, "fixture-compound");
    // One process per follower: no shell/sleep grandchildren to outlive the test.
    const source = `#!${process.execPath}
import { appendFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
const args = process.argv.slice(2);
const record = (event) => appendFileSync(${JSON.stringify(witness)}, JSON.stringify({
  event, pid: process.pid, ppid: process.ppid, args, home: process.env.COMPOUND_HOME,
}) + "\\n");
if (args.includes("--once")) {
  await Bun.sleep(${delayedStartup ? 9000 : 0});
  console.log(JSON.stringify({ cursor: 0, events: [] }));
} else {
  record("started");
  process.on("SIGTERM", () => {
    record("term");
    if (!args.includes("--json") || ${delayedStartup}) process.exit(0);
  });
  let consumed = 0;
  setInterval(() => {
    const lines = readFileSync(join(process.env.COMPOUND_HOME, "events/work.jsonl"), "utf8").trim().split("\\n");
    for (const line of lines.slice(consumed)) {
      if (line) console.log(JSON.stringify({ kind: "work_event", event: JSON.parse(line) }));
    }
    consumed = lines.length;
  }, 10);
}
`;
    writeFileSync(binary, source, { mode: 0o755 });
    const externalHome = join(workspace, "external-home");
    mkdirSync(join(externalHome, "events"), { recursive: true });
    writeFileSync(join(externalHome, "events/work.jsonl"), "");
    const external = spawn(binary, ["stream", "--follow", "--cursor", "1"], {
      env: { ...process.env, COMPOUND_HOME: externalHome }, stdio: "ignore",
    });
    const externalClosed = new Promise<void>((done) => external.once("close", () => done()));
    let smokePID: number | undefined;
    try {
      const deadline = Date.now() + 3000;
      while (!readFileSync(witness, "utf8").includes('"started"') && Date.now() < deadline) {
        await Bun.sleep(20);
      }
      expect(readFileSync(witness, "utf8")).toContain('"started"');
      const result = spawnSync(process.execPath, ["run", smoke], {
        cwd: resolve(import.meta.dir, "../console"),
        env: { ...process.env, TMPDIR: `${roots}/`, COMPOUND_BIN: binary },
        encoding: "utf8", timeout: 15_000, killSignal: "SIGKILL", detached: true,
      });
      smokePID = result.pid;
      expect(result.error).toBeUndefined();
      expect(result.status).toBe(1);
      expect(result.stderr).toContain(delayedStartup
        ? "appended event never streamed"
        : "follow child survived client disconnect");
      const records = readFileSync(witness, "utf8").trim().split("\n").map((line) => JSON.parse(line));
      // A late child can be killed before its script writes a startup marker.
      // Use the real ChildProcess launch identities, then independently probe.
      const joined = result.stdout.split("\n").find((line) => line.startsWith("stream-door-smoke: owned children closed "));
      expect(joined).toBeDefined();
      const followers = JSON.parse(joined!.slice(joined!.indexOf("[")));
      expect(followers.length).toBe(2); // smoke decoy and the real route child
      expect(followers.some((row: any) => row.route)).toBe(true);
      for (const row of followers) expect(alive(row.pid)).toBe(false);
      if (!delayedStartup) {
        expect(records.some((row) => row.event === "term" && row.args.includes("--json"))).toBe(true);
      }
      expect(readdirSync(roots)).toEqual(["stream-door-home-neighbor"]);
      expect(readFileSync(sentinel, "utf8")).toBe("preserved neighbor");
      expect(readFileSync(binary, "utf8")).toBe(source);
      expect(alive(external.pid!)).toBe(true);
    } finally {
      // Also contain a deliberately broken smoke in the counterfactual run.
      // The smoke alone owns this detached process group; external is outside it.
      try {
        if (smokePID) {
          try { process.kill(-smokePID, "SIGKILL"); }
          catch (error: any) { if (error.code !== "ESRCH") throw error; }
          const deadline = Date.now() + 2000;
          while (spawnSync("pgrep", ["-g", String(smokePID)]).status === 0 && Date.now() < deadline) {
            await Bun.sleep(20);
          }
          expect(spawnSync("pgrep", ["-g", String(smokePID)]).status).toBe(1);
        }
      } finally {
        external.kill("SIGKILL");
        await externalClosed;
      }
    }
  }, 25_000);
}
