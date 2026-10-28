#!/usr/bin/env bash
# Retired: this historical entrypoint mixed verification, direct binary
# publication and live-home mutation. It must never perform those effects.
printf '%s\n' \
  'release-smoke.sh is retired; no binaries or runtime state were changed.' \
  'For candidate verification, use compound proof-check via skills/compound-verify.' \
  'For installation, use the verified release installer or install.sh, after quiescing consumers.' \
  'Read docs/getting-started.md and docs/operations/recovery.md before activation or rollback.' >&2
exit 2
