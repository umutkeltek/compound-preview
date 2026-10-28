# Synthetic Compound lifecycle reproduction

Synthetic local demonstration; no independent user or customer validation is claimed. The sample evidence records one historical maintainer-operated run.

This example uses a real installed Compound binary and a synthetic local Node command. It demonstrates rejected launch admission, governed execution, exact output acceptance, and durable work history. It makes no model call.

Requirements: Node 20 or newer, an installed Compound binary built from the reported candidate, and a supported local file-backed single-user environment. The historical sample was executed on Darwin/arm64. Other platforms need their own native execution evidence. Installation and release authenticity are separate checks; this example does not download, install, or authenticate a release.

Run from this directory, supplying your own absolute installed binary path and an absent output directory:

```sh
node reproduce.mjs /absolute/installed/compound /absolute/absent-demo-run
```

Use a fresh output directory for each run. The script creates an isolated COMPOUND_HOME inside it and never uses the default runtime home. It does not change shell configuration, connect accounts, enroll a provider, or publish. Successful execution prints a report and exits 0. Any failed assertion exits nonzero.

The script initializes the home, orients the synthetic workspace, creates and claims one work item, and attempts an unsupported harness. The refusal must leave both the entire WorkEnvelope and attempt inventory unchanged. It then executes a supported local command through work advance --execute. The script independently checks the exact file bytes, the successful linked RunAttempt, completed work state, and proof-chain doctor before accepting the demonstration.

Files named 01-*.json and subsequent numbered files retain actual CLI output. The home retains ActionIntent, PolicyDecision, RunAttempt, Receipt, and Transition records. These raw files can contain local paths and are private. evidence.json is a deliberately small projection; evidence.schema.json documents its shape. sample-evidence.json is the observed sanitized projection from one run.

A receipt records execution history; it does not prove a product improvement or automatic learning. Closing uses audit_only for this reason. Rejecting an unsupported adapter is an admission refusal, not evidence of recovery after interruption or an external side effect.

To inspect a run from a fresh terminal, set COMPOUND_HOME to its home subdirectory and invoke the same binary:

```sh
COMPOUND_HOME=/absolute/demo-run/home /absolute/installed/compound work show work_synthetic_reproduction --json
COMPOUND_HOME=/absolute/demo-run/home /absolute/installed/compound run-attempt list --json
COMPOUND_HOME=/absolute/demo-run/home /absolute/installed/compound work proof-chain-doctor --json
```

Retain the run directory until review is complete. This package has no cleanup command.
