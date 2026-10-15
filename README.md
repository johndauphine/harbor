# Harbor S3 Browser

Harbor is a standalone Go executable serving an embedded S3 browser on loopback only. The current target is reliable general-purpose use on macOS Apple Silicon and Windows ARM64/x64, and Linux x64.

Current source includes saved connection profiles with OS credential vault storage, S3 bucket and prefix browsing, pagination, and background transfers. Browser uploads now stage files locally and support background progress, cancellation, and retry. Downloads also run in the background with progress, cancellation, and retry. Download opens Save As first and saves automatically to the selected destination. This is a pre-release build. Keep backups and verify behavior with disposable files before using important data. It has not been assessed for regulated federal use.

## Linux x64 support

Linux builds target **x86-64 / AMD64**. Linux ARM64 is not currently packaged. Linux support was added after the initial v0.1.0 beta; that release does not contain a Linux asset. The [GitHub Actions workflow](https://github.com/johndauphine/harbor/actions/workflows/ci.yml) produces Linux development archives when its checks pass.

### Runtime requirements

- A Linux x64 system and a current local web browser. Distribution-specific desktop compatibility has not yet been fully validated.
- A user D-Bus session and a running Secret Service provider with an unlocked **login** collection, such as GNOME Keyring, for saving and retrieving credentials. Run Harbor as your desktop user, not with `sudo`.
- System CA certificates for verified HTTPS connections to S3-compatible storage.
- `xdg-open` (commonly supplied by `xdg-utils`) for automatic browser opening. It is optional: use `--no-browser` and open the printed URL manually.
- Writable home/cache directories and sufficient free disk space for transfer staging; uploads are currently limited to 5 GiB.

The packaged executable does not require Go, Node.js, Wails, or WebView2. A headless system without a user keyring cannot save credentials. `--no-browser` only disables browser launching; it does not remove the keyring requirement or enable remote access. Harbor provides no plaintext credential fallback.

### Install and run

Download the Linux `.tar.gz` archive and its matching `SHA256SUMS`. In the download directory, verify the archive with `sha256sum --ignore-missing -c SHA256SUMS` and confirm that the Linux archive reports `OK`. Extract it into a new folder using your archive manager; the archive preserves executable permissions. Open a terminal in that folder and run:

```sh
./harbor
```

Harbor opens the default browser. If that fails, open the printed session URL on the same computer, or start with `./harbor --no-browser`. Keep the session URL private. Stop Harbor with Ctrl+C; closing the browser tab does not stop it.

Settings default to `~/.harbor/profiles.db`; `--db /chosen/path/profiles.db` selects another location. Credentials remain in the Secret Service keyring. See [upgrade instructions](UPGRADE.md) before moving or restoring settings.

### Validation and troubleshooting

A Linux x64 development archive passed checksum verification, extraction, version checks, isolated startup, embedded-page loading, and shutdown in an emulated Linux x64 container. CI is configured to run native Linux regression, browser-form, large-transfer, vulnerability, and archive-startup checks. Full interactive Linux desktop behavior, automatic browser opening, and real Secret Service credential integration have not yet been verified.

If credential saving fails, check that Harbor runs in your logged-in desktop session and that the Secret Service login keyring is available and unlocked. If automatic browser opening fails, use the printed URL on the same machine. Archives are unsigned development builds until explicitly published as a release.

## Development

Requires Go 1.26.9 or newer; use the patched version recorded in `go.mod`. Run `go test ./...` then `go run .`. Harbor opens your default browser automatically. The session URL is also printed as a fallback; keep it private. Use `--no-browser` for unattended startup or to open the URL yourself. Stop with Ctrl+C. Source has no dependency on Node, Wails, WebView2, or a separate server.

Build: `go build -o harbor .`. Cross-build Windows with `GOOS=windows GOARCH=arm64 go build -o harbor.exe .` (or `amd64`).

## Verification

`go test -race ./...` and `go vet ./...` pass. Regression coverage includes local TLS transfers, cancel/retry attempt isolation, shutdown draining and timeout behavior, credential save failures and interrupted-save recovery, private credential references, browser request boundaries, and endpoint/redirect protections. `node --test tests/*.test.cjs` passes 42 checks, including native Chromium form activation and connection cleanup warnings.

The preceding build passed live Edge checks in Windows 11 ARM under Parallels for both ARM64 and emulated AMD64 executables, with byte-identical uploads/downloads using isolated HTTPS fixtures. The latest Go regression suite also passed inside that VM for ARM64 and emulated AMD64, including credential consistency, cancellation/retry, shutdown, and process locking. Opt-in tests requiring the separate live MinIO server were skipped; the suite used synthetic local fixtures. These checks do not establish native x64 coverage, and the latest backend revision has not repeated the full interactive browser checklist.

## Current limits

- Upload staging is limited to 5 GiB and uses disk space. Transfer history, retryable staged uploads, and chosen download destinations survive shutdown and restart. Staging is kept in a private `<database>.transfers` directory beside the settings database; successful uploads remove their staged data.
- Interrupted transfers return as failed (or cancelled after orderly shutdown), with Retry available. Retry starts a new attempt from the beginning; it does not resume byte ranges. Startup removes recorded partial downloads and unreferenced files in this database’s private cache. It does not sweep shared caches from older builds.
- The executable enforces one process per profile database using an OS-backed lock. Close older Harbor binaries before upgrading because they do not honor this lock. Use a trusted local filesystem; network-share locking is not verified, and hard-linked configuration databases are rejected.
- Autocomplete loads matches in pages. Tab only infers a shared or unique completion once the matching pages are complete; explicitly highlighting a result still works immediately.
- Public artifacts are unsigned until publisher signing is configured. Mac notarization and Windows publisher signing are not yet complete. No automatic self-update is implemented.
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

Startup acquires an OS-backed lock before SQLite initialization or credential access. A second executable using the same canonical configuration path is refused. Symlink aliases resolve to the same lock; Windows also normalizes junctions and short names. Current databases also bind to their canonical location before credential access. Copies or moves to a different location are rejected; restore backups to the original location or create a new empty configuration and re-enter credentials. Legacy backups without a binding cannot be detected before their first current startup. A sidecar lock file may remain after exit, but the OS lock itself is released automatically, including after a crash. Do not delete or replace the database or lock file while Harbor is running.

Cancel/retry uses an identity per attempt. An old queued worker cannot claim a newer retry, concurrent retries cannot launch duplicate workers, and active cancellation must unwind before retry is allowed.

Shutdown closes admission, cancels active transfers and tracked API/file operations, waits for them to stop, and commits their final state. Retryable staged uploads are retained. Normal command-line shutdown handles interruption and termination signals. The server allows five seconds for request draining, then up to twenty-five seconds for remaining work; an incomplete drain reports an error and preserves potentially active files instead of racing their removal. After an abrupt exit, startup restores transfer history and removes recorded partial download files. Staged upload bodies are retained until a successful retry or deletion of the connection.

## General-use release status

The current release work targets ordinary S3 browsing and file transfers, not FedRAMP certification. Read [UPGRADE.md](UPGRADE.md) before replacing an older build and [RELEASE.md](RELEASE.md) for build, verification, and signing instructions. `harbor --version` prints build and toolchain information without opening settings or credentials.

Go 1.26.9 replaces the earlier vulnerable toolchain. A source vulnerability scan with govulncheck 1.3.0 reported no known advisories for the three shipped targets at the time of the audit; this is not a guarantee against unknown vulnerabilities. CI repeats dependency integrity checks, tests, vulnerability scans, and builds with checksums. CI artifacts are development builds, not automatically published releases.

Synthetic local HTTPS tests cover approximately 13 MB multipart uploads and 12 MB downloads with dropped connections, truncated responses, cancellation, manual retry, and automatic upload-part retry. Successful results are compared byte for byte. Interrupted downloads preserve an existing destination. Multipart initiation records the exact upload ID before sending parts. Cancellation attempts cleanup with a separate five-second deadline; startup retries recorded cleanup within a bounded budget, and Retry must finish that cleanup before starting a fresh upload. Offline servers or changed connection settings leave a visible pending-cleanup warning and retain the ID. Cleanup never lists and aborts unrelated uploads by key or prefix. Bucket lifecycle rules remain useful for the narrow crash window between remote initiation and recording its response, or for uploads created by older releases.

The hosted CI workflow passed on macOS, Windows and Linux. Release-candidate validation now includes versioned archives with checksums and extraction/startup checks. An opt-in streaming HTTPS test uploaded and downloaded 2,152,726,651 bytes in 411 parts with matching SHA-256 hashes and approximately 67 MiB sampled peak Go heap. Cancellation and a truncated response beyond 2 GiB preserved existing downloads; oversized uploads were rejected before staging. Run `HARBOR_GIB_TEST=1 go test -race -run 'Test(StreamingMultiGiBTransfers|UploadSizeLimitRejectsBeforeStaging)$' -v -timeout 20m .` to repeat it.

An isolated real macOS Keychain test passed credential save, replacement, reopen and deletion, and verified cleanup of its dummy entries. Safari 27.0.1 on macOS 27.0.1 checks against isolated HTTPS storage passed folder navigation, Tab/Enter filename completion, upload and download with byte comparisons, explicit picker confirmation, and bucket-switch selection clearing. These checks used synthetic files and credentials.

Remaining distribution work includes publisher signing and Mac notarization. Full 5 GiB transfers, arbitrary storage-provider behavior, WAN conditions, and recovery of arbitrary damaged OS credential vaults remain outside verified coverage. Windows x64 browser coverage remains emulated; native Windows CI tests provide backend coverage. See RELEASE.md for the candidate checklist and supported scope. An unsigned package must be identified as such even after the application reaches a stable version.

Regulated federal use remains outside the current release target. This app does not confer FedRAMP authorization or establish workstation, cryptographic-validation, audit, or organizational controls.

## License

Copyright 2026 John Dauphine. Licensed under the [Apache License, Version 2.0](LICENSE). Third-party dependencies retain their own licenses.
