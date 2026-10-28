# Scheduled SSD guardians

The existing daily, budget, pressure and weekly jobs now measure, report and defer. `--apply` only publishes an append-only receipt; `--force`, zero caps/ages, manifest policies and environment variables cannot authorize removal or relocation. There is no automatic delete, Homebrew/Cargo cleanup, simulator erase, model unload, cache merge/link replacement or snapshot thinning path. All capture, session, model, database, backup and staging contents remain in place.

Measurements resolve configured root aliases and count each physical root once per report. Allocated bytes are not exclusive APFS extents and overlapping parent/child rows must not be summed. Failed/truncated scans are failures with unknown size, never empty successes. Recurring reports claim zero reclaim; concurrent free-space changes are unattributed. Exit 2 indicates measurement failure or pressure, exit 3 indicates an owner-review deferral, and exit 0 indicates a completed observation without such conditions. Existing schedules provide the next observation; a retry does not grant disposal authority.

The weekly driver never forwards `--apply` to children. A fresh backup is an observation, not disposal permission. Capture database auditing is deferred to Switchback because the existing direct audit can recover SQLite WAL state. Scheduled work does not open it.

Exact approved manual cleanup batches remain a separate owner operation requiring a sealed manifest, current identities, custody checks and active-use exclusion. A future cleaner is not enabled by these files or a config change. No new service or schedule is introduced.

Runtime activation requires delivery of the seven files (`report_only.ts` plus the six entrypoints) together through the canonical source owner. No native binary, browser, install or enrollment change is included. Source rollback cannot restore erased data and must not silently re-enable the previous destructive scheduler.

`bun test tools/ssd_guardian_report_only.test.ts scripts/ssd-guardian` exercises synthetic fixtures. The boundary test is in `tools/` so the existing canonical Bun gate consumes it. Its child preload denies unlisted processes, SQL opens and all non-report writes; mixed cache/model/session/backup/capture sentinels remain unchanged under real entrypoint `--apply` runs. Never test these entrypoints against user paths.
