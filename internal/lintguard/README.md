# lintguard

`go test` guard against new silent error paths in the backup data path (ADR 0009 phase 0, #326).

- `guard.go`: syntactic scanner for `_ = call(...)` and `if err != nil { log... }` with a log-only body.
- `ignored_errors_test.go`: scans `internal/{tasks,replication,binlog,upload}` and fails on any site not in `allowlist.go` at its reviewed count.
- `allowlist.go`: reviewed sites keyed `file:func:kind`, each with a reason. Entries marked `follow-up:` are known gaps for phase 1.
- `guard_test.go` + `testdata/bad`: proves the scanner finds both shapes and ignores handled errors.

Not covered (by design, syntactic only): `v, _ := f()` blank error results, and errors dropped by not assigning a call's result at all. The full `errcheck`/`go vet` CI step needs a workflow edit and is tracked in the token-gaps handoff.

When the test fails: report a finding, return the error, or add the site to `allowlist.go` with a reason in the same PR.
