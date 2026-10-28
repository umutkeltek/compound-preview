import fs from "node:fs";

const files = [
  "src/app/work/page.tsx",
  "src/components/briefing/PipelineStrip.tsx",
  "src/components/work/PipelineLens.tsx",
];
const residue = /\b(?:admit|admission|governed|policydecision|envelope(?:s| spine| trace)?|rented executors?)\b/i;
const failures = files.flatMap((file) => {
  const text = fs.readFileSync(file, "utf8");
  return text.split("\n").flatMap((line, index) =>
    residue.test(line) ? [`${file}:${index + 1}: ${line.trim()}`] : [],
  );
});

if (failures.length) {
  console.error("work-language-smoke FAILED\n" + failures.join("\n"));
  process.exit(1);
}
console.log("work-language-smoke ok: work surfaces use operator language only");
