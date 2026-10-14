# Harbor S3 Browser

Work in progress: a Go executable serving an embedded browser UI on loopback only.

Current source includes saved connection profiles with OS credential vault storage, S3 bucket and prefix browsing, pagination, and a drafted transfer backend. The UI and transfer HTTP endpoints are incomplete. This initial snapshot is not a tested release and must not be used for production or federal data.

## Development

Requires Go 1.26 or newer. Run `go test ./...` then `go run .`. Open the session URL printed by the program and keep it private. Stop with Ctrl+C. Source currently has no dependency on Node or a separate server.

## Security scope

The server binds only to 127.0.0.1 and requires a random session token for API requests, checks Host and Origin, and serves bundled assets with a restrictive content security policy. Storage endpoints require HTTPS with certificate validation. Credentials use macOS Keychain or Windows Credential Manager through go-keyring; profile files contain connection metadata only.

Assumptions: patched OS and browser, disk encryption, protected user account, and approved storage endpoints. FedRAMP High compliance, validated cryptography configuration, real storage integrations, and platform packaging are not verified.

Independent implementation inspired by familiar S3 file-manager layouts. No code or assets copied from CS Browser, brows3, BucketDock, or Cyberduck.
