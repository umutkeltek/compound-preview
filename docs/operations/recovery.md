# Backup, restore, upgrade, and rollback

The public alpha supports recovery for the file backend on one local disk. A
backup is useful only after `home verify` accepts it and a restore drill has
proven that a new home can consume it. Keep the archive outside the source home
so the backup cannot include or overwrite itself.

Quiesce the home's writers before a consistent backup, including scheduled jobs and
hook telemetry; admission locking alone does not stop an already-running writer.
If an entry grows or shrinks during copying, backup refuses publication and names
its relative path. Preserve that error, identify the writer, and settle it through
its existing owner before retrying. Do not treat a partial archive as a restore point
or bypass the backup owner with an unverified live-directory copy.

## Create and verify a backup

```sh
compound home doctor --home "$COMPOUND_HOME"
compound home backup --home "$COMPOUND_HOME" --target /safe/local/path/compound-home.tar.gz
compound home verify /safe/local/path/compound-home.tar.gz
```

Do not prune the last verified local copy merely because no remote destination
is configured. Keep at least one verified archive whose survival does not
depend on the home being backed up.

### Populated homes and the streamed profile

The default backup format remains v0.1: at most 250,000 logical tar members
(files, directories and metadata) and one manifest of at most 64 MiB. A writer
budget refusal publishes no archive. It is not permission to prune history or
to increase a verifier's limits.

For a larger home, explicitly select the bounded v0.2 profile:

```sh
compound home backup --home "$COMPOUND_HOME" --format streamed-v2 --target /safe/local/path/compound-home-v2.tar.gz
compound home verify /safe/local/path/compound-home-v2.tar.gz
```

`--format v1` preserves the default. Unknown or missing format values refuse.
The streamed format has a small leading version descriptor, bounded chunks of
file checksums, and a final index binding those chunks. Verification and restore
recognize that descriptor; an archive cannot supply its own resource limits.
Chunking does not waive checksums, duplicate-path checks, source identity,
staged-file verification, absent-target publication or writer quiescence.

Retain the exact recovery binary that produced and verified a streamed archive
alongside the archive's evidence. Older binaries do not understand v0.2. Use
the retained compatible recovery binary for the absent-target restore before
starting a prior application binary against the restored home. Changing the
archive format does not change the home schema or authorize a live migration.

A header inventory, successful archive write or synthetic scale test is not
populated-home recovery acceptance. The actual archive must pass verification,
an isolated restore, home health checks and a real restored-home consumer.

On macOS, streamed restore syncs each file to the device while staging remains
private and unpublished. It then requires a checked full-device flush after
directory sync and staged verification, before publishing the new home. A second
checked flush follows the rename and parent-directory sync. All staged entries
must remain on the same device; mounted subtrees are refused. The destination
filesystem must support `F_FULLFSYNC`: unsupported or failed flushes are errors,
not a fallback to weaker durability. A post-publication error preserves the new
target for manual verification and must not trigger an automatic repeat.
Unpublished staging is not a resumable recovery checkpoint. Keep it free of
concurrent writers. Legacy archives and other platforms retain their existing
sync protocol; these checks do not simulate physical power failure.

### Link-preserving backups

For a home containing symbolic links, select `--format streamed-v3` (v0.3).
The legacy v0.1 writer omits non-regular entries, including links; its successful
verification does **not** prove link custody. The v0.2 writer refuses included
links. Do not delete links or use v0.1 to bypass a v0.2 refusal.

The v0.3 profile retains the v0.2 resource limits and stores at most 4,096 links
as checksum-bound records in the existing 1 MiB final index. That byte limit can
refuse an archive before the count limit. Links count as logical members and in
the reported file count. Bodies are never read through them; tar link headers
remain forbidden. Each record preserves the original target spelling and a
portable target. Canonically spelled in-home absolute targets are rebased to
relative targets so an isolated restore cannot point back to the source home.
Canonical relative targets, including dangling and cyclic links, remain links;
this does not repair an already-broken target. Escaping targets, noncanonical
spellings (including trailing slashes), ambiguous parent traversal, missing or
symbolic parents, path collisions, non-UTF-8 link metadata and oversized paths
are refused. A link to a body excluded by the backup habitat remains a link;
its presence does not prove that excluded body was archived.

Restore validates the metadata, extracts regular bodies first, creates links
last and rechecks the staged inventory and link targets before publication.
No link is followed during backup or restore. Retain a v0.3-compatible recovery
binary with a v0.3 archive; older binaries cannot restore it. The original
absolute home and target spellings are private archive metadata, not public
release evidence. This format preserves link identity and target semantics,
not extended attributes or original link timestamps.

## Restore drill

Restore into a new, absent target. Never restore over the active home.

```sh
restored_home=/safe/local/path/compound-restored
compound home restore /safe/local/path/compound-home.tar.gz --target "$restored_home"
COMPOUND_HOME="$restored_home" compound home doctor --json
COMPOUND_HOME="$restored_home" compound workbench --json
```

The restore command verifies the pinned archive, extracts into private staging,
runs initialization and health checks there, and publishes only a validated
home to an absent target. A failed or ambiguous publication is not success;
preserve its output and the source archive for diagnosis.

Readonly directories stay owner-writable in the private staging tree until child files, links and
required habitat directories are complete. Their archived final permissions are then restored,
children first, before rooted health, staged checksums, durability and publication checks. Original
source permissions are never changed to make a restore pass.

## Upgrade

1. Record `compound version --json` and the current home manifest.
2. Create and verify a backup using the commands above.
3. Install the immutable tagged artifact and verify its checksums.
4. Run `compound home doctor --json` before any other mutation.
5. Exercise a synthetic read/write/proof flow inside the supported scope.
6. Keep the prior binary and verified backup through the observation window.

An unsupported schema or home kind is a stop condition. Do not edit the
manifest by hand to silence the check; use the migration command documented by
the release, or roll back.

## Roll back

1. Stop active Compound and console processes using the home.
2. Preserve the failed-upgrade home for forensic comparison; do not overwrite
   or delete it.
3. Reinstall the prior verified binary.
4. Restore the pre-upgrade archive to a new target.
5. Point `COMPOUND_HOME` at the restored target and run `home doctor` plus the
   same synthetic flow used before the upgrade.
6. Switch normal use only after those checks pass.

An older binary is not expected to read a newer manifest schema. Rollback is a
binary-plus-backup operation, not an in-place downgrade.

## Evidence to retain

- binary version and full build commit;
- archive path and `home verify` result;
- source and restored manifest identities;
- `home doctor --json` output for both homes;
- the synthetic consumer command and its observed result;
- any ambiguous publication or filesystem error.

Recovery never authorizes deletion of the original home or archive. Deletion
is a separate operator decision after recovery is proven.

## Recover an abandoned private artifact stage

`home doctor` reports an unexpected `.artifact-*` name in
`private/artifacts/sha256` as manual. A stage can contain unique bytes even when
no digest object exists. Preserve it. Quiesce older Compound binaries and other
writers before the first recovery with a binary that uses the private artifact
directory lock; older binaries do not participate in that lock. Stage age alone
does not establish that a writer has stopped.

For each exact stage, independently obtain its SHA-256 using a read-only hashing
tool and construct `artifact:sha256:<64 lowercase hex>`. Review the exact stage
name and expected reference, then replace the example name and digest below
with those values and run the owner command:

```sh
compound home private-custody repair --home "$COMPOUND_HOME" \
  --recover-stage '.artifact-123456789=artifact:sha256:<64 lowercase hex>' \
  --dry-run --json
compound home private-custody repair --home "$COMPOUND_HOME" \
  --recover-stage '.artifact-123456789=artifact:sha256:<64 lowercase hex>' \
  --json
compound home doctor --home "$COMPOUND_HOME" --json
```

Repeat `--recover-stage NAME=REF` to select multiple exact stages in one call.
Malformed, duplicate, or conflicting selections fail before repair. The owner
hashes through a pinned private path, checks ownership, mode, link count, and
expected digest, publishes without replacing an existing digest, and removes
only a selected stage after the digest is verified and durably linked. If a
partial publication or filesystem failure occurs, retain the report and retry
the same selection; the owner recognizes a linked stage or an already verified
digest. A successful recovery preserves the bytes observed in the stage. It
does not prove that the interrupted `PutPrivate` call finished writing its
intended body or attach the digest to a source manifest.

This procedure assumes a trusted local home and quiesced old writers. The
directory lock coordinates cooperating binaries; it cannot exclude a hostile
same-UID process that swaps a path between final validation and unlink.
