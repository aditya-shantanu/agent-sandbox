# P4 split (2026-07-24): #1256 → #1256 (status half) + #1277 (adoption half)

User decision, executing igooch's round-2 scope preference (threads :80 and
:1162 on #1256): split the combined optimistic-lock PR at the boundary she
named. No stacking — #1277 is based directly on upstream main (`aee6d3b`).

## Boundary

**#1256 keeps (the #940 + stale-overwrite fix), head `1770166`:**
- optimistic-locked claim STATUS patch (`MergeFromWithOptimisticLock`) with
  the benign 409 drop + the round-2 comment fixes (409 provenance is any
  concurrent writer; the drop's dependency on `getTimingPredicate` returning
  true for every update, noted at both ends)
- `updateStatus` authoritative-bool + metrics-only-when-authoritative gate
  (#940 exactly-once) and its tests (`StatusPatchCarriesOptimisticLock`,
  `StaleStatusPatchConflictDroppedWithoutMetrics` + `histogramSampleCount`)
- `retryAdoptionAnnotation` in-pass 409 retry on a fresh base + its test
  (`AdoptionConflictRetriedInPass`), the `APIReader` wiring, and the shared
  helpers (`updateClaimOnFreshBase`, `authoritativeReader`)
- terminal annotation-write conflicts surface as plain reconciler errors
  until #1277 lands (the sentinel left with the other half)

**#1277 gets (the #418-class fix), branch
`upstream-p4b-adoption-assignment-lock`, head `efeb705`:**
- `completeAdoption` optimistic lock; `resolveAdoptionCompletion` +
  `removeAssignedSandboxAnnotation` (expanded reader-fallback preambles,
  duplicated locally per the no-stacking rule); no-fall-through at both call
  sites; `errAdoptionConflict` sentinel + `AdoptionConflict` Ready reason
- flip regression tests (`...ConflictDoesNotSwitchCandidates`,
  `...StaleAdoptionRepatchIdempotentWithoutWrite`,
  `...AssignedSandboxDeletedTerminalCleanup`) + the 4 cache-lag test reworks
  (rawClient/interceptor split, `APIReader`, RV pinning) + the same-candidate
  adoption case
- body evidence: isolation-leg RCA numbers (flips 340→0, 323 rebinds, 74
  Ready regressions, PUT 409 10,880 / PUT 404 8,200 churn, APIReader &
  workqueue exonerated) — measured on THIS half's fix specifically
- Fixes #418, Ref #478; #1129 complementarity paragraph carried over

## Notes

- Both PRs validate independently (build / vet / gofmt / `go test
  ./extensions/... -race` / lint-go 0 issues).
- Expected: small mechanical conflict for whichever merges second (adjacent
  hunks in `sandboxclaim_controller.go`; both wire `APIReader` in main.go and
  the struct). Noted in both PR bodies.
- #1256 body re-scoped honestly: burst/sustained parity numbers were
  measured on the combined pre-split branch and are labeled as such; the
  flip forensics moved to #1277.
- igooch threads :80 and :1162 replied + resolved with the split links.
