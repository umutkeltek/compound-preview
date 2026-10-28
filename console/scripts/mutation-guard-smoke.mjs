import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";

import {
  CONSOLE_CAPABILITY_ENV,
  CONSOLE_CAPABILITY_HEADER,
  CONSOLE_MUTATIONS_ENV,
  consoleMutationBlockedResponse,
} from "../src/lib/console-mutations.ts";
import { consoleChildEnv } from "../src/lib/console-child-env.ts";

const LOCAL_ORIGIN = "http://localhost:3000";
const FORMAT_VALID_CAPABILITY = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const LEGACY_COOKIE = "compound_console_session_token";

function request(origin, host, token, options = {}) {
  const headers = new Headers();
  if (origin) headers.set("origin", origin);
  if (host) headers.set("host", host);
  if (token) headers.set(CONSOLE_CAPABILITY_HEADER, token);
  if (options.cookie) {
    headers.set("cookie", `${LEGACY_COOKIE}=${encodeURIComponent(options.cookie)}`);
  }
  if (options.fetchSite) headers.set("sec-fetch-site", options.fetchSite);
  if (options.body) headers.set("content-type", "application/json");
  return new Request(options.url ?? new URL("/api/decision/execute", LOCAL_ORIGIN), {
    method: options.method ?? "POST",
    headers,
    body: options.body,
  });
}

/**
 * Same POST /api/decision/execute request the bespoke path uses, except the
 * body is generic-pathway-shaped ({area, itemId, actionId, confirmed} — see
 * generic-actions.ts / decision-execute.ts's executeGenericAction). The
 * guard is body-shape-agnostic. The real route ordering is exercised by
 * work-mutation-trail-smoke.mjs, which imports POST and proves the access gate
 * rejects invalid capability requests before JSON body parsing.
 */
function genericActionRequest(origin, host, token, options = {}) {
  return request(origin, host, token, {
    ...options,
    body: JSON.stringify({
      area: "job-health",
      itemId: "jhp_smoke",
      actionId: "record_job_health_review",
      confirmed: true,
    }),
  });
}

async function assertBlockedResponseDoesNotDisclose(response, capability, label) {
  assert.ok(response instanceof Response, `${label} must return a blocking response`);
  const serialized = JSON.stringify({
    headers: [...response.headers.entries()],
    body: await response.clone().text(),
  });
  assert.equal(serialized.includes(capability), false, `${label} must not disclose capability material`);
  assert.equal(response.headers.has("set-cookie"), false, `${label} must not mint a capability cookie`);
}

const oldEnabled = process.env[CONSOLE_MUTATIONS_ENV];
const oldCapability = process.env[CONSOLE_CAPABILITY_ENV];
const oldLegacySession = process.env.COMPOUND_CONSOLE_SESSION_TOKEN;

try {
  delete process.env[CONSOLE_MUTATIONS_ENV];
  delete process.env[CONSOLE_CAPABILITY_ENV];

  let blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000"));
  assert.equal(blocked?.status, 403);

  process.env[CONSOLE_MUTATIONS_ENV] = "1";
  blocked = consoleMutationBlockedResponse(request("https://example.com", "localhost:3000"));
  assert.equal(blocked?.status, 403);

  blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000"));
  assert.equal(blocked?.status, 403, "missing out-of-band capability must fail closed");

  process.env[CONSOLE_CAPABILITY_ENV] = "short";
  blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000", "short"));
  assert.equal(blocked?.status, 403, "malformed capability configuration must fail closed");

  process.env[CONSOLE_CAPABILITY_ENV] = FORMAT_VALID_CAPABILITY;
  process.env.COMPOUND_CONSOLE_SESSION_TOKEN = "legacy-session-sentinel";
  const childEnv = consoleChildEnv({ COMPOUND_HOME: "/synthetic/compound-home" });
  assert.equal(CONSOLE_CAPABILITY_ENV in childEnv, false, "child commands must not inherit the console capability");
  assert.equal(
    "COMPOUND_CONSOLE_SESSION_TOKEN" in childEnv,
    false,
    "child commands must not inherit the legacy session token",
  );
  assert.equal(childEnv.COMPOUND_HOME, "/synthetic/compound-home", "explicit nonsecret child overrides must win");
  const childProbe = spawnSync(
    process.execPath,
    [
      "-e",
      "process.stdout.write(JSON.stringify({ capability: Object.hasOwn(process.env, 'COMPOUND_CONSOLE_CAPABILITY'), legacySession: Object.hasOwn(process.env, 'COMPOUND_CONSOLE_SESSION_TOKEN'), home: process.env.COMPOUND_HOME }))",
    ],
    { env: childEnv, encoding: "utf8" },
  );
  assert.equal(childProbe.status, 0, "child-environment canary must execute");
  assert.deepEqual(JSON.parse(childProbe.stdout), {
    capability: false,
    legacySession: false,
    home: "/synthetic/compound-home",
  });

  blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000"));
  assert.equal(blocked?.status, 403);
  await assertBlockedResponseDoesNotDisclose(blocked, FORMAT_VALID_CAPABILITY, "missing capability error");
  blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000", "wrong"));
  assert.equal(blocked?.status, 403);
  await assertBlockedResponseDoesNotDisclose(blocked, FORMAT_VALID_CAPABILITY, "wrong capability error");
  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "localhost:3000", "d".repeat(64)),
  );
  assert.equal(blocked?.status, 403, "same-length wrong capability must fail closed");
  blocked = consoleMutationBlockedResponse(request("http://localhost:3000", "localhost:3000", FORMAT_VALID_CAPABILITY));
  assert.equal(blocked, null);

  // The Request URL is the server-owned origin. Host is caller-controlled and
  // must play no part in authorization. Exact scheme + hostname + port must
  // match the parsed Origin, and both sides must remain loopback-local.
  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "attacker.example", FORMAT_VALID_CAPABILITY),
  );
  assert.equal(blocked, null, "a forged Host header must not override an exact local Request/Origin pair");

  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "localhost:3000", FORMAT_VALID_CAPABILITY, {
      url: (() => {
        const mismatchedPortURL = new URL("/api/decision/execute", LOCAL_ORIGIN);
        mismatchedPortURL.port = "3777";
        return mismatchedPortURL;
      })(),
    }),
  );
  assert.equal(blocked?.status, 403, "Origin port must match the Request URL port exactly");

  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "localhost:3000", FORMAT_VALID_CAPABILITY, {
      url: new URL("/api/decision/execute", LOCAL_ORIGIN.replace("http:", "https:")),
    }),
  );
  assert.equal(blocked?.status, 403, "Origin scheme must match the Request URL scheme exactly");

  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "localhost:3000", FORMAT_VALID_CAPABILITY, {
      url: "http://example.invalid:3000/api/decision/execute",
    }),
  );
  assert.equal(blocked?.status, 403, "a local Origin cannot authorize a non-local Request URL");

  blocked = consoleMutationBlockedResponse(
    request("http://localhost:3000", "localhost:3000", null, { cookie: FORMAT_VALID_CAPABILITY }),
  );
  assert.equal(blocked?.status, 403, "browser cookies must never carry the server capability");
  await assertBlockedResponseDoesNotDisclose(blocked, FORMAT_VALID_CAPABILITY, "cookie-only capability error");

  // The generic governed-action pathway (Phase 3a) is dispatched from the
  // SAME POST /api/decision/execute handler, so it must be gated identically
  // -- guard-off blocks it regardless of GENERIC_ACTION_ALLOWLIST, exactly
  // like the bespoke path above.
  delete process.env[CONSOLE_MUTATIONS_ENV];
  delete process.env[CONSOLE_CAPABILITY_ENV];
  blocked = consoleMutationBlockedResponse(genericActionRequest("http://localhost:3000", "localhost:3000"));
  assert.equal(blocked?.status, 403, "generic-shaped request must 403 when COMPOUND_CONSOLE_MUTATIONS is unset");

  process.env[CONSOLE_MUTATIONS_ENV] = "1";
  blocked = consoleMutationBlockedResponse(genericActionRequest("https://example.com", "localhost:3000"));
  assert.equal(blocked?.status, 403, "generic-shaped cross-origin request must still 403 with guard on");

  blocked = consoleMutationBlockedResponse(genericActionRequest("http://localhost:3000", "localhost:3000"));
  assert.equal(blocked?.status, 403, "generic-shaped request without a capability must fail closed");

  process.env[CONSOLE_CAPABILITY_ENV] = FORMAT_VALID_CAPABILITY;

  blocked = consoleMutationBlockedResponse(
    genericActionRequest("http://localhost:3000", "localhost:3000", FORMAT_VALID_CAPABILITY),
  );
  assert.equal(blocked, null, "generic-shaped request with the exact capability passes");

  const middlewareURL = new URL("../src/middleware.ts", import.meta.url);
  if (existsSync(middlewareURL)) {
    const middlewareSource = readFileSync(middlewareURL, "utf8");
    assert.equal(/CONSOLE_(?:SESSION_TOKEN|CAPABILITY)|cookies\.set\(/.test(middlewareSource), false,
      "standard middleware must never receive or mint the server capability");
  }

  const guardSource = readFileSync(new URL("../src/lib/console-mutations.ts", import.meta.url), "utf8");
  assert.match(guardSource, /createHash\("sha256"\)/,
    "capability comparison must normalize both inputs to fixed-size digests");
  assert.match(guardSource, /timingSafeEqual\(providedDigest, expectedDigest\)/,
    "capability equality must use the fixed-size timing-safe primitive");
  assert.equal(/providedBytes\.length\s*===\s*expectedBytes\.length/.test(guardSource), false,
    "capability equality must not branch on the secret's byte length");

  const nextConfigSource = readFileSync(new URL("../next.config.ts", import.meta.url), "utf8");
  assert.match(
    nextConfigSource,
    /logging:\s*{\s*incomingRequests:\s*false\s*}/,
    "Next request URLs must not be logged because rejected query credentials are secret-bearing",
  );

  for (const owner of [
    "src/lib/compound.ts",
    "src/lib/run-attempts.ts",
    "src/app/api/stream/route.ts",
  ]) {
    const source = readFileSync(new URL("../" + owner, import.meta.url), "utf8");
    const childCalls = source.match(/\b(?:exec|spawn)\(/g) ?? [];
    const filteredEnvironments = source.match(/env:\s*consoleChildEnv\(/g) ?? [];
    assert.equal(
      filteredEnvironments.length,
      childCalls.length,
      `${owner} must pass the centralized secret-free environment to every direct child process`,
    );
    assert.doesNotMatch(source, /env:\s*{\s*\.\.\.process\.env/, `${owner} must not clone ambient env directly`);
  }
} finally {
  if (oldEnabled === undefined) delete process.env[CONSOLE_MUTATIONS_ENV];
  else process.env[CONSOLE_MUTATIONS_ENV] = oldEnabled;

  if (oldCapability === undefined) delete process.env[CONSOLE_CAPABILITY_ENV];
  else process.env[CONSOLE_CAPABILITY_ENV] = oldCapability;

  if (oldLegacySession === undefined) delete process.env.COMPOUND_CONSOLE_SESSION_TOKEN;
  else process.env.COMPOUND_CONSOLE_SESSION_TOKEN = oldLegacySession;
}

console.log("smoke opt-in, same-origin, and out-of-band capability gates ok");
