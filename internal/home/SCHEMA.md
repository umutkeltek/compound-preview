# Home manifest schema

## `compound.home.v0.1`

The writer and reader currently support exactly the fixed identity pair:

```yaml
schema_version: compound.home.v0.1
kind: compound_home
```

`home_kind` is required profile and placement metadata, not an identity
discriminator. Historical profiles such as `global`, `local`, `test`,
`local_monolith`, `experiential-smoke`, and harness-specific fixture names stay
valid; current readers accept any nonblank profile without normalizing it.

Homes written by early v0.1 binaries may omit `kind`. A kind-less v0.1
manifest with a nonblank `home_kind` is explicitly legacy-compatible:
`LoadManifest`, `Init`, `Doctor`, backup, and restore normalize its fixed kind
only in memory and preserve the manifest bytes exactly. They never silently
rewrite it. `DiscoverAncestorHome` remains deliberately narrower and recognizes
only historical local markers because it is a non-authoritative discovery
bridge, not runtime identity validation.

A `public_fixture` archive is deliberately not a runtime home. Its sanitized
`manifest.yaml` entry is export metadata, and backup restore rejects the
archive kind before staging; `public_fixture` is not added to the runtime kind
set above.

`Init` validates its requested nonblank profile before creating a path. For an existing
home it validates the manifest before any filesystem mutation, then reads and
validates the same bytes through the pinned home root before creating missing
habitat. It never rewrites an incompatible manifest or adopts a populated
manifestless directory. A fresh identity is written and synced to a rooted
same-directory staging file, published with an atomic no-replace link, synced,
and byte-revalidated before habitat scaffolding. An interrupted owned staging
file does not become authority or block a retry: retry reclaims only a stable,
regular, current-UID staging entry through rooted validated removal. Recovery
also recognizes that exact staging name followed by one or more
`.remove-pending` suffixes and requires coexisting names in a removal chain to
identify one inode. Fresh publication remains stricter and never treats those
quarantine names as ignorable root content. A symlinked, foreign-owner, or
raced staging entry fails closed and remains for inspection. Quarantine
publication and restoration are atomic no-replace operations, so a competing
file is never overwritten; arbitrary root content still blocks adoption and is
never removed.

Every non-read CLI admission checks the selected home before dispatch or hands
that check to a narrower protocol boundary. Held, governed, install, and
mutating commands refuse an incompatible or missing manifest before their first
write. Admission consumes `home` option values with the same grammar as the
handler, matches read-only flags as exact case-sensitive tokens, and exempts help only where a
specific handler proves it returns before mutation. Runtime hook commands are
the narrow exception: they must still emit a
protocol-shaped fail-open or deny response, so they run without a writable home
when compatibility fails and never persist into that home. `home init` and
`home restore` retain their own fresh-target compatibility boundaries, and
read-only recovery diagnostics remain available. Compatibility failures expose
a typed surface (`home_manifest` or `archive_envelope`) plus recovery guidance.

Archive creation pins and decodes the exact manifest bytes before walking the
home, writes those pinned bytes into the archive, and derives envelope metadata
from the same decoded body. Verification checks both the envelope identity and
the exact embedded manifest; a compatible envelope cannot mask an incompatible
embedded home.

There is no automatic migration. Before migration, use a Compound version that
supports the existing home to run `compound home backup --target <archive>` and
`compound home verify <archive>`, preserve the original home, and rehearse
how to roll back with that supporting version's `compound home restore
<archive> --target <new-absent-home-path>`. A future compatible schema change
must add an explicit read/migration decision and update this file in the same
change.
