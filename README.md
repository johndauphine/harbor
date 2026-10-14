# Harbor S3 Browser

Work in progress: a Go executable serving an embedded browser UI on loopback only.

Current source includes saved connection profiles with OS credential vault storage, S3 bucket and prefix browsing, pagination, and background transfers. Browser uploads now stage files locally and support background progress, cancellation, and retry. Downloads also run in the background with progress, cancellation, and retry. Download opens Save As first and saves automatically to the selected destination. This is an early test build; do not use production or federal data.

## Development

Requires Go 1.26 or newer. Run `go test ./...` then `go run .`. Open the session URL printed by the program and keep it private. Stop with Ctrl+C. Source has no dependency on Node, Wails, WebView2, or a separate server.

Build: `go build -o harbor .`. Cross-build Windows with `GOOS=windows GOARCH=arm64 go build -o harbor.exe .` (or `amd64`).

## Verification

`go test -race ./...` and `go vet ./...` pass. Regression coverage includes local TLS transfers, cancel/retry attempt isolation, shutdown draining and timeout behavior, credential save failures and interrupted-save recovery, private credential references, browser request boundaries, and endpoint/redirect protections. `node --test tests/*.test.cjs` passes 39 checks, including native Chromium form activation and connection cleanup warnings.

The preceding build passed live Edge checks in Windows 11 ARM under Parallels for both ARM64 and emulated AMD64 executables, with byte-identical uploads/downloads using isolated HTTPS fixtures. The latest Go regression suite also passed inside that VM for ARM64 and emulated AMD64, including credential consistency, cancellation/retry, shutdown, and process locking. Opt-in tests requiring the separate live MinIO server were skipped; the suite used synthetic local fixtures. These checks do not establish native x64 coverage, and the latest backend revision has not repeated the full interactive browser checklist.

## Current limits

- Upload staging is limited to 5 GiB and uses disk space; failed/cancelled upload files remain available for retry in the user cache until normal shutdown. Successful uploads remove their staging file.
- Cached downloads are removed on normal shutdown, but abrupt termination can leave cache files.
- The executable enforces one process per profile database using an OS-backed lock. Close older Harbor binaries before upgrading because they do not honor this lock. Use a trusted local filesystem; network-share locking is not verified, and hard-linked configuration databases are rejected.
- Autocomplete still considers a limited listing page; a shared prefix may not represent every matching object.
- FIPS build/configuration validation, signed releases, security audit logging, and formal deployment assessment remain pending.
- Do not infer FedRAMP compliance from these application-level safeguards.

## Security scope

The server binds only to 127.0.0.1 and requires a random session token for API requests, checks Host and Origin, and serves bundled assets with a restrictive content security policy. Storage endpoints require HTTPS with certificate validation, and redirects are rejected. Custom endpoints come only from the saved Harbor connection; ambient AWS endpoint overrides are ignored. Proxy and system trust configuration still require operator review. Credentials use macOS Keychain or Windows Credential Manager through go-keyring; SQLite stores connection metadata only. Credentials are not stored in SQLite.

Assumptions: patched OS and browser, disk encryption, protected user account, and approved storage endpoints. FedRAMP High compliance, validated cryptography configuration, real storage integrations, and platform packaging are not verified.

Independent implementation inspired by familiar S3 file-manager layouts. No code or assets copied from CS Browser, brows3, BucketDock, or Cyberduck.

## Configuration location

Default: `.harbor/profiles.db` under the current user home directory on both macOS and Windows. Override with `--db /chosen/path/profiles.db`. No YAML bootstrap file is required. Credentials remain in the OS vault; the internal vault service name is retained for compatibility.

## Local MinIO compatibility test

Verified against official MinIO release `RELEASE.2025-10-15T17-29-55Z`, built with `CGO_ENABLED=0` on Apple Silicon. A local HTTPS endpoint with a private test CA passed bucket listing, folder-marker creation and overwrite protection, multipart upload (6.75 MB), folder browsing, and byte-identical download. The SDK emitted a multipart-checksum validation warning; the test independently compared all downloaded bytes.

An opt-in test is available: `HARBOR_MINIO_TEST=1 go test -race -run TestLocalMinIOIntegration -v .`. It requires a saved `MinIO Local` connection pointing to `https://127.0.0.1:59000`, path-style addressing, and its CA certificate. It creates a uniquely named test bucket and retains sample objects for manual browsing. It does not target AWS profiles.

## Single-file deletion

Select a file and use Delete file. Confirmation shows its bucket and full key. Harbor checks the current ETag before deletion and sends an If-Match condition. No recursive folder deletion or automatic bucket emptying is implemented. Empty-bucket deletion requires typing the exact bucket name; S3 enforces permissions, versions, and retention restrictions. Versioning and retention rules determine whether a delete marker is created or a file is permanently removed; version management is not yet exposed.

The local MinIO release ignores If-Match on DeleteObject, so its preflight catches stale selections but cannot prevent a concurrent replacement between the check and delete. The live local test verifies stale-selection rejection and deletion using only a disposable generated object. No AWS objects were deleted during development.

## Transfer refresh

The browser automatically refreshes the relevant current listing when an upload or download completes, preserving the selected file if it remains visible. Terminal status changes are reported once, and failed transfers include their error in the status field. Completed downloads show their saved location in the transfer row. Transfers in other buckets do not change the user's current location. Frontend regression check: `node --test tests/ui-transfers.test.cjs` (Node is only required for this developer test).

## Save As

Download opens a folder-and-filename dialog inside Harbor before queuing the selected file. It starts in Downloads, supports Home/Up navigation and an explicit folder path, and prompts before replacing an existing file. Canceling the dialog does not queue a download. The transfer streams to a temporary file in the destination folder and publishes it only on success; progress, cancellation, retry, and the saved location stay in Transfers. No second Save file action is needed. Filesystems that do not support hard links may reject creation of new saved files; existing-file replacement uses rename.

## Upload picker

Upload uses a matching in-app dialog with local folder navigation, file sizes, and a highlighted file selection. The destination bucket and folder are shown before confirmation. Harbor stages a private copy of the selected regular file (up to 5 GiB) before background upload; cancellation, retry, and cleanup never remove the original local file. Local file listing and upload endpoints use the same loopback, session, Host, and Origin protections as the other APIs.

## Reliability safeguards

Credential replacements are staged under a new private vault reference. SQLite commits the connection metadata and reference together, so a failed save does not overwrite the previous credentials. Existing profiles migrate when saved. A durable queue retries removal of unused vault entries after interrupted saves or locked-vault failures. If the connection change committed but cleanup failed, the UI reports that distinction; unlock the vault and restart Harbor to retry cleanup. SQLite contains references, never the credential values. Do not downgrade to an older executable after this migration without an appropriate backup/recovery plan.

Startup acquires an OS-backed lock before SQLite initialization or credential access. A second executable using the same canonical configuration path is refused. Symlink aliases resolve to the same lock; Windows also normalizes junctions and short names. A sidecar lock file may remain after exit, but the OS lock itself is released automatically, including after a crash. Do not delete or replace the database or lock file while Harbor is running.

Cancel/retry uses an identity per attempt. An old queued worker cannot claim a newer retry, concurrent retries cannot launch duplicate workers, and active cancellation must unwind before retry is allowed.

Shutdown closes admission, cancels active transfers and tracked API/file operations, waits for them to stop, and only then removes cached transfer files. Normal command-line shutdown handles interruption and termination signals. The server allows five seconds for request draining, then up to twenty-five seconds for remaining work; an incomplete drain reports an error and preserves potentially active files instead of racing their removal. An abrupt exit or timed-out shutdown may therefore leave local data requiring controlled cleanup.

## Regulated-data release gate

Development and regression tests use synthetic data. The target is strict application safeguards, not a claim that a home workstation or this executable is authorized for federal information. Do not introduce regulated federal data until the responsible organization approves the client, workstation, storage destination, and data flow within its system boundary.

Before such use, the unresolved engineering and deployment work includes validated cryptographic build/runtime settings; dependency and vulnerability review; release integrity/signing; audit requirements; cache, download, backup, and crash-retention controls; endpoint/proxy trust; least-privilege credentials; and verification of the target filesystem and workstation configuration. The current tests do not establish these controls. The loopback browser session also relies on the security of the signed-in OS account and browser.

FedRAMP distinguishes [cloud-service scope](https://www.fedramp.gov/2026/scope/) from [agency responsibility for using the service within its system](https://www.fedramp.gov/2026/agencies/use/). This local client requires assessment in that deployment context; fixing bugs does not confer authorization.
