# Compound source preview

Compound keeps work context, bounded execution and inspectable results together.
This is a source-only developer preview for one trusted operating-system user.

Start with the [synthetic reproduction](examples/synthetic-lifecycle/README.md),
then read the [founder-operated experiment](case-study/CASE_STUDY.md). The
experiment found no acceptance or cost advantage from the added reviewed context.
It establishes no performance, ROI or customer-demand claim.

## Build and run the local example

Requirements: Git, Go 1.27.1 or newer, Node 20 or newer, and a supported local
file-backed environment. Go may download the declared public modules when they
are not cached. This example makes no model call and needs no provider account.

```sh
git clone https://github.com/umutkeltek/compound-preview.git
cd compound-preview
compound_preview_run="$(mktemp -d "${TMPDIR:-/tmp}/compound-preview.XXXXXX")"
compound_preview_source="$(git rev-parse HEAD)"
go build -trimpath -ldflags "-X main.buildSHA=$compound_preview_source" \
  -o "$compound_preview_run/compound" ./cmd/compound
go build -trimpath -o "$compound_preview_run/compound-breadth-engine" ./cmd/compound-breadth-engine
PATH="$compound_preview_run:$PATH" node examples/synthetic-lifecycle/reproduce.mjs \
  "$compound_preview_run/compound" "$compound_preview_run/demo"
```

The example uses a fresh isolated `COMPOUND_HOME` under the run directory. It
checks a refused launch, governed execution, exact result bytes and the resulting
work proof chain. Successful execution prints JSON; failed assertions exit
nonzero. Keep raw run directories private because they contain local paths.
The [sample evidence](examples/synthetic-lifecycle/sample-evidence.json) is a
sanitized historical example, not proof that every checkout has been tested.

## What was tested

The prior H10 binary source revision
`83fbadc452fd7bcc61fd7e464313fcf8902f329f` passed native Darwin/arm64 and
Linux/amd64 installed acceptance: 84 steps and 11 named gates on each platform.
Its continuity worker was synthetic (`native_inference=false`).

This snapshot derives from source revision
`7a617d720cd2c510dfcb074378a97671fad7b447` (tree
`f45603c6d89dd91f2684f5b3933e74485eab73d0`). Its 3,521 source files include
16 changed fixture, test and comment files relative to the historical H10
source, covering privacy fixture cleanup and a cancellation-test fixture repair.
The public snapshot contains 3,531 regular files, including the reviewed
source-preview documentation, demo and case-study overlay.
Raw source `git archive` SHA-256: `5c480189a1a4f9ac6ec59c53f52e74619098f35963be3abc2cfa8147b2b26f34`.

Those are distinct source identities. H10 evidence does not certify binaries built
from this snapshot. Exact provenance is in [SOURCE_PROVENANCE.json](SOURCE_PROVENANCE.json).

There is no supported signed binary release for this source preview. Follow the
source-build route above; upstream reference pages do not establish a release
download or verified reporting route for this repository. The original project
overview is preserved as [UPSTREAM_README.md](UPSTREAM_README.md).

## Scope and reporting

The supported preview boundary is local files and one trusted user; shared homes,
remote console exposure and unattended authority changes are outside this example.
Read [source-preview reporting](SOURCE_PREVIEW_SECURITY.md) before sharing a bug.
GitHub workflow files are preserved as upstream references; publication requires
repository Actions to be disabled and verified before the first source push.

The source is under [Apache-2.0](LICENSE). Tracked
[source credits](skills/product-pipeline/SOURCES.md) remain included; separately
downloaded dependencies retain their own licenses.
