import { afterAll, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

import { buildPreflightReport, expandASCCommand } from "./asc_preflight.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

function buildCompound(): string {
  const bin = join(temporary.dir("compound-asc-preflight-bin-"), "compound");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], { encoding: "utf8" });
  if (build.status !== 0) throw new Error(build.stderr || build.stdout);
  return bin;
}

function writeConfig(apps: unknown[]): string {
  const path = join(temporary.dir("compound-asc-preflight-config-"), "asc-products.json");
  writeFileSync(path, `${JSON.stringify({ apps }, null, 2)}\n`);
  return path;
}

describe("asc_preflight edge", () => {
  test("passes private mapping, product authority, smoke, and scheduler checks without applying", () => {
    const home = temporary.dir("compound-asc-preflight-home-");
    const bin = buildCompound();
    const env = initializeCompoundHome(bin, home);
    const imported = spawnSync(bin, ["product", "import", "fixtures/product/market-loop.yaml"], {
      encoding: "utf8",
      env,
    });
    if (imported.status !== 0) throw new Error(imported.stderr || imported.stdout);
    const beforeAuthority = spawnSync(bin, ["product", "authority", "--json"], { encoding: "utf8", env });
    if (beforeAuthority.status !== 0) throw new Error(beforeAuthority.stderr || beforeAuthority.stdout);
    const beforeAuthorityReport = JSON.parse(beforeAuthority.stdout);

    const configPath = writeConfig([
      {
        product_id: "product:focus-notes",
        asc_app_id: "123456789",
        vendor: "123456",
      },
    ]);
    const report = buildPreflightReport({
      configPath,
      compoundBin: bin,
      ascCommand: "printf '%s\\n' '{\"date\":\"2026-07-04\",\"apps\":[{\"metrics\":{\"units\":1}}]}'",
      date: "2026-07-04",
      smokeLimit: 1,
      plistPath: "tools/templates/ai.compound.asc-daily.plist",
      env,
    });

    expect(report.status).toBe("ok");
    expect(report.product_authority).toEqual({
      checked: true,
      product_count: 1,
      missing_product_count: 0,
    });
    expect(report.smoke).toEqual({
      attempted: true,
      checked_app_count: 1,
      ok_count: 1,
      failed_count: 0,
    });
    expect(report.scheduler.load_commands.join("\n")).toContain("launchctl bootstrap");

    const privateConfig = report.actions.find((action: { kind: string }) => action.kind === "private_config_ready");
    expect(privateConfig.requires_operator).toBe(true);
    expect(privateConfig.expected_validation).toContain("all ASC mappings registered Compound products");
    expect(privateConfig.stop_conditions).toContain("product_authority_missing_mapping");

    const credentialSmoke = report.actions.find((action: { kind: string }) => action.kind === "credential_smoke");
    expect(credentialSmoke.command).toContain("--asc-command");
    expect(credentialSmoke.prerequisites).toContain("private_config_ready");
    expect(credentialSmoke.expected_validation).toContain("asc_credential_smoke ok_count_gt_0 failed_count_eq_0");

    const schedulerLoad = report.actions.find((action: { kind: string }) => action.kind === "scheduler_load");
    expect(schedulerLoad.command).toContain("launchctl bootstrap");
    expect(schedulerLoad.prerequisites).toContain("credential_smoke");
    expect(schedulerLoad.rollback).toContain("launchctl bootout");
    expect(schedulerLoad.stop_conditions).toContain("asc_credential_smoke_not_ok");

    const firstApply = report.actions.find((action: { kind: string }) => action.kind === "first_signal_apply");
    expect(firstApply.prerequisites).toContain("scheduler_loaded");
    expect(firstApply.expected_validation).toContain("compound product authority shows fresh ASC signal");
    expect(firstApply.stop_conditions).toContain("scheduler_not_loaded");

    const authority = spawnSync(bin, ["product", "authority", "--json"], { encoding: "utf8", env });
    const authorityReport = JSON.parse(authority.stdout);
    expect(authorityReport.signal_count).toBe(beforeAuthorityReport.signal_count);
  }, 15000);

  test("fails when private ASC mapping references an unregistered product", () => {
    const home = temporary.dir("compound-asc-preflight-home-");
    const bin = buildCompound();
    const env = initializeCompoundHome(bin, home);
    const configPath = writeConfig([
      {
        product_id: "product:missing",
        asc_app_id: "123456789",
      },
    ]);

    const report = buildPreflightReport({
      configPath,
      compoundBin: bin,
      date: "2026-07-04",
      smokeLimit: 1,
      plistPath: "tools/templates/ai.compound.asc-daily.plist",
      env,
    });

    expect(report.status).toBe("fail");
    expect(report.product_authority.missing_product_count).toBe(1);
    expect(report.checks.find((check) => check.id === "asc_credential_smoke")?.status).toBe("warn");
  }, 15000);

  test("expands ASC command placeholders as shell-safe arguments", () => {
    const command = expandASCCommand(
      "asc daily --app {app_id} --product {product_id} --vendor {vendor} --date {date}",
      {
        product_id: "product:demo",
        asc_app_id: "123'456",
        vendor: "vendor-x",
      },
      "2026-07-04",
    );

    expect(command).toBe(
      "asc daily --app '123'\\''456' --product 'product:demo' --vendor 'vendor-x' --date '2026-07-04'",
    );
  });
});
