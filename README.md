# Harbor S3 Browser

Work in progress: a Go executable serving an embedded browser UI on loopback only.

Current source includes saved connection profiles with OS credential vault storage, S3 bucket and prefix browsing, pagination, and a drafted transfer backend. Browser uploads now stage files locally and support background progress, cancellation, and retry. Downloads currently use a direct browser download and do not yet support queue cancellation or retry. This is an early test build; do not use production or federal data.

## Development

Requires Go 1.26 or newer. Run `go test ./...` then `go run .`. Open the session URL printed by the program and keep it private. Stop with Ctrl+C. Source has no dependency on Node, Wails, WebView2, or a separate server.

Build: `go build -o harbor .`. Cross-build Windows with `GOOS=windows GOARCH=arm64 go build -o harbor.exe .` (or `amd64`).

## Verification

`go test -race ./...` passes boundary tests for Host, Origin, session authorization, HTTPS-only endpoints, secret separation, and prefix pagination against a local TLS fixture. macOS ARM64 and Windows ARM64/AMD64 executables compile. Real AWS/on-premises integration and Windows runtime behavior are not yet verified.

## Current limits

- Upload staging is limited to 5 GiB and uses disk space; failed/cancelled upload files remain available for retry in the user cache.
- Downloads are buffered by the browser and have no transfer-queue progress/cancel/retry yet.
- Connection editing, credential removal, FIPS build validation, signing, and audit logging are pending.
- Do not infer FedRAMP compliance from these application-level safeguards.

## Security scope

The server binds only to 127.0.0.1 and requires a random session token for API requests, checks Host and Origin, and serves bundled assets with a restrictive content security policy. Storage endpoints require HTTPS with certificate validation. Credentials use macOS Keychain or Windows Credential Manager through go-keyring; profile files contain connection metadata only.

Assumptions: patched OS and browser, disk encryption, protected user account, and approved storage endpoints. FedRAMP High compliance, validated cryptography configuration, real storage integrations, and platform packaging are not verified.

Independent implementation inspired by familiar S3 file-manager layouts. No code or assets copied from CS Browser, brows3, BucketDock, or Cyberduck.
