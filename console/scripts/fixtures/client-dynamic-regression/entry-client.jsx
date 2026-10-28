/* A leading comment must not hide the directive prologue. */
"use strict";
"use client";

export async function loadCapabilityBridge() {
  return import("./capability-bridge.mjs");
}
