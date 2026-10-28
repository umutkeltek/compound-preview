import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rm } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const consoleRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = path.resolve(consoleRoot, "..");
let serverOrigin = "";
const scratch = await mkdtemp(path.join(tmpdir(), "compound-console-capability-canary-"));
const homePath = path.join(scratch, "home");
const binaryPath = path.join(scratch, "compound");
const capability = randomBytes(32).toString("hex");
const legacySession = randomBytes(32).toString("hex");
const wrongCapability = (capability.startsWith("0") ? "1" : "0") + capability.slice(1);
const secretVariants = new Set();

for (const secret of [capability, legacySession]) {
  secretVariants.add(secret);
  secretVariants.add(encodeURIComponent(secret));
  secretVariants.add(Buffer.from(secret, "utf8").toString("base64"));
  secretVariants.add(Buffer.from(secret, "utf8").toString("hex"));
}

const canaryEnv = {
  ...process.env,
  COMPOUND_HOME: homePath,
  COMPOUND_CONSOLE_CAPABILITY: capability,
  COMPOUND_CONSOLE_SESSION_TOKEN: legacySession,
  COMPOUND_CONSOLE_MUTATIONS: "1",
  COMPOUND_CONSOLE_PRIVATE_FALLBACKS: "1",
};

function assertSecretFree(label, value) {
  const material = Buffer.isBuffer(value) ? value.toString("utf8") : String(value ?? "");
  for (const variant of secretVariants) {
    assert.equal(material.includes(variant), false, label + " disclosed capability/session material");
  }
}

function runChecked(label, command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: repoRoot,
    env: canaryEnv,
    encoding: "utf8",
    maxBuffer: 128 * 1024 * 1024,
    ...options,
  });
  assertSecretFree(label + " stdout", result.stdout);
  assertSecretFree(label + " stderr", result.stderr);
  assert.equal(result.status, 0, label + " failed");
  return result;
}

function firstJsonObject(text) {
  const start = text.indexOf("{");
  assert.notEqual(start, -1, "launch dry-run did not emit JSON");
  return JSON.parse(text.slice(start));
}

async function filesUnder(root) {
  const entries = await readdir(root, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const full = path.join(root, entry.name);
    if (entry.isDirectory()) files.push(...(await filesUnder(full)));
    else if (entry.isFile()) files.push(full);
  }
  return files;
}

async function assertArtifactTreeSecretFree(root, label) {
  for (const file of await filesUnder(root)) {
    assertSecretFree(label + " " + path.relative(root, file), await readFile(file));
  }
}

async function allocateLoopbackPort() {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  assert.ok(address && typeof address !== "string", "ephemeral loopback port allocation failed");
  await new Promise((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

function captureChild(command, args, env = canaryEnv, cwd = repoRoot) {
  const child = spawn(command, args, {
    cwd,
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const stdout = [];
  const stderr = [];
  child.stdout.on("data", (chunk) => stdout.push(Buffer.from(chunk)));
  child.stderr.on("data", (chunk) => stderr.push(Buffer.from(chunk)));
  return {
    child,
    output() {
      return {
        stdout: Buffer.concat(stdout).toString("utf8"),
        stderr: Buffer.concat(stderr).toString("utf8"),
      };
    },
  };
}

async function waitForServer(captured, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (captured.child.exitCode !== null) {
      throw new Error("console profile exited before becoming ready");
    }
    try {
      const response = await fetch(serverOrigin + "/", { signal: AbortSignal.timeout(2_000) });
      await response.body?.cancel();
      return;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  throw new Error("console profile did not become ready before timeout");
}

async function stopProfile(captured) {
  if (captured.child.exitCode !== null) return;
  captured.child.kill("SIGTERM");
  await Promise.race([
    new Promise((resolve) => captured.child.once("close", resolve)),
    new Promise((resolve) => setTimeout(resolve, 10_000)),
  ]);
  if (captured.child.exitCode === null) {
    captured.child.kill("SIGKILL");
    await new Promise((resolve) => captured.child.once("close", resolve));
  }
}

function serializedResponse(response, body) {
  return JSON.stringify({
    status: response.status,
    headers: [...response.headers.entries()],
    body,
  });
}

async function fetchSurface(label, input, init) {
  const response = await fetch(input, { ...init, signal: AbortSignal.timeout(30_000) });
  const body = await response.text();
  assertSecretFree(label, serializedResponse(response, body));
  return { response, body };
}

function staticAssetURLs(html) {
  const urls = new Set();
  const pattern = /(?:src|href)=["']([^"']*\/_next\/static\/[^"']+)["']/g;
  for (const match of html.matchAll(pattern)) {
    urls.add(new URL(match[1].replaceAll("&amp;", "&"), serverOrigin).toString());
  }
  return [...urls];
}

async function exerciseProfile(lifecycle) {
  const runtimeEnv = {
    ...canaryEnv,
    COMPOUND_BIN: binaryPath,
    COMPOUND_CONSOLE_BUILD_KIND: lifecycle === "dev" ? "development" : "production",
    COMPOUND_CONSOLE_MUTATIONS: "1",
    COMPOUND_CONSOLE_PRIVATE_FALLBACKS: "0",
  };
  const lifecycleCommand = lifecycle === "dev" ? "dev" : "start";
  const port = new URL(serverOrigin).port;
  const captured = captureChild(
    "bun",
    ["run", "next", lifecycleCommand, "-H", "127.0.0.1", "-p", port],
    runtimeEnv,
    consoleRoot,
  );
  let profileError = null;
  try {
    await waitForServer(captured, lifecycle === "dev" ? 180_000 : 60_000);
    const html = await fetchSurface(lifecycle + " HTML", serverOrigin + "/");
    assert.equal(html.response.status, 200, lifecycle + " initial HTML must render");

    const rsc = await fetchSurface(lifecycle + " RSC", serverOrigin + "/?_rsc=capability-canary", {
      headers: {
        RSC: "1",
        "Next-Router-Prefetch": "1",
      },
    });
    assert.ok(rsc.response.status >= 200 && rsc.response.status < 500, lifecycle + " RSC request must complete");

    const assets = staticAssetURLs(html.body);
    assert.ok(assets.length > 0, lifecycle + " HTML must reference Next static assets");
    for (const asset of assets) {
      const result = await fetchSurface(lifecycle + " static asset", asset);
      assert.equal(result.response.status, 200, lifecycle + " static asset must load");
    }

    const route = serverOrigin + "/api/decision/execute";
    const baseHeaders = { origin: serverOrigin, "content-type": "application/json" };
    const missing = await fetchSurface(lifecycle + " missing-capability error", route, {
      method: "POST",
      headers: baseHeaders,
      body: "{",
    });
    assert.equal(missing.response.status, 403, lifecycle + " missing capability must fail closed");
    assert.equal(missing.response.headers.has("set-cookie"), false, lifecycle + " error must not mint a cookie");

    const wrong = await fetchSurface(lifecycle + " wrong-capability error", route, {
      method: "POST",
      headers: { ...baseHeaders, "x-compound-console-capability": wrongCapability },
      body: "{",
    });
    assert.equal(wrong.response.status, 403, lifecycle + " wrong capability must fail closed");

    const cookieOnly = await fetchSurface(lifecycle + " cookie-only error", route, {
      method: "POST",
      headers: { ...baseHeaders, cookie: "compound_console_session_token=" + capability },
      body: "{",
    });
    assert.equal(cookieOnly.response.status, 403, lifecycle + " cookie-only capability must fail closed");

    const queryOnly = await fetchSurface(lifecycle + " query-only error", route + "?capability=" + capability, {
      method: "POST",
      headers: baseHeaders,
      body: "{",
    });
    assert.equal(queryOnly.response.status, 403, lifecycle + " query-only capability must fail closed");

    const bodyOnly = await fetchSurface(lifecycle + " body-only error", route, {
      method: "POST",
      headers: baseHeaders,
      body: JSON.stringify({ capability }),
    });
    assert.equal(bodyOnly.response.status, 403, lifecycle + " body-only capability must fail closed");

    const authorized = await fetchSurface(lifecycle + " authorized validation error", route, {
      method: "POST",
      headers: { ...baseHeaders, "x-compound-console-capability": capability },
      body: "{",
    });
    assert.equal(authorized.response.status, 400, lifecycle + " correct header must pass auth and reach JSON validation");
    assert.equal(authorized.response.headers.has("set-cookie"), false, lifecycle + " authorized route must not mint a cookie");

    await new Promise((resolve) => setTimeout(resolve, 500));
  } catch (error) {
    profileError = error;
  } finally {
    await stopProfile(captured);
    const output = captured.output();
    assertSecretFree(lifecycle + " stdout log", output.stdout);
    assertSecretFree(lifecycle + " stderr log", output.stderr);
  }
  if (profileError) throw profileError;
}

try {
  serverOrigin = "http://localhost:" + (await allocateLoopbackPort());
  await mkdir(homePath, { recursive: true, mode: 0o700 });
  runChecked("candidate build", "go", ["build", "-o", binaryPath, "./cmd/compound"]);
  runChecked("home init", binaryPath, ["home", "init"]);

  const defaultPlanResult = runChecked("projection-only dry-run", binaryPath, [
    "console",
    "dev",
    "--operator",
    "--require-explicit-home",
    "--console-dir",
    consoleRoot,
    "--dry-run",
    "--json",
  ]);
  const defaultPlan = firstJsonObject(defaultPlanResult.stdout);
  assert.equal(defaultPlan.mutations_enabled, false, "default plan must remain projection-only");
  assert.equal(defaultPlan.private_fallbacks, false, "default plan must keep private fallbacks disabled");
  assert.equal(defaultPlan.env.COMPOUND_CONSOLE_MUTATIONS, "0", "default plan must publish explicit mutation-off posture");
  assert.equal(
    Object.hasOwn(defaultPlan.env, "COMPOUND_CONSOLE_CAPABILITY"),
    false,
    "launch JSON must not contain capability material",
  );

  const buildMutationAttempt = spawnSync(
    binaryPath,
    ["console", "build", "--operator", "--require-explicit-home", "--console-dir", consoleRoot, "--mutations", "--dry-run"],
    { cwd: repoRoot, env: canaryEnv, encoding: "utf8" },
  );
  assertSecretFree("rejected build stdout", buildMutationAttempt.stdout);
  assertSecretFree("rejected build stderr", buildMutationAttempt.stderr);
  assert.notEqual(buildMutationAttempt.status, 0, "console build must reject the runtime mutation extension");

  const buildEnv = {
    ...canaryEnv,
    COMPOUND_BIN: binaryPath,
    COMPOUND_CONSOLE_BUILD_KIND: "production",
    COMPOUND_CONSOLE_MUTATIONS: "0",
    COMPOUND_CONSOLE_PRIVATE_FALLBACKS: "0",
  };
  delete buildEnv.COMPOUND_CONSOLE_CAPABILITY;
  delete buildEnv.COMPOUND_CONSOLE_SESSION_TOKEN;
  const build = runChecked("production console build", "bun", ["run", "next", "build"], {
    cwd: consoleRoot,
    env: buildEnv,
  });
  assertSecretFree("production build output", build.stdout + build.stderr);
  await assertArtifactTreeSecretFree(path.join(consoleRoot, ".next", "production"), "production artifact");

  await exerciseProfile("dev");
  await assertArtifactTreeSecretFree(path.join(consoleRoot, ".next", "development"), "development artifact");
  await exerciseProfile("serve");

  const buildID = (await readFile(path.join(consoleRoot, ".next", "production", "BUILD_ID"), "utf8")).trim();
  const binaryDigest = createHash("sha256").update(await readFile(binaryPath)).digest("hex");
  console.log("capability profile canary ok: dev+serve surfaces clean; build_id=" + buildID + " binary_sha256=" + binaryDigest);
} finally {
  await rm(scratch, { recursive: true, force: true });
}
