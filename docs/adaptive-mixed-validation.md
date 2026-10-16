# Mixed-transfer adaptive tuning validation

This follow-up uses disposable local MinIO servers, synthetic credentials, isolated profiles and journals, and real S3 requests through HTTPS fault-injection proxies. It does not access saved user connections or storage. Network shaping is local and controlled; these are correctness and recovery checks, not WAN or AWS performance benchmarks.

## Problems found and corrected

1. **Slow requests were mistaken for idle time.** A gap greater than 30 seconds between completions reset the learning history even while a request was actively transferring. Idle detection now considers the interval before the next request started. A deterministic regression covers 35-second parts, useful upward recovery, and a genuine idle gap. The real slow-part regression also failed against an isolated copy of the original controller: a roughly 32-second completion gap discarded its active learning window.
2. **A concurrency probe could be judged before its work completed.** Four acknowledged completions could come from requests already in flight before the increase. The controller now excludes pre-probe operations, warms a fresh measurement window, and measures at least three times the current request limit in useful completions, with a minimum three-second interval. A late old operation restarts warmup. Safety feedback, including throttling, still takes effect immediately. Regression traces cover beneficial increases and plateaus for four-to-five and seven-to-eight requests, with completion timing jitter.

## Live scenarios

- **Competing paced transfers:** three distinct 1 GiB uploads and three distinct 128 MiB downloads, each request paced to 4 MiB/s. One actual 503 SlowDown response tests upload backoff and download isolation. Downloads finishing releases capacity for a measured upload recovery probe. The scenario interrupts both an upload and a download, explicitly retries them, and checks reused verified progress and the old download destination. Different-object data streams must be active at the actual interruption and retry events.
- **Very long requests:** a distinct 256 MiB upload with each real UploadPart held for 32 seconds and one actual 503 response. The test requires learning to survive completion gaps longer than 30 seconds while demand remains, then verifies the object and cleanup. Normal low-demand tail resets are permitted.
- **Integrity and limits:** file contents differ between files and between blocks within each file. Independent SHA-256 checks detect cross-file swaps, duplicated parts and reordered parts. Proxy body counters end on EOF/Close, rather than server-handler return, and scheduler observations check the common eight-request budget. Every test checks for owned unfinished multipart uploads and admission leaks; the mixed scenario also checks temporary download cleanup.

A symmetric experiment with equally long uploads and downloads did not demonstrate accepted recovery: increasing the upload allowance offered no additional capacity while both directions shared the global budget, and early measurement could also include the prior cohort. The final asymmetric workload supplies sustained upload demand after downloads finish; this is an explicit change in demand, not a claim that tuning creates bandwidth or overrides fair sharing.

## Reproduce

```sh
HARBOR_MINIO_BINARY=/path/to/minio \
HARBOR_ADAPTIVE_MIXED_MINIO=1 \
HARBOR_ADAPTIVE_SLOW_MINIO=1 \
go test -race -v -count=1 -timeout 10m \
  -run '^TestMinIOAdaptive(MixedTransfers|SlowPartContinuity)$' .
```

The tests use real elapsed time and production Automatic mode. They do not accelerate controller clocks or force resource ceilings. Several minutes of sustained traffic are deliberate: cooldown and fresh observation windows need enough completed work. Short transfers may finish before upward tuning can be measured. Resource detection and the eight-active-transfer fairness limitation are unchanged.

## Recorded checks

- The full existing real MinIO regression suite passed with race instrumentation in 102.984 seconds against the final production fixes. The two longer live scenarios are separately enabled by the flags above.
- The complete ordinary Go race suite passed in 15.985 seconds; static checks passed. All 74 frontend regressions passed. Windows AMD64 and Linux AMD64 cross-builds passed; this does not establish native runtime coverage on those systems.
- Repeated controller/stream-accounting race tests passed, including completion-cohort jitter, stale queued completions after fresh warmup, and true idle reset.
- The final slow-part scenario passed in 130.03 seconds: one observed 32.085-second sustained-demand gap retained its active window, the real 503 reduced the upload scope to four, the complete 256 MiB object matched SHA-256, and no owned multipart uploads or scheduler permits remained. An independent repeated run also passed. A duplicate EOF read was subsequently removed without changing behavior, as checked by both reviewers.
- The 512 MiB-per-upload experiment did not establish accepted recovery before its files completed. At probe time only 47 upload parts remained across all three files, while queued pre-probe operations, fresh warmup and three fresh cohorts could require about 40 completions. A file finishing invalidates the population comparison; its rollback reason was not recorded, so that cause remains an inference. The final workload uses three 1 GiB files and records policy state plus each file's progress to remove this ambiguity.

Raw logs, earlier failed experiments, and final fixture hashes are under the ignored `dist/adaptive-mixed/` directory. Do not interpret earlier failed experiments as successful validation. Both independent reviewers approved the final controller changes and strengthened test oracles.


## Final mixed-load outcome

**Passed:** 183.52 seconds for the complete real MinIO test, including setup and independent verification (178.964 seconds from transfer launch through final checks).

| Observation | Verified result |
|---|---|
| Concurrent workload | Three distinct 1 GiB uploads and three distinct 128 MiB downloads; request bodies paced at 4 MiB/s |
| Provider feedback | One SDK-observed 503 SlowDown reduced upload admission from eight to four; the healthy download allowance stayed eight |
| Recovery probe | Five-request probe began at 57.570 seconds; accepted at 89.861 seconds after fresh work was measured |
| Measured policy windows | 15.843 MiB/s baseline versus 19.830 MiB/s during the accepted probe |
| Download interruption | Existing destination survived; explicit Retry reused 8 MiB of verified data |
| Upload interruption | Sixteen dropped part attempts exercised SDK retry/failure handling; explicit Retry reused 528 MiB |
| Integrity | All six complete files matched their independently calculated, distinct SHA-256 digests |
| Shared budget and cleanup | Paced body streams and sampled scheduler admission stayed within eight; no owned unfinished multipart uploads, temporary download files, or held/waiting admission slots remained |

The throughput numbers are the controller's useful-completion windows on a controlled local link, not an end-to-end WAN benchmark. The test requires another object's real body stream to be active at both interruption and retry events. Separate upload/download proxy peaks occurred at different times and must not be added together.

The final diagnostics show that the accepted probe completed while all three upload files were still running, with roughly half their data remaining. This resolves the stable-population ambiguity from the earlier 512 MiB experiment. It does not imply that every short transfer must raise concurrency before it finishes.
