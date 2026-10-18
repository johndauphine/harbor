# Windows folder upload investigation — 2026-10-17

The reported case used Add folder on Downloads with eleven files totaling about
853 MiB. Its first file was a 731 MiB installer. No transfer was queued in the
original AWS window. Closing and reopening that draft allowed a generated 1 KiB
file to upload to the user's test AWS bucket. This did not by itself establish
the cause of the original stale dialog.

A real native folder selection containing a generated 731 MiB file reproduced
a concrete transport failure in the old packaged x64 app on Windows ARM64:
the dialog entered Preparing, then WebView2 crashed and required an app restart.
The small-file and synthetic JavaScript File tests did not catch this failure.
The failing real-file path sent the entire File as a WebView2 request body.

The fix uses native desktop file/folder dialogs and returns only metadata to
JavaScript. The existing QueueLocalUpload implementation copies selected bytes
to staging with bounded Go I/O, retaining existing transfer journal, retry,
cancellation, and shutdown behavior. Browser uploads keep their existing path.
Desktop dragged files over 64 MiB receive a visible instruction to use the native
pickers. That threshold is a conservative guard, not a measured WebView2 limit.
Native folder scans reject links, enumeration or metadata failures, and oversized
selections as a whole. Read access is checked during staging, where a file can
still fail individually after earlier files queue. Cancelled/stale picker results
cannot populate a new draft.

Validation:

- Rebuilt packaged x64 app: Add folder selected the same generated 731 MiB file
  and a 1 KiB companion. Both completed through local MinIO, with the window
  remaining usable. No personal Downloads content was uploaded in this test.
- Opt-in real WebView2 regression: native picker metadata, eleven files with
  nested folder keys, staging, HTTPS multipart upload, and download. Set
  HARBOR_DESKTOP_TEST=1 and HARBOR_DESKTOP_LARGE_TEST=1 to include the 731 MiB
  disk-backed source. Every received multipart part is hashed and compared
  against its source range, and the total size is checked.
- Unit coverage includes picker host/origin/method/admission checks, relative
  keys, cancellation, missing files, selection limits, and link rejection where
  the OS permits creating a test link. Frontend coverage includes native paths,
  failed preparation and retry, stale picker completion, and the drag guard.
- All 77 frontend tests passed, including native Edge form activation.
- go vet with desktop,production tags and git diff --check passed.
- The full Go suite with desktop,production tags passed outside the sandbox
  (18.673 seconds), including existing transfer cancellation, retry, shutdown,
  and journal recovery coverage.
- The 731 MiB WebView2 regression passed on windows/amd64 under ARM64 emulation
  and natively on windows/arm64. The installed x64 build's SHA-256 is
  `4a63fd7c03b9334ebd5898df6a50664039264db7fd53a52ebff1d32cdaa88817`.

The initial sandboxed full Go run could not connect to its loopback fixtures;
its failures are not treated as product regressions or passing validation.
Native x64 hardware remains untested. The original stale draft had no diagnostic
trace, so the later reproduced crash cannot prove every detail of that first
incident. The new native picker path removes the reproduced large-body failure.
No merge is authorized or performed; independent merge reviews remain required.

## Subsequent full Downloads test against AWS

The user explicitly authorized uploading the complete Downloads folder to
`s3://johndauphine-delete-me-today/`. The native Add folder selection contained
19 files totaling about 1.6 GiB, including the installer and an identically sized
731 MiB .crdownload file. All 19 staged and queued, and the dialog remained usable.
The connection was a phone hotspot.

Seventeen transfers completed. Both 731 MiB transfers failed during UploadPart
with HTTP 403 RequestTimeTooSkewed after some parts were acknowledged. There were
no active transfers left at the final journal check. No automatic retries of the
failed large files were initiated. This is not a passing full-folder AWS test.

The local clock matched the UTC clock tool. Code inspection found that
tuningRoundTripper acquires scheduler capacity inside RoundTrip, after SDK
signing. Long waits on the slow connection can therefore age signed requests.
This identified a separate admission/signing ordering problem, addressed below.

## Signing admission fix

Capacity admission now runs in SDK Finalize middleware immediately before
Signing, inside the SDK Retry loop. Each attempt waits before its signature is
generated. The HTTP transport consumes that attempt's existing lease and retains
the existing response-body/cancellation lifetime. Signing failures before HTTP
release capacity through a deferred cleanup path. Direct transport-only callers
retain the fallback admission path.

Regression coverage holds all eight slots and verifies the queued operation has
not reached signing. It then releases capacity and requires a fresh active lease
at signing on both an initial request and a simulated 503 retry. The regression
was also run with the new admission hook temporarily disabled: it failed with
"signing without an exclusive per-attempt lease". The fixed source was restored.
Additional coverage checks cancellation while waiting and failure before HTTP.
The complete desktop-tagged Go suite passed (17.788 seconds), and go vet passed.

The installed x64 build has SHA-256
`c4d39adbeae83993510bbbe7e94ba6dc0b10406f7d5761d9fe481ac3e5486626`.
The user authorized retries of both failed AWS files; both were started from their
saved journals with the new build. S3 confirmed 192 MiB of the installer, including
128 MiB of newly uploaded data beyond its reused 64 MiB. The other file retained
32 MiB of confirmed parts. Neither retry had a new error at the last check.
Both remained running. The user initially asked not to wait for completion, then
later asked to stop the test uploads. At cancellation, S3 had confirmed 320 MiB of
the installer and 160 MiB of the other file, including 384 MiB of new data across
the retries. No new signing error was reported. Full cloud completion has not
been claimed. No branch merge or release was performed.

## Independent review preparation

Two independent agents reviewed the complete prospective diff against main,
including the initial desktop integration, tests, configuration, and documents.
Neither found a blocking code defect. They identified two documentation issues:
the README still described native pickers/provider validation as future work,
and selection documentation overstated unreadable-file rejection. Both were
corrected: current desktop behavior and evidence are linked, historical validation
is labelled as such, and scan/metadata failures are distinguished from per-file
staging read failures.

Fresh review validation: the full desktop-tagged Go suite passed in 19.694
seconds; vet, module verification, dependency notices, all 77 frontend tests
(including native Edge), and diff whitespace checks passed. Three Python packaging
tests passed, but the version-formatting test encountered Windows access denial
while deleting its temporary executable. That local run is not a passing
packaging check. Re-running all four Python packaging tests using the native
Windows ARM64 toolchain passed (3.925 seconds). The x64 emulation cleanup failure
remains recorded rather than treated as passing. Race checks and hosted CI remain
required before merge. Native x64 hardware and full hotspot upload completion
remain unverified.
