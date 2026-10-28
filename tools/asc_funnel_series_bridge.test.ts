import { afterAll, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { parseConfig } from "./asc_daily_signals.ts";
import { loadFunnelRows, parseMetricList, planFunnelSeriesApplications } from "./asc_funnel_series_bridge.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());
const seriesPath = "fixtures/product/asc-funnel-series.jsonl";
const apps = [{ product_id: "product:focus-notes", asc_app_id: "123456789" }];
const options = {
 seriesPath, beforeWindow: "2026-07-01/2026-07-03", afterWindow: "2026-07-08/2026-07-10",
 days: 3, changeEvent: "Synthetic change.", metrics: ["impressions"], compoundBin: "compound",
};
function selectedRows(): any[] { return loadFunnelRows(seriesPath).filter(r => r.asc_app_id === "123456789" && r.date !== "2026-06-15"); }

// The producer's frozen contract is copied byte-for-byte into this synthetic fixture.
describe("ASC daily v2 bridge", () => {
 test("shared producer schema and published populations match v2 fixture", () => {
  const contract = JSON.parse(readFileSync("fixtures/product/asc-daily-v2-contract.json", "utf8"));
  for (const row of selectedRows()) {
   expect(row.schema_version).toBe(contract.schema_version);
   for (const metric of ["impressions", "product_page_views", "first_time_downloads", "proceeds_usd"]) {
    expect(row.metric_evidence[metric].population).toBe(contract.observed_populations[metric]);
   }
  }
 });
 test("plans by app ID with observed evidence for every inclusive day, including zero", () => {
  const plan = planFunnelSeriesApplications(parseConfig("fixtures/product/asc-products.json").apps, loadFunnelRows(seriesPath), options);
  expect(plan.commands).toHaveLength(1);
  expect(plan.commands[0].metric).toBe("impressions");
  expect(plan.commands[0].args.slice(-2)).toEqual(["--metric", "impressions"]);
  expect(plan.skipped).toEqual([{product_id: "product:other-app", asc_app_id: "987654321", reason: "no_rows_in_after_window"}]);
 });
 test("missing interior calendar day never produces a partial window", () => {
  const rows = selectedRows().filter(r => r.date !== "2026-07-02");
  const plan = planFunnelSeriesApplications(apps, rows, options);
  expect(plan.commands).toHaveLength(0);
  expect(plan.skipped[0].reason).toContain("2026-07-02");
 });
 test("duplicate app/date is rejected, never summed or picked arbitrarily", () => {
  const rows = selectedRows(); rows.push(structuredClone(rows[0]));
  expect(() => planFunnelSeriesApplications(apps, rows, options)).toThrow("duplicate");
 });
 const mutations: Array<[string, (row: any) => void]> = [
  ["legacy", r => {delete r.schema_version; r.coverage.complete_funnel = true;}],
  ["null", r => {r.impressions = null;}],
  ["boolean value", r => {r.impressions = true;}],
  ["missing numeric", r => {delete r.impressions;}],
  ["missing evidence", r => {delete r.metric_evidence;}],
  ["negative count", r => {r.impressions = -1;}],
  ["nonfinite", r => {r.impressions = Infinity;}],
  ["unknown metric", r => {r.metric_evidence.impressions.status = "unknown";}],
  ["suppressed", r => {r.metric_evidence.impressions.reason = "suppressed_or_invalid";}],
  ["unique population", r => {r.metric_evidence.impressions.population = "unique_people";}],
  ["no matching row", r => {r.metric_evidence.impressions.matched_rows = 0;}],
  ["missing source", r => {delete r.source_reports;}],
  ["open inventory", r => {r.source_reports.app_store_discovery_and_engagement_standard.segment_inventory_complete = false;}],
  ["missing segment", r => {r.source_reports.app_store_discovery_and_engagement_standard.expected_segment_ids.push("missing");}],
  ["duplicate segment", r => {const s=r.source_reports.app_store_discovery_and_engagement_standard; s.expected_segment_ids.push(s.expected_segment_ids[0]);}],
  ["bad hash", r => {r.source_reports.app_store_discovery_and_engagement_standard.segments[0].sha256 = "bad";}],
  ["empty body", r => {r.source_reports.app_store_discovery_and_engagement_standard.segments[0].byte_count = 0;}],
  ["wrong report", r => {r.source_reports.app_store_discovery_and_engagement_standard.report_name = "App Downloads Standard";}],
  ["wrong granularity", r => {r.source_reports.app_store_discovery_and_engagement_standard.granularity = "WEEKLY";}],
  ["invalid processing date", r => {r.source_reports.app_store_discovery_and_engagement_standard.processing_date = "2026-02-30";}],
 ];
 for (const [name, mutate] of mutations) {
  test(`refuses ${name} instead of planning a numerical signal`, () => {
   const rows = selectedRows(); mutate(rows[0]);
   const plan = planFunnelSeriesApplications(apps, rows, options);
   expect(plan.commands).toHaveLength(0); expect(plan.skipped[0].reason).toContain("impressions");
  });
 }
 test("signed explicit USD money remains eligible", () => {
  const rows = selectedRows(); for (const row of rows) row.proceeds_usd = -2.5;
  const plan = planFunnelSeriesApplications(apps, rows, {...options, metrics:["proceeds_usd"]});
  expect(plan.commands).toHaveLength(1);
 });
 test("metric selection has no implicit default or unmatched conversion substitution", () => {
  for (const metric of ["product_page_conversion_rate", "app_store_product_page_conversion", "conversion_rate"]) {
   expect(() => parseMetricList(metric)).toThrow("unmatched");
   expect(() => planFunnelSeriesApplications(apps, selectedRows(), {...options,metrics:[metric]})).toThrow("unmatched");
  }
  expect(() => parseMetricList(undefined)).toThrow("required");
  expect(() => parseMetricList("paying_users")).toThrow("unsupported");
 });
 test("strict real calendar dates do not normalize February30", () => {
  const rows=selectedRows(); rows[0].date="2026-02-30";
  expect(() => planFunnelSeriesApplications(apps, rows, options)).toThrow("date");
 });
 test("derived windows still require all selected metrics on all days", () => {
  const plan=planFunnelSeriesApplications(apps, selectedRows(), {...options,beforeWindow:undefined,afterWindow:undefined,afterEnd:"2026-07-10",days:1,metrics:["impressions","product_page_views"]});
  expect(plan.before).toBe("2026-07-09/2026-07-09"); expect(plan.after).toBe("2026-07-10/2026-07-10");
  expect(plan.commands.map(c=>c.metric)).toEqual(["impressions","product_page_views"]);
 });
 test("apply reaches real Go consumer idempotently without promoting claims", () => {
  const home=temporary.dir("asc-v2-home-"); const bin=join(temporary.dir("asc-v2-bin-"),"compound");
  const build=spawnSync("go",["build","-o",bin,"./cmd/compound"],{encoding:"utf8"});
  if(build.status!==0) throw new Error(build.stderr||build.stdout);
  const env=initializeCompoundHome(bin,home);
  const args=["tools/asc_funnel_series_bridge.ts","--config","fixtures/product/asc-products.json","--funnel-series",seriesPath,"--change-event","Synthetic change.","--before",options.beforeWindow,"--after",options.afterWindow,"--metric","impressions","--compound-bin",bin,"--apply","--json"];
  for(let i=0;i<2;i++) {
   const run=spawnSync(process.execPath,args,{encoding:"utf8",env}); if(run.status!==0) throw new Error(run.stderr||run.stdout);
   const report=JSON.parse(run.stdout);expect(report.planned).toBe(1);expect(report.applied).toBe(1);
   expect(report.apply_results[0].signal_count).toBe(1); expect(report.apply_results[0].transition_count).toBe(1);
   const timeline=spawnSync(bin,["product","timeline","product:focus-notes"],{encoding:"utf8",env});
   expect(timeline.status).toBe(0); expect(timeline.stdout).toContain("app_store_connect_impressions 300 -> 400");
   expect(existsSync(join(home,"ledger","claims.jsonl"))).toBe(false);
  }
 },15000);
});
