import fs from "node:fs";
import path from "node:path";

export function runAskTriageSelfTest(): { ok: boolean; details: string[] } {
  const root = path.resolve(process.cwd(), "src/components/agents");
  const files = fs.readdirSync(root).filter((name) => name.endsWith(".tsx"));
  const forbidden = /needs[ -]you|needs your|waiting on you/i;
  const leaks = files.flatMap((name) => fs.readFileSync(path.join(root, name), "utf8").split("\n").flatMap((line, i) => forbidden.test(line) ? [`${name}:${i + 1}`] : []));
  return { ok: leaks.length === 0, details: leaks.length ? leaks : ["agents components expose lane health, not pager badges"] };
}
