import { readFile } from "node:fs/promises";
import { join } from "node:path";

import { INSTANCE_PIN, type InstancePin } from "./instance-pin.ts";

export const CONSOLE_READINESS_SCHEMA = "compound.console_readiness.v0.1";

type BuildManifest = { rootMainFiles?: unknown };
type AppBuildManifest = { pages?: Record<string, unknown> };

function staticAssetPath(value: unknown): string | null {
  if (typeof value !== "string" || !value.startsWith("static/") || value.includes("..") ||
      !(value.endsWith(".js") || value.endsWith(".css")) ||
      !/^static\/[A-Za-z0-9/_.-]+$/.test(value)) return null;
  return `/_next/${value}`;
}

function manifestAssets(value: unknown): string[] {
  if (!Array.isArray(value) || value.length === 0) throw new Error("missing build assets");
  return value.map((item) => {
    const asset = staticAssetPath(item);
    if (!asset) throw new Error("invalid build asset");
    return asset;
  });
}

/** Build-only and pin-only facts; no Compound CLI or home projection reads. */
export async function consoleReadiness(
  pin: InstancePin = INSTANCE_PIN,
  consoleDir: string = process.cwd(),
) {
  if (pin.buildKind !== "production" && pin.buildKind !== "development") {
    throw new Error("unknown build kind");
  }
  const distDir = join(consoleDir, ".next", pin.buildKind);
  const buildManifest = JSON.parse(await readFile(join(distDir, "build-manifest.json"), "utf8")) as BuildManifest;
  const appManifest = JSON.parse(await readFile(join(distDir, "app-build-manifest.json"), "utf8")) as AppBuildManifest;
  let buildID = "";
  if (pin.buildKind === "production") {
    buildID = (await readFile(join(distDir, "BUILD_ID"), "utf8")).trim();
    if (!/^[A-Za-z0-9_-]{1,128}$/.test(buildID)) throw new Error("invalid build id");
  }
  const assets = new Set(manifestAssets(buildManifest.rootMainFiles));
  for (const page of ["/layout", "/page"]) {
    const files = appManifest.pages?.[page];
    if (files === undefined && pin.buildKind === "development") continue;
    for (const asset of manifestAssets(files)) assets.add(asset);
  }
  if (assets.size === 0 || assets.size > 32) throw new Error("invalid build asset count");
  return {
    schema_version: CONSOLE_READINESS_SCHEMA,
    home_hash: pin.homeHash,
    explicit: pin.explicit,
    build_kind: pin.buildKind,
    build_id: buildID,
    static_assets: [...assets],
    data_freshness: "not_checked" as const,
  };
}
