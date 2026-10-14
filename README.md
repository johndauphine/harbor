# Harbor S3 Browser

Work in progress: a Go executable serving an embedded browser UI on loopback only.

Current source includes saved connection profiles with OS credential vault storage, S3 bucket and prefix browsing, pagination, and a drafted transfer backend. Browser uploads now stage files locally and support background progress, cancellation, and retry. Downloads also run in the background with progress, cancellation, and retry. Download opens Save As first and saves automatically to the selected destination. This is an early test build; do not use production or federal data.

## Development

Requires Go 1.26 or newer. Run `go test ./...` then `go run .`. Open the session URL printed by the program and keep it private. Stop with Ctrl+C. Source has no dependency on Node, Wails, WebView2, or a separate server.

Build: `go build -o harbor .`. Cross-build Windows with `GOOS=windows GOARCH=arm64 go build -o harbor.exe .` (or `amd64`).

## Verification

`go test -race ./...` passes actual local TLS upload/download, cancellation and retry tests, SQLite metadata round-trip tests, and boundary tests for Host, Origin, session authorization, HTTPS-only endpoints, secret separation, and prefix pagination against a local TLS fixture. macOS ARM64 and Windows ARM64/AMD64 executables compile. Real AWS/on-premises integration and native Windows x64 behavior are not yet verified. The preceding x64 build served its UI successfully in Windows ARM emulation; the updated build requires a fresh smoke test.

## Current limits

- Upload staging is limited to 5 GiB and uses disk space; failed/cancelled upload files remain available for retry in the user cache until normal shutdown. Successful uploads remove their staging file.
- Cached downloads are removed on normal shutdown, but abrupt termination can leave cache files.
- Connection editing, credential removal, FIPS build validation, signing, and audit logging are pending.
- Do not infer FedRAMP compliance from these application-level safeguards.

## Security scope

The server binds only to 127.0.0.1 and requires a random session token for API requests, checks Host and Origin, and serves bundled assets with a restrictive content security policy. Storage endpoints require HTTPS with certificate validation. Credentials use macOS Keychain or Windows Credential Manager through go-keyring; SQLite stores connection metadata only. Credentials are not stored in SQLite.

Assumptions: patched OS and browser, disk encryption, protected user account, and approved storage endpoints. FedRAMP High compliance, validated cryptography configuration, real storage integrations, and platform packaging are not verified.

Independent implementation inspired by familiar S3 file-manager layouts. No code or assets copied from CS Browser, brows3, BucketDock, or Cyberduck.

## Configuration location

Default: `.harbor/profiles.db` under the current user home directory on both macOS and Windows. Override with `--db /chosen/path/profiles.db`. No YAML bootstrap file is required. Credentials remain in the OS vault; the internal vault service name is retained for compatibility.

## Local MinIO compatibility test

Verified against official MinIO release `RELEASE.2025-10-15T17-29-55Z`, built with `CGO_ENABLED=0` on Apple Silicon. A local HTTPS endpoint with a private test CA passed bucket listing, folder-marker creation and overwrite protection, multipart upload (6.75 MB), folder browsing, and byte-identical download. The SDK emitted a multipart-checksum validation warning; the test independently compared all downloaded bytes.

An opt-in test is available: `HARBOR_MINIO_TEST=1 go test -race -run TestLocalMinIOIntegration -v .`. It requires a saved `MinIO Local` connection pointing to `https://127.0.0.1:59000`, path-style addressing, and its CA certificate. It creates a uniquely named test bucket and retains sample objects for manual browsing. It does not target AWS profiles.

## Single-file deletion

Select a file and use Delete file. Confirmation shows its bucket and full key. Harbor checks the current ETag before deletion and sends an If-Match condition. No recursive folder or bucket deletion is implemented. Versioning and retention rules determine whether a delete marker is created or a file is permanently removed; version management is not yet exposed.

The local MinIO release ignores If-Match on DeleteObject, so its preflight catches stale selections but cannot prevent a concurrent replacement between the check and delete. The live local test verifies stale-selection rejection and deletion using only a disposable generated object. No AWS objects were deleted during development.

## Transfer refresh

The browser automatically refreshes the relevant current listing when an upload or download completes, preserving the selected file if it remains visible. Terminal status changes are reported once, and failed transfers include their error in the status field. Completed downloads show their saved location in the transfer row. Transfers in other buckets do not change the user's current location. Frontend regression check: `node --test tests/ui-transfers.test.cjs` (Node is only required for this developer test).

## Save As

Download opens a folder-and-filename dialog inside Harbor before queuing the selected file. It starts in Downloads, supports Home/Up navigation and an explicit folder path, and prompts before replacing an existing file. Canceling the dialog does not queue a download. The transfer streams to a temporary file in the destination folder and publishes it only on success; progress, cancellation, retry, and the saved location stay in Transfers. No second Save file action is needed. Filesystems that do not support hard links may reject creation of new saved files; existing-file replacement uses rename.

## Upload picker

Upload uses a matching in-app dialog with local folder navigation, file sizes, and a highlighted file selection. The destination bucket and folder are shown before confirmation. Harbor stages a private copy of the selected regular file (up to 5 GiB) before background upload; cancellation, retry, and cleanup never remove the original local file. Local file listing and upload endpoints use the same loopback, session, Host, and Origin protections as the other APIs.
