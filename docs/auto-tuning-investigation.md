# Automatic transfer tuning investigation

Investigation date: 2026-10-16. This is a proposed design, not an implemented feature. No additional network benchmarks were run for this investigation. Measurements reported by the concurrent upload experiment remain evidence for this Mac and connection, not a universal optimum.

## Recommendation

Build a resource-aware transfer scheduler with observable, bounded adaptation. Keep the validated eight-request behavior as the initial default; first collect the signals needed to make safe tuning decisions, then enable a conservative controller. Hardware supplies ceilings. Successful transfer measurements determine whether additional concurrency helps. A machine with 48 GiB of RAM should not automatically generate more S3 requests simply because it has spare memory.

Treat part sizing and request concurrency as separate decisions. Start new large uploads with the current 16 MiB parts. Adapt concurrency between parts without changing their boundaries. Larger parts are a later experiment: the current upload screening found no clear reason to prefer the larger candidate over 16 MiB, and larger parts increase work lost before acknowledgement.

## Current implementation and extension points

| Current code | Implication |
| --- | --- |
| `App.rangeSlots` and `transferSlots()` in `parallel_download.go` implement one capacity-eight channel. | There is an app-wide request cap for large checkpointed transfers, with no adjustable limit or explicit fairness policy. |
| `parallelDownload()` launches at most eight workers using 40 MiB ranges. `uploadMultipart()` launches the selected number of workers; current defaults are eight with 16 MiB parts. | Change admission to individual requests, rather than restarting transfers whenever the desired concurrency changes. Do not try to resize a channel in place. |
| `queueTransfer()` and `RetryTransfer()` launch a goroutine per transfer. | Large file count can still create many waiting workers and concurrent preparation work. Add bounded transfer preparation and request scheduling, not just a larger semaphore. |
| Small/nonjournaled uploads use `manager.NewUploader`; serial/fallback downloads use their original GET path. | These paths bypass the large-transfer semaphore. Any claimed whole-app budget must account for SDK-internal upload part requests and serial streams too. |
| `configuredS3Client()` clones a transport for each created client. | Separate transfers do not currently share a deliberately managed connection pool. A keyed, bounded client/transport cache is a candidate; preserve TLS, CA, redirect and credential-update behavior and measure the benefit. |
| Upload progress advances after storage acknowledgement and a successful checkpoint write. Download progress includes bytes received before verification and journal commit. | UI progress counters cannot serve as a comparable upload/download throughput instrument. Collect separate network, acknowledgement and durable-progress signals. |
| Download slots remain held through local sync and checkpoint commit; upload slots are released before the checkpoint write. | Slot occupancy currently measures different work. Instrument each phase before making controller decisions. |
| `saveCheckpointBlock()` persists a cloned checkpoint while holding `App.mu`, opening the store for every commit. | Growing concurrency may expose journal or lock contention. Measure commit and lock time; do not batch away durability guarantees for a superficial throughput gain. |

## Proposed ownership and interfaces

An app-owned `TransferScheduler` grants cancellable request leases. A lease is released exactly once after the response body has been consumed/closed or the attempt has ended. Shrinking a limit affects future admissions; it never kills an in-flight part. Preparation and disk work have separate budgets so increasing network workers cannot trigger unbounded staging, hashing or checkpoint writes.

Conceptual API:

```go
Acquire(ctx context.Context, request RequestClass) (Lease, error)
Observe(sample RequestSample)
Snapshot() TuningStatus
```

`RequestClass` identifies transfer, connection scope, upload/download direction, protocol operation, priority and expected bytes. `RequestSample` reports queue delay, active duration, verified bytes, attempt outcome and local phase costs. The scheduler does not own credentials, file content, recovery identity, or retry policy. It must not hold `App.mu` while waiting or performing I/O.

Use three nested limits:

1. A hard process ceiling for transfer requests and a bounded active-transfer/preparation count.
2. A per-connection/provider ceiling, with upload/download targets and shared host-resource accounting.
3. A per-transfer ceiling, constrained by remaining parts and fair scheduling.

Upload and download may have different useful targets. They still compete for disk, CPU and connections, so adapt one direction at a time during an experiment. Do not infer a network interface's real capacity from its nominal link speed. Reserve a small, separately bounded path for browsing, cancellation/abort, completion and recovery metadata, so bulk transfers cannot make the UI or cleanup wait indefinitely. Include this reserve explicitly when displaying or testing total request limits.

Start with connection identity based on existing `multipartClientScope()` plus Harbor profile identity and direction. Provider throttling may need an additional bucket/service partition later; isolate a failing connection immediately rather than slowing unrelated providers. Recompute/invalidate learning after endpoint, region, path-style, trust or credential configuration changes, without persisting any credential material.

## Signals and safety limits

Collect cheap monotonic counters and bounded aggregates, not one persistent record per read:

- Useful acknowledged/verified bytes per wall-clock interval, separately from bytes read or sent during retries. Report end-to-end time separately from active network time.
- Request concurrency, waiting demand and queue delay; per-part duration and time to response headers; SDK attempt count, retry delay and classified throttling/service/network failures. An SDK operation that eventually succeeds can still have retried: observe attempts, not only terminal errors.
- Staging, whole-source hashing, local range writes, sync, checkpoint commit and final publication time; their queueing and lock contention. Distinguish a busy disk from a provider slowing down.
- Go heap, process resident memory where supported, CPU consumption and available system memory/pressure. Monitor cache disk free space and outstanding staged bytes. Include a platform adapter for Darwin/Linux/Windows with a conservative fallback when pressure information is unavailable.

Use available memory and process limits in addition to total physical RAM. Existing seekable multipart sources stream from disk: do not assume every worker allocates a whole part, or compute workers as RAM divided by part size. Measure buffers and resident-memory growth; retain explicit bounded streaming buffers. Avoid changing global Go memory settings as a side effect of tuning. CPU count is an upper safety input, not an instruction to open one stream per thread. Disk free-space checks must include concurrent staging and download reservations; a sparse partial file is not proof that enough physical capacity remains.

Initial release: retain the validated maximum of eight bulk requests. Resource pressure may lower that limit. Testing above eight requires a separately configured hard cap, transport limits and new measurements; the current code rejects upload concurrency greater than eight. There is no evidence yet for a universal cap of sixteen or thirty-two.

## Conservative control loop

First run in observation/shadow mode. Record what the controller would choose without changing concurrency. This establishes signal quality and exposes oscillation before affecting recovery or throughput.

For live tuning, use a bounded hill-climb with fast safety reductions:

1. Begin at the validated default clamped to current resource ceilings, or a recent conservative learned value. An unknown machine can start lower if resources are uncertain. Retain a predictable fixed-mode fallback.
2. Only probe upward when there is queued work, sufficient remaining data, healthy resources, stable demand and several successful completed parts. Change one connection/direction at a time. Use dwell windows long enough for multiple parts to complete; a one-second UI poll is not a control interval.
3. Add one request, compare aggregate useful throughput against the prior window at comparable demand, and retain the change only for a meaningful sustained gain. If variance is high or competing transfers changed, hold rather than declare a winner. Exact gain thresholds and dwell times must be calibrated from trace replay, not presented as established values.
4. Reduce the affected provider's admissions promptly on classified throttling; apply a cooldown and let the SDK honor its retry/backoff semantics. Do not add a second blind operation retry layer. Unconfirmed in-flight parts can finish; shrinking only blocks replacement work. For persistent transport failures, pause probing and preserve normal checkpoint/error behavior.
5. Reduce immediately on severe memory/disk pressure. Local sync or hashing saturation should suppress increases and use the local-work budget. Re-probe after a stable recovery period, with hysteresis.
6. Stop learning when there are too few remaining parts, user cancellation, source identity failure, checkpoint corruption or an invalid destination. Those failures are not congestion samples. Sleep/wake, network changes and long idle periods invalidate the current window.

Do not require a standalone speed test or transfer extra customer data solely to tune. Learn during useful work. A single short upload should usually use defaults; there may be too little evidence to adapt meaningfully.

## Fairness and responsiveness

Use round-robin admission across ready transfers initially, with at most one newly granted request per transfer per pass and a capped per-transfer share. A lone transfer can consume the available budget. A newly queued transfer must gain access when current requests finish, without waiting for every part of a large transfer. Prioritize short transfers carefully with aging so neither large nor small files starve.

Because upload/download chunks differ in size, equal request counts are not exact bandwidth fairness. Begin with observable bounded waiting; byte-weighted deficit scheduling is a later improvement if mixed-workload tests show a problem. Include small SDK uploads and serial downloads in admission. A long serial request can occupy a lease until completion; do not promise a strict wall-clock wait bound unless its duration can be bounded or its work split safely.

## Recovery invariants and compatibility

- Never change an existing multipart upload's recorded `ChunkSize`, part numbers, upload ID or scope. New uploads choose settings once before creating the first part; retries keep their recorded part size. Concurrency is runtime policy and need not become part of recovery identity.
- Keep current supported 5/16/32 MiB upload sizes until a deliberate compatibility change. The checkpoint validator explicitly enumerates these sizes. Old binaries cannot be assumed to understand a newly introduced size. Download checkpoint validation currently requires exactly 40 MiB ranges, so download chunk-size experimentation also requires a versioned migration plan.
- Reuse only acknowledged parts validated against S3 and a rehashed staged source. Keep download ETag/version checks, range validation, local range hashes and sync-before-checkpoint. Adaptive admission never bypasses them.
- Preserve complete-then-publish behavior and atomic completed-state journaling. Lost completion replies, explicit cancellation and failed abort cleanup retain their current semantics.
- Unknown or corrupt learned tuning data falls back to defaults; it must never prevent opening the transfer journal or recovering valid transfers.

## Persisted learning and UI

Store a small, versioned, expiring tuning summary separately from transfer checkpoints: connection/direction key, recommended concurrency within known caps, measured useful throughput, variability, sample count, confidence and last observation. Do not store object keys, paths, credentials or unbounded traces. Invalidate on configuration/network change where detectable; otherwise decay confidence after idle time and verify with new measurements. A saved result is a starting hint, not permission to ignore live resource pressure.

Keep normal UI simple: **Automatic** (default after rollout), **Limit bandwidth**, and an advanced **Fixed concurrency** override with a hard resource ceiling. Distinguish concurrency from bandwidth: a real bandwidth limit requires a byte-rate limiter, not merely fewer streams. If that limiter is not implemented yet, defer that control rather than mislabel concurrency. Offer a compact explanation such as “Optimizing transfer speed” or “Reduced activity because storage is busy,” plus optional diagnostics showing active requests and useful throughput. Reset learned settings should not delete transfer history or checkpoints. No per-file tuning dialog is needed.

## Staged implementation and acceptance

| Stage | Scope | Exit evidence |
| --- | --- | --- |
| 1: Instrument | Phase timers, SDK attempt outcomes, resource sampling and an opt-in bounded diagnostics export. Keep fixed defaults. | Measurements do not log secrets or object identifiers; negligible overhead in current 1/5 GiB controls. |
| 2: Schedule | Cancellable fair leases, global/per-connection limits, bounded preparation, small/serial-path accounting, reserved control capacity. Keep fixed limits. | Deterministic limit/fairness/cancellation tests; existing race and recovery suites pass. |
| 3: Shadow | Pure controller with injectable clock, replayed traces and proposed settings in diagnostics. | No oscillation or false congestion decisions on bursty checkpoints, SDK retries or demand changes. |
| 4: Adapt within validated cap | Opt-in live adjustment, resource downshifts, provider cooldown, conservative expiring learning and fixed override. | Repeatable gains or unchanged performance on fast/slow/shaped networks; no resilience regressions. |
| 5: Extend evidence | Explore more than eight requests, new part sizes, transport reuse, byte fairness and optional bandwidth limits. | Rotated real-provider trials justify each expansion; compatibility and recovery tests cover any new part size. |

Deterministic tests should simulate throughput plateaus, noisy intervals, changing demand, throttling with hidden SDK retries, CPU/disk/memory pressure, clock jumps, sleep/wake, invalid learned data and connection reconfiguration. Assert hard limits, prompt cancellation of queued admissions, no permit leaks, no starvation under bounded request durations and no duplicate work from stale attempts. Unit tests should exercise a pure controller with fake time rather than real sleeps.

Reuse the current TLS range fixtures, MinIO fault proxy and process workers for mixed upload/download workloads, ignored ranges, corrupt or changed source/parts, process termination, lost completion replies and abort failure. Add a recovery run while limits shrink/grow and verify that the same journaled part boundaries survive restart. Repeated small files must not escape the aggregate budget.

Benchmark one large file and mixed queues: 100 MiB, 1 GiB and 5 GiB, on at least one constrained machine and this Mac; fast local MinIO, shaped latency/bandwidth/loss, and opt-in actual AWS. Rotate fixed/adaptive trial order, use at least three repeats for comparisons, verify SHA-256 outside upload timing, and report staging, network, retry bytes, median/variability, CPU, heap/RSS, tail completion time and fairness. Re-run after network changes and interruption. Resource detection alone cannot establish a speed improvement.

## Recommended follow-up scope

The recommended implementation follow-up is stages 1–3 as internal infrastructure, preserving validated transfer settings and recovery behavior. Make live adaptation opt-in initially and keep its maximum at eight until tests support more. Use current 16 MiB upload parts and 40 MiB download ranges. Defer automatic part-size changes, plugin-wide policy, byte-accurate fairness and predictive network models. The investigation does not implement these stages. A follow-up implementation can use these defaults without asking the user to select low-level tuning values.

The largest engineering risk is confusing useful throughput with retries, local durability stalls or competing demand. The largest product risk is a controller that constantly probes and slows ordinary transfers. Instrumentation, a fair scheduler, conservative ceilings and shadow evaluation address these before increasing parallelism.
