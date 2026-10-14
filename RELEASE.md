# Building and distributing Harbor

The current workflow produces **unsigned development artifacts**. It does not create tags, publish GitHub releases, sign executables, or submit software for notarization. A public release remains a separate, deliberate step after the checks pass.

## Local checks and builds

The tested security baseline is **Go 1.26.9**, also required by `go.mod`. The official Go vulnerability checker found no advisories for the macOS ARM64, Windows ARM64, or Windows x64 source targets with this toolchain at the time of testing. Re-run the audit before each release; this is a point-in-time result.

Build from a known, clean Git revision. These commands use a macOS or Linux shell; race tests also require the platform's C compiler. Node 24 and Chromium are test dependencies, not application runtime dependencies.

```sh
export GOTOOLCHAIN=go1.26.9
go mod verify
go test -race -timeout 5m ./...
go vet ./...
node --test tests/*.test.cjs

go install golang.org/x/vuln/cmd/govulncheck@v1.3.0
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 govulncheck ./...
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 govulncheck ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 govulncheck ./...
```

The Go bin directory must be on your PATH for `govulncheck`. Set `HARBOR_TEST_CHROMIUM` to the installed Chromium/Chrome executable to require native browser coverage. Without a detected browser, that test skips; a skipped check is not a browser pass. CI requires the browser check on Linux.

```sh
harbor_version="development-$(git rev-parse --short=12 HEAD)"
mkdir -p dist
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$harbor_version" -o dist/harbor-macos-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$harbor_version" -o dist/harbor-windows-arm64.exe .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$harbor_version" -o dist/harbor-windows-amd64.exe .
```

Use an approved release version in place of the development label only when preparing that release. `--version` reports the label, Git revision, toolchain and target. Matching source, toolchain and build options make the build inputs repeatable; signing and timestamping change the final bytes. Compilation does not replace runtime tests on each supported platform. Windows x64 testing under ARM emulation does not establish native x64 coverage.

## Checksums and packaging

Generate checksums **after** any signing or final packaging. On macOS:

```sh
(cd dist && shasum -a 256 harbor-* > SHA256SUMS)
(cd dist && shasum -a 256 -c SHA256SUMS)
```

On Linux, substitute `sha256sum` for `shasum -a 256`. On Windows, `Get-FileHash -Algorithm SHA256 .\harbor-windows-amd64.exe` prints a digest to compare with the manifest. Checksums detect changed files; they do not authenticate the publisher when the executable and manifest come from the same untrusted source.

The CI download contains raw unsigned executables and a checksum manifest. Artifact extraction may lose the macOS executable permission; restore it with `chmod +x harbor-macos-arm64` for local testing. A public macOS archive should preserve executable permissions. Do not package local settings, credentials, caches, session URLs or test data.

## Publisher signing prerequisites

- **macOS:** Apple Developer membership alone is insufficient. The publisher needs a valid **Developer ID Application** signing identity and its private key, hardened-runtime signing, timestamping, and an authorized notarization workflow. No valid local signing identity was available during this preparation. Signing and notarization must be completed and checked before describing a download as Developer ID signed or notarized. See [Apple's distribution-signing guide](https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac/) and [notarization guide](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).
- **Windows:** These builds have no publisher signature. Authenticode signing requires a publisher-controlled certificate/private key or an approved signing service, with timestamping and verification. Signing does not guarantee the absence of SmartScreen reputation warnings. See [Microsoft SignTool documentation](https://learn.microsoft.com/en-us/windows/win32/seccrypto/signtool).

Keep signing keys and notarization credentials out of the repository and build artifacts. No developer-account login, certificate issuance, signing-service setup or release publication is automated by this repository's current CI.
