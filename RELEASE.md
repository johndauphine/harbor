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

The Go bin directory must be on your PATH for `govulncheck`. Set `HARBOR_TEST_CHROMIUM` to the installed Chromium/Chrome executable to require native browser coverage. Without a detected browser, that test skips; a skipped check is not a browser pass. CI requires the browser check on Linux and native Windows x64, using Chrome or Edge with a disposable browser profile and mocked storage calls.

From a clean checkout, prepare an explicitly named candidate without creating a tag or publishing anything:

```sh
python3 scripts/package_release.py --version v0.1.0-rc.1 --output dist
```

Choose the version for the candidate being evaluated. The script rejects modified source for candidate and release labels. Developers may use `--version development-local --allow-dirty`; those archives explicitly record the modified-source state. The script builds all four targets with embedded version labels, normalizes archive timestamps/ownership/permissions, includes build metadata and a short startup guide, and writes checksums. Repeated packaging with the same source, compiler, Python/compression runtime and options is repeatable. Signing/timestamping changes the final bytes.

Each archive contains only the executable, `README.txt`, `build-info.json`, Harbor's Apache 2.0 `LICENSE`, and `THIRD_PARTY_NOTICES.txt`. No settings, credentials, caches, session URLs or test data are included. `--version` reports the label, Git revision, toolchain and target. CI uses a development revision label and uploads the archives as workflow artifacts; it does not publish releases.

## Preserving dependency notices

`THIRD_PARTY_NOTICES.txt` preserves the license, notice, copyright, patent and author files supplied by the actual non-test runtime imports across all four shipped targets. It includes the Go runtime/toolchain license and applicable vendored-package notices, AWS SDK/Smithy notices, and the bundled third-party notices supplied by the SQLite/libc modules. Module-wide upstream documents are kept intact; some describe optional or test-only components without implying those components are linked into Harbor.

After changing dependencies or the Go toolchain, run:

```sh
python3 scripts/generate_notices.py
python3 scripts/generate_notices.py --check
```

Review and commit the generated changes. CI and the packaging command check for stale notices; every archive must contain both Harbor's `LICENSE` and the upstream notices. This preserves the upstream attribution text and is not a claim of legal certification.

## Checksums, extraction and smoke checks

The output consists of a macOS `.tar.gz`, two Windows `.zip` files, a Linux x64 `.tar.gz`, and `SHA256SUMS`. The tar archive preserves the macOS executable permission even when downloaded inside a GitHub Actions artifact. Extract the platform archive itself before running Harbor.

On macOS, verify the archives before extraction:

```sh
(cd dist && shasum -a 256 -c SHA256SUMS)
tar -xzf dist/harbor-v0.1.0-rc.1-darwin-arm64.tar.gz -C /path/to/empty/folder
```

On Linux, substitute `sha256sum` for `shasum -a 256`. On Windows, compare `Get-FileHash -Algorithm SHA256 .\dist\harbor-v0.1.0-rc.1-windows-amd64.zip` with the manifest, then use `Expand-Archive` to extract the ZIP into an empty folder. Checksums detect changed files; they do not authenticate the publisher when the archive and manifest come from the same untrusted source.

The native smoke checker verifies the checksum, extracts into a temporary directory, checks version/target metadata, starts Harbor with a fresh explicit database and isolated home/cache, fetches the embedded browser page, and checks graceful shutdown. It creates no storage connection and uses no cloud credentials:

```sh
python3 scripts/check_archive.py dist/harbor-v0.1.0-rc.1-darwin-arm64.tar.gz
```

Use `python scripts/check_archive.py dist\harbor-v0.1.0-rc.1-windows-amd64.zip` on Windows x64, or substitute `windows-arm64.zip` on Windows ARM. The checker deliberately requires a native architecture match; cross-compilation is not a runtime pass. CI runs this check on its native macOS ARM64 and Windows x64 runners, and runs the opt-in multi-GiB transfer test on all three test operating systems after verifying temporary disk space. Windows ARM64 archive execution remains a separate native-device check. Keep checksum files beside their archives. After future publisher signing or repackaging, regenerate checksums for the final downloadable bytes.

## Candidate platform scope

| Archive | Intended platform | Current evidence and remaining scope |
| --- | --- | --- |
| `darwin-arm64.tar.gz` | Apple Silicon macOS | Local Safari/Chrome application checks; native archive extraction/startup checks must pass for each candidate. |
| `windows-arm64.zip` | Windows 11 ARM64 | Earlier Edge application checks in the Parallels Windows ARM VM; repeat against the final candidate archive. |
| `windows-amd64.zip` | Windows 11 x64 | Hosted Windows x64 Go tests and earlier Edge checks under ARM emulation; full native x64 browser/file workflow coverage remains a release-validation task. |

Intel macOS and Linux ARM64 executables are not included. Linux x64 packaging is supported; an unlocked Secret Service login keyring, user D-Bus session and system CA certificates are required. `xdg-open` is optional when using `--no-browser`. Full interactive Linux desktop/keyring coverage is not yet established. No minimum macOS version or Windows version below Windows 11 has been established. ARM emulation checks do not establish native x64 browser coverage. State these limits alongside a candidate rather than implying wider compatibility.

## Release-candidate checklist

- [ ] Choose a clean source revision and candidate version; all CI jobs pass on that exact revision.
- [ ] Run module verification, race tests, vet, frontend/native-browser tests and a fresh vulnerability audit for every shipped target.
- [ ] Produce archives with the packaging script; confirm archive checksums, embedded revision/version, member list and executable permissions. Run packaging regressions with `python3 -m unittest discover -s scripts -p 'test_*.py'`.
- [ ] Run `check_archive.py` on native macOS ARM64, Windows ARM64 and Windows x64 using the actual candidate downloads.
- [ ] Verify browsing, navigation, file dialogs, upload/download content, cancellation/retry, credential edits, deletion safeguards and graceful shutdown in a browser on each supported platform using disposable data. Record OS/browser versions and any emulation.
- [ ] Review upgrades using a backed-up disposable configuration and ensure existing saved connections still work; confirm no test settings or credentials are packaged.
- [ ] Resolve remaining functional defects; document user-visible limitations and agreed support scope. Include the selected Apache 2.0 LICENSE file in every public archive.
- [ ] Decide whether the release is explicitly unsigned, or complete publisher signing/notarization and validate the final downloaded archives. Unsigned status is an explicit distribution limitation, not proof of application instability.
- [ ] Review release notes and final checksums, then separately approve publication. Passing this checklist does not publish or tag anything automatically.

## Publisher signing prerequisites

- **macOS:** Apple Developer membership alone is insufficient. The publisher needs a valid **Developer ID Application** signing identity and its private key, hardened-runtime signing, timestamping, and an authorized notarization workflow. The developer membership is currently expired and no valid local signing identity was available during this preparation; renewal and an appropriate identity are prerequisites for Apple notarization. Signing and notarization must be completed and checked before describing a download as Developer ID signed or notarized. See [Apple's distribution-signing guide](https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac/) and [notarization guide](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).
- **Windows:** These builds have no publisher signature. Authenticode signing requires a publisher-controlled certificate/private key or an approved signing service, with timestamping and verification. Signing does not guarantee the absence of SmartScreen reputation warnings. See [Microsoft SignTool documentation](https://learn.microsoft.com/en-us/windows/win32/seccrypto/signtool).

Keep signing keys and notarization credentials out of the repository and build artifacts. No developer-account login, certificate issuance, signing-service setup or release publication is automated by this repository's current CI.
