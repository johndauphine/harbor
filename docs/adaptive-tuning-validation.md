# Adaptive transfer tuning validation

Date: 2026-10-16. Feature branch: `codex/adaptive-transfer-tuning`.

## Real AWS comparison

One Mac with 18 logical processors and 48 GiB installed RAM, using the existing AWS S3 connection in us-east-1. Three rotated 1 GiB upload/download round trips per mode through Harbor’s staging, transfer engine, and checkpoint journal. Downloaded data was independently checked with SHA-256. Observe is the fixed eight-request control. Runs did not use race instrumentation. Browser file reading is outside these timings.

| Controller start | Mode/direction | Trials | Median MiB/s | Range MiB/s | Peak Go heap MiB |
|---|---|---:|---:|---:|---:|
| 4 (rejected) | observe-upload | 3 | 78.3 | 70.4–90.2 | 7.8 |
| 4 (rejected) | auto-upload | 3 | 47.8 | 38.2–79.2 | 6.3 |
| 4 (rejected) | observe-download | 3 | 80.9 | 39.0–84.5 | 6.5 |
| 4 (rejected) | auto-download | 3 | 67.7 | 63.1–74.8 | 5.8 |
| 8 (current) | observe-upload | 3 | 77.1 | 77.0–86.8 | 7.2 |
| 8 (current) | auto-upload | 3 | 83.4 | 56.4–84.5 | 7.3 |
| 8 (current) | observe-download | 3 | 81.0 | 66.9–86.5 | 6.6 |
| 8 (current) | auto-download | 3 | 86.8 | 78.2–89.4 | 6.4 |

Starting at four requests reduced throughput in these short transfers, so Automatic now starts at the previously validated eight-request limit. Resource pressure or provider throttling can lower activity; subsequent growth is measured and bounded at eight. All 12 round trips across the initial and revised comparisons passed SHA-256. The earlier bucket was independently checked absent with HeadBucket returning 404, and the revised test verified 404 after cleanup.

The revised Automatic and Observe settings both stayed at eight in this unthrottled comparison. Their different medians reflect network variation and do not demonstrate that adaptive control increases peak throughput. These trials support avoiding the earlier cold-start penalty. They do not establish performance on constrained machines, shaped networks, mixed queues, or real provider throttling. Raw samples and logs remain under the ignored `dist/aws-adaptive/` and `dist/aws-adaptive-eight/` directories.

## Recovery and failure coverage

- Real MinIO adaptive recovery: interrupt uploads and downloads after verified progress, reduce the live request ceiling to one, shut down and reconstruct Harbor, then Retry. Check reused progress, complete contents, protected pre-existing destinations and no leftover multipart upload or scheduler leases.
- The final full race-enabled suite with the real MinIO binary passed in 108.387 seconds, including process termination, shutdown, cancellation, retry, deletion, adaptive recovery, and checkpoint failure scenarios under the new default.
- Synthetic real-SDK tests cover small-transfer admission, 503 followed by a successful retry, truncated bodies, cancellation of a stalled unread response, and permit release.
- Controller and scheduler tests cover scoped backoff, resource reductions and measured recovery, probe rollback, cancellation, queue saturation, independent control admission, and mode changes.
- Frontend tests cover launch override, failed refresh, failed mode save rollback, provider backoff explanation, and missing available-memory measurements. Actual browser checks verified the launch override, Automatic default, GUI opt-out, and opt-out persistence across an app restart.

## Independent review

Two independent agents reviewed the complete feature diff and re-reviewed fixes. Resolved findings included shared queue saturation blocking control traffic, provider backoff being described as learning, failed setting changes showing the wrong selection, conflicting probes, response-body lifecycle handling, and inaccurate useful-byte wording. Neither final review found an unresolved blocking issue.

Explicit limitations: request fairness applies among up to eight active transfers; a ninth waits for an active transfer to finish. Resource measurements exclude total process RSS, container limits, and the chosen download destination’s free space. Available RAM is unknown on this Mac. The mode persists; learned controller state does not. Cancellation and recovery formats retain existing behavior.

## Final checks

- `HARBOR_MINIO_BINARY=/private/tmp/harbor-ui-fixture/minio go test -race -count=1 -timeout 8m ./...`: passed, 108.387 seconds.
- `HARBOR_GIB_TEST=1 go test -race -count=1 -timeout 20m -run 'Test(StreamingMultiGiBTransfers|UploadSizeLimitRejectsBeforeStaging)$' .`: passed, 20.920 seconds. Exercises a transfer above 2 GiB, truncated download, cancellation, checksums, and staging rejection.
- `node --test tests/*.test.cjs`: all 74 passed, including the native headless Chromium form test; no skips.
- `go vet ./...`, third-party notice verification, three packaging-script unit tests, and whitespace checks passed.
- Builds passed for macOS ARM64, Windows AMD64/ARM64, and Linux AMD64. Cross-builds do not establish interactive runtime coverage on those operating systems.
- Actual browser smoke test used an isolated database and the final feature binary; the existing user configuration was untouched during the test.

These results precede GitHub CI and merge. No unrelated credential-test changes are included in this feature.
