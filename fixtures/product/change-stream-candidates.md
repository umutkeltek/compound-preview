# Change Stream Promotion Candidates

Generated: 2026-06-30T03:36:35Z
Source capture: `/tmp/change-stream/2026-06-30-015127.json`

These are candidates, not truth. Promote only if change is durable and measurable.

## 1. focus-notes — App Store screenshots

- Kind: repo-signal
- Score: 5 (mapped-app:focus-notes, surface:App Store screenshots, dirty:3)
- Summary: app-store-screenshots on main: dirty 3, recent commits 0
- Repo: `/tmp/products/focus-notes/app-store-screenshots` branch `main`
- Dirty preview:
- ` M package.json`
- ` M app/page.tsx`
- `?? public/screenshots/`
- Promote if real:

```bash
log_app_change.py focus-notes '<change-type>' '<describe durable change>' --surface 'App Store screenshots' --expected '<expected effect>' --measure-after-days 7
```

## 2. focus-notes — App Review/release

- Kind: repo-signal
- Score: 4 (mapped-app:focus-notes, surface:App Review/release, dirty:2, large-dirty-surface-needs-review)
- Summary: app-ios on main: dirty 2, recent commits 0
- Repo: `/tmp/products/focus-notes/app-ios` branch `main`
- Dirty preview:
- ` M docs/release/v1.0.2/asc-submission-copy.md`
- `?? .asc/`
- Promote if real:

```bash
log_app_change.py focus-notes '<change-type>' '<describe durable change>' --surface 'App Review/release' --expected '<expected effect>' --measure-after-days 7
```
