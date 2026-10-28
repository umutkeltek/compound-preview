# macos-activity — Tier-0 ambient-activity collector

A ~200-line Swift launchd agent that emits a **content-free** NDJSON stream
(`compound.activity_observation.v0.2`) of app focus, idle, and lock transitions. It is the
replaceable half of compound's activity sensor: the collector observes, the compound membrane
(`compound activity drain`) validates, classifies, and persists. The collector holds zero
authority — no `COMPOUND_HOME` access, no network, and only zero-permission APIs (no
Accessibility or Screen Recording prompts).

## What it emits (and what it never captures)

Emitted kinds: `app_activated`, `idle_started`, `idle_ended`, `screen_locked`,
`screen_unlocked`, `capture_gap`. Fields: monotonic `seq`, RFC3339 `ts`, `bundle_id`,
`app_name`, engine identity. Coverage honesty: a running marker + persisted `seq` detect
unclean shutdown, and the next start emits `capture_gap` — a silent outage never reads as
inactivity.

Never captured, by schema (the membrane refuses unknown fields): window titles, URLs, document
names, keystrokes, clipboard, screenshots, audio, message bodies.

## Build

```sh
make build            # swiftc -O -o compound-activity-sensor main.swift
```

## Install (launchd)

```sh
install -m 0755 compound-activity-sensor "$HOME/.local/bin/"
sed "s|\${HOME}|$HOME|g" com.compound.activity-sensor.plist.template \
  > "$HOME/Library/LaunchAgents/com.compound.activity-sensor.plist"
launchctl load "$HOME/Library/LaunchAgents/com.compound.activity-sensor.plist"
```

Spool output: `~/Library/Application Support/compound-activity/spool/<YYYY-MM-DD>.ndjson`
(0600, day-rotated). Ingest with `compound activity drain` — drain is grant-gated and
fail-closed; the spool is inert until the operator enables the sensor family.

## Uninstall

```sh
launchctl unload "$HOME/Library/LaunchAgents/com.compound.activity-sensor.plist"
rm "$HOME/Library/LaunchAgents/com.compound.activity-sensor.plist" \
   "$HOME/.local/bin/compound-activity-sensor"
```
