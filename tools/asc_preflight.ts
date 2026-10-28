#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";

import { parseConfig } from "./asc_daily_signals.ts";

type Status = "ok" | "warn" | "fail";

type AppConfig = {
  product_id: string;
  asc_app_id: string;
  vendor?: string;
};

type PreflightOptions = {
  configPath: string;
  compoundBin: string;
  ascCommand?: string;
  date: string;
  smokeLimit: number;
  plistPath: string;
  env?: Record<string, string | undefined>;
};

type Check = {
  id: string;
  status: Status;
  summary: string;
  next_action?: string;
};

type Action = {
  kind: string;
  command: string;
  boundary: string;
  prerequisites?: string[];
  expected_validation?: string[];
  rollback?: string;
  stop_conditions?: string[];
  requires_operator?: boolean;
};

type PreflightReport = {
  schema_version: "compound.asc_preflight.v0.1";
  status: Status;
  app_count: number;
  checks: Check[];
  actions: Action[];
  product_authority: {
    checked: boolean;
    product_count: number;
    missing_product_count: number;
  };
  smoke: {
    attempted: boolean;
    checked_app_count: number;
    ok_count: number;
    failed_count: number;
  };
  scheduler: {
    plist_path: string;
    required_env: string[];
    load_commands: string[];
    unload_commands: string[];
  };
};

function usage(): never {
  console.error(`usage: bun tools/asc_preflight.ts --config <json|yaml> [--compound-bin compound] [--asc-command <cmd>] [--date YYYY-MM-DD] [--smoke-limit N] [--plist tools/templates/ai.compound.asc-daily.plist] [--json]

Preflights the ASC private-config / credential-smoke / scheduler valve without applying product signals or loading a scheduler.`);
  process.exit(2);
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--json") {
      out.json = true;
      continue;
    }
    if (!arg.startsWith("--") || i + 1 >= argv.length) usage();
    out[arg.slice(2)] = argv[++i];
  }
  return out;
}

function buildPreflightReport(opts: PreflightOptions): PreflightReport {
  const config = parseConfig(opts.configPath);
  const apps = config.apps as AppConfig[];
  const checks: Check[] = [
    {
      id: "private_config",
      status: apps.length > 0 ? "ok" : "fail",
      summary: `loaded ${apps.length} ASC app mapping(s)`,
      next_action: apps.length > 0 ? undefined : "add private ASC product mapping config",
    },
  ];

  const productAuthority = checkProductAuthority(opts.compoundBin, apps, opts.env);
  checks.push(productAuthority.check);

  const smoke = checkASCSmoke(apps, opts);
  checks.push(smoke.check);

  const scheduler = schedulerPlan(opts.plistPath);
  checks.push(scheduler.check);
  const requiredEnv = ["COMPOUND_REPO", "COMPOUND_ASC_PRODUCTS", "COMPOUND_ASC_DAILY_COMMAND"];
  const loadCommands = [
    "mkdir -p \"${HOME}/Library/LaunchAgents\"",
    "launchctl setenv COMPOUND_REPO \"$(pwd)\"",
    "launchctl setenv COMPOUND_ASC_PRODUCTS \"${COMPOUND_HOME:-${HOME}/.compound}/private/asc-products.json\"",
    "launchctl setenv COMPOUND_ASC_DAILY_COMMAND '<ASC_DAILY_COMMAND>'",
    "cp tools/templates/ai.compound.asc-daily.plist \"${HOME}/Library/LaunchAgents/ai.compound.asc-daily.plist\"",
    "launchctl bootstrap \"gui/$(id -u)\" \"${HOME}/Library/LaunchAgents/ai.compound.asc-daily.plist\"",
  ];
  const unloadCommands = [
    "launchctl bootout \"gui/$(id -u)\" \"${HOME}/Library/LaunchAgents/ai.compound.asc-daily.plist\"",
  ];

  return {
    schema_version: "compound.asc_preflight.v0.1",
    status: statusFromChecks(checks),
    app_count: apps.length,
    checks,
    actions: buildActions(opts.date, loadCommands, unloadCommands),
    product_authority: {
      checked: productAuthority.checked,
      product_count: productAuthority.productCount,
      missing_product_count: productAuthority.missingProductCount,
    },
    smoke: {
      attempted: smoke.attempted,
      checked_app_count: smoke.checkedAppCount,
      ok_count: smoke.okCount,
      failed_count: smoke.failedCount,
    },
    scheduler: {
      plist_path: opts.plistPath,
      required_env: requiredEnv,
      load_commands: loadCommands,
      unload_commands: unloadCommands,
    },
  };
}

function checkProductAuthority(
  compoundBin: string,
  apps: AppConfig[],
  env: Record<string, string | undefined> = process.env,
): {
  checked: boolean;
  productCount: number;
  missingProductCount: number;
  check: Check;
} {
  const result = spawnSync(compoundBin, ["product", "authority", "--json"], {
    encoding: "utf8",
    env,
  });
  if (result.status !== 0) {
    return {
      checked: false,
      productCount: 0,
      missingProductCount: apps.length,
      check: {
        id: "product_authority",
        status: "fail",
        summary: "could not read Compound product authority",
        next_action: "run compound product authority --json and repair product registry before scheduler load",
      },
    };
  }
  const report = JSON.parse(result.stdout);
  const productIDs = new Set((report.products ?? []).map((item: { product_id?: string }) => item.product_id));
  const missing = apps.filter((app) => !productIDs.has(app.product_id));
  return {
    checked: true,
    productCount: Number(report.product_count ?? 0),
    missingProductCount: missing.length,
    check: {
      id: "product_authority",
      status: missing.length === 0 ? "ok" : "fail",
      summary:
        missing.length === 0
          ? "all ASC mappings have registered Compound products"
          : `${missing.length} ASC mapping(s) missing registered Compound products`,
      next_action:
        missing.length === 0
          ? undefined
          : "import/register products before running ASC scheduler or applying signals",
    },
  };
}

function checkASCSmoke(apps: AppConfig[], opts: PreflightOptions): {
  attempted: boolean;
  checkedAppCount: number;
  okCount: number;
  failedCount: number;
  check: Check;
} {
  if (!opts.ascCommand) {
    return {
      attempted: false,
      checkedAppCount: 0,
      okCount: 0,
      failedCount: 0,
      check: {
        id: "asc_credential_smoke",
        status: "warn",
        summary: "ASC command smoke was not run",
        next_action: "rerun with --asc-command after private credentials are available",
      },
    };
  }

  const limit = Math.max(1, opts.smokeLimit);
  const selected = apps.slice(0, limit);
  let okCount = 0;
  let failedCount = 0;
  for (const app of selected) {
    const command = expandASCCommand(opts.ascCommand, app, opts.date);
    const result = spawnSync("/bin/zsh", ["-lc", command], {
      encoding: "utf8",
      env: opts.env ?? process.env,
    });
    if (result.status !== 0 || !looksLikeASCJSON(result.stdout)) {
      failedCount++;
    } else {
      okCount++;
    }
  }

  return {
    attempted: true,
    checkedAppCount: selected.length,
    okCount,
    failedCount,
    check: {
      id: "asc_credential_smoke",
      status: failedCount === 0 ? "ok" : "fail",
      summary: `ASC command smoke checked ${selected.length} app(s): ok=${okCount} failed=${failedCount}`,
      next_action: failedCount === 0 ? undefined : "fix ASC command/auth before scheduler load",
    },
  };
}

function schedulerPlan(plistPath: string): { check: Check } {
  if (!existsSync(plistPath)) {
    return {
      check: {
        id: "scheduler_plan",
        status: "fail",
        summary: "ASC launchd example plist missing",
        next_action: "restore tools/templates/ai.compound.asc-daily.plist or provide another scheduler plan",
      },
    };
  }
  const text = readFileSync(plistPath, "utf8");
  const required = ["COMPOUND_REPO", "COMPOUND_ASC_PRODUCTS", "COMPOUND_ASC_DAILY_COMMAND", "--apply"];
  const missing = required.filter((needle) => !text.includes(needle));
  return {
    check: {
      id: "scheduler_plan",
      status: missing.length === 0 ? "ok" : "fail",
      summary:
        missing.length === 0
          ? "scheduler example contains required private env handoff and apply path"
          : `scheduler example missing ${missing.join(", ")}`,
      next_action: missing.length === 0 ? undefined : "repair scheduler example before operator load",
    },
  };
}

function buildActions(date: string, loadCommands: string[], unloadCommands: string[]): Action[] {
  const loadCommand = loadCommands.join(" && ");
  const unloadCommand = unloadCommands.join(" && ");
  return [
    {
      kind: "private_config_ready",
      command: "bun tools/asc_preflight.ts --config <private-asc-products.json> --json",
      boundary: "operator supplies private ASC product mapping; read-only preflight only",
      expected_validation: ["all ASC mappings registered Compound products"],
      stop_conditions: ["product_authority_missing_mapping", "private_config_missing_or_invalid"],
      requires_operator: true,
    },
    {
      kind: "credential_smoke",
      command: "bun tools/asc_preflight.ts --config <private-asc-products.json> --asc-command '<ASC_DAILY_COMMAND>' --json",
      boundary: "operator-controlled read-only ASC credential smoke; no product signal apply",
      prerequisites: ["private_config_ready"],
      expected_validation: ["asc_credential_smoke ok_count_gt_0 failed_count_eq_0"],
      stop_conditions: ["product_authority_not_ok", "asc_credential_smoke_not_ok"],
      requires_operator: true,
    },
    {
      kind: "scheduler_load",
      command: loadCommand,
      boundary: "operator-only launchd load after private config, credential smoke, and scheduler plan are all green",
      prerequisites: ["private_config_ready", "credential_smoke", "scheduler_plan_ready"],
      expected_validation: ["launchctl bootstrap exits zero", "scheduler plist installed under user LaunchAgents"],
      rollback: unloadCommand,
      stop_conditions: ["private_config_not_ready", "asc_credential_smoke_not_ok", "scheduler_plan_not_ok"],
      requires_operator: true,
    },
    {
      kind: "first_signal_apply",
      command: `bun tools/asc_daily_signals.ts --config <private-asc-products.json> --date ${date} --asc-command '<ASC_DAILY_COMMAND>' --apply --json`,
      boundary: "operator-triggered first ASC signal apply; uses existing generic product signal adapter",
      prerequisites: ["scheduler_loaded"],
      expected_validation: ["compound product authority shows fresh ASC signal", "second apply is idempotent"],
      rollback: "restore COMPOUND_HOME backup or revert the generated product signal receipt if the first apply is wrong",
      stop_conditions: ["scheduler_not_loaded", "asc_credential_smoke_not_ok", "product_signal_apply_failed"],
      requires_operator: true,
    },
  ];
}

function expandASCCommand(template: string, app: AppConfig, date: string): string {
  return template
    .replaceAll("{app_id}", shellQuote(app.asc_app_id))
    .replaceAll("{product_id}", shellQuote(app.product_id))
    .replaceAll("{vendor}", shellQuote(app.vendor ?? ""))
    .replaceAll("{date}", shellQuote(date));
}

function looksLikeASCJSON(stdout: string): boolean {
  try {
    const parsed = JSON.parse(stdout);
    return parsed && typeof parsed === "object";
  } catch {
    return false;
  }
}

function statusFromChecks(checks: Check[]): Status {
  if (checks.some((check) => check.status === "fail")) return "fail";
  if (checks.some((check) => check.status === "warn")) return "warn";
  return "ok";
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

function todayISO(): string {
  return new Date().toISOString().slice(0, 10);
}

function printHuman(report: PreflightReport) {
  console.log(`asc preflight ${report.status} apps=${report.app_count}`);
  for (const check of report.checks) {
    console.log(`- ${check.id}: ${check.status} - ${check.summary}`);
    if (check.next_action) console.log(`  next: ${check.next_action}`);
  }
  console.log("scheduler load commands:");
  for (const command of report.scheduler.load_commands) console.log(`- ${command}`);
  console.log("scheduler unload commands:");
  for (const command of report.scheduler.unload_commands) console.log(`- ${command}`);
  console.log("operator actions:");
  for (const action of report.actions) {
    console.log(`- ${action.kind}: ${action.boundary}`);
    console.log(` command: ${action.command}`);
    if (action.rollback) console.log(` rollback: ${action.rollback}`);
  }
}

export { buildPreflightReport, expandASCCommand, statusFromChecks };

if (import.meta.main) {
  try {
    const args = parseArgs(process.argv.slice(2));
    if (typeof args.config !== "string") usage();
    const report = buildPreflightReport({
      configPath: args.config,
      compoundBin: typeof args["compound-bin"] === "string" ? args["compound-bin"] : "compound",
      ascCommand: typeof args["asc-command"] === "string" ? args["asc-command"] : undefined,
      date: typeof args.date === "string" ? args.date : todayISO(),
      smokeLimit: typeof args["smoke-limit"] === "string" ? Number.parseInt(args["smoke-limit"], 10) : 1,
      plistPath: typeof args.plist === "string" ? args.plist : "tools/templates/ai.compound.asc-daily.plist",
    });
    if (args.json) {
      console.log(JSON.stringify(report, null, 2));
    } else {
      printHuman(report);
    }
    process.exit(report.status === "fail" ? 1 : 0);
  } catch (error) {
    console.error(`asc-preflight: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
