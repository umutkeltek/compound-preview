import { createHash, timingSafeEqual } from "node:crypto";

const LOCAL_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

export const CONSOLE_MUTATIONS_ENV = "COMPOUND_CONSOLE_MUTATIONS";
export const CONSOLE_CAPABILITY_ENV = "COMPOUND_CONSOLE_CAPABILITY";
export const CONSOLE_CAPABILITY_HEADER = "x-compound-console-capability";

// Encoding/length validation only. Entropy comes from the operator's out-of-band
// generator and cannot be inferred from a string matching this transport shape.
const CAPABILITY_RE = /^[a-f0-9]{64}$/i;

function cleanHostname(hostname: string): string {
  return hostname.trim().toLowerCase().replace(/^\[/, "").replace(/\]$/, "");
}

function isLocalHostname(hostname: string): boolean {
  return LOCAL_HOSTS.has(cleanHostname(hostname));
}

function sameLocalOrigin(request: Request): boolean {
  const origin = request.headers.get("origin");
  if (!origin) return false;

  try {
    const requestURL = new URL(request.url);
    const parsedOrigin = new URL(origin);
    return (
      requestURL.origin === parsedOrigin.origin &&
      isLocalHostname(requestURL.hostname) &&
      isLocalHostname(parsedOrigin.hostname)
    );
  } catch {
    return false;
  }
}

function configuredCapability(): string | null {
  const capability = process.env[CONSOLE_CAPABILITY_ENV] ?? "";
  return CAPABILITY_RE.test(capability) ? capability : null;
}

function exactCapabilityMatch(provided: string | null, expected: string): boolean {
  const providedDigest = createHash("sha256").update(provided ?? "", "utf8").digest();
  const expectedDigest = createHash("sha256").update(expected, "utf8").digest();
  const matches = timingSafeEqual(providedDigest, expectedDigest);
  return provided !== null && matches;
}

function capabilityBlockedResponse(request: Request, label: string): Response | null {
  const expected = configuredCapability();
  const provided = request.headers.get(CONSOLE_CAPABILITY_HEADER);
  if (expected === null || !exactCapabilityMatch(provided, expected)) {
    return Response.json(
      { ok: false, stderr: `${label} out-of-band capability missing or invalid` },
      { status: 403 },
    );
  }
  return null;
}

/** API-extension flag only. Standard browser readiness adds a permanent
 * projection-only client boundary in instance-pin.ts. */
export function consoleMutationsEnabled(): boolean {
  return process.env[CONSOLE_MUTATIONS_ENV] === "1";
}

export function consoleMutationBlockedResponse(request: Request): Response | null {
  if (process.env[CONSOLE_MUTATIONS_ENV] !== "1") {
    return Response.json(
      {
        ok: false,
        stderr:
          "console mutations disabled; the unsupported local API extension requires COMPOUND_CONSOLE_MUTATIONS=1",
      },
      { status: 403 },
    );
  }

  if (!sameLocalOrigin(request)) {
    return Response.json(
      { ok: false, stderr: "console mutations require same-origin localhost requests" },
      { status: 403 },
    );
  }

  const capabilityBlocked = capabilityBlockedResponse(request, "console mutation");
  if (capabilityBlocked) return capabilityBlocked;

  return null;
}
