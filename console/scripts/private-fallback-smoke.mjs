import assert from "node:assert/strict";

const oldHome = process.env.COMPOUND_HOME;
const oldFallback = process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS;

try {
  process.env.COMPOUND_HOME = "/tmp/compound-private-fallback-smoke";
  delete process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS;

  const { privateFallbacksEnabled, reviewCommand, reviewSentence } =
    await import("../src/lib/compound.ts");
  const row = { area: "external-repo-intake", id: "abc123" };

  assert.equal(privateFallbacksEnabled(), false);
  assert.equal(reviewSentence(row), "External repo awaits a projected review action.");
  assert.equal(reviewCommand(row), "compound reviews --area external-repo-intake");
  assert.equal(reviewCommand(row).includes(process.env.COMPOUND_HOME), false);

  process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS = "1";
  assert.equal(privateFallbacksEnabled(), false, "private reads stay deferred even when the legacy flag is set");
  assert.equal(reviewCommand(row), "compound reviews --area external-repo-intake");
  assert.equal(reviewCommand(row).includes(process.env.COMPOUND_HOME), false);

  const { GET } = await import("../src/app/api/document/route.ts");
  const response = await GET();
  assert.equal(response.status, 403, "document API must remain unavailable");
  const body = await response.json();
  assert.equal(body.ok, false);
  assert.match(body.stderr, /direct document reads are unavailable/);
  assert.equal("content" in body, false, "deferred API must never return private bytes");
} finally {
  if (oldHome === undefined) delete process.env.COMPOUND_HOME;
  else process.env.COMPOUND_HOME = oldHome;

  if (oldFallback === undefined) delete process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS;
  else process.env.COMPOUND_CONSOLE_PRIVATE_FALLBACKS = oldFallback;
}

console.log("private fallback smoke ok: direct private reads remain unavailable");
