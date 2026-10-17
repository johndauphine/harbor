# Windows desktop preview validation — 2026-10-16

This records the initial preview checks. Subsequent native picker, large-file,
AWS transfer, x64 smoke-test, and signing timing results are documented in
[the 2026-10-17 investigation](windows-folder-upload-fix.md). The limitations
below describe the initial validation date, not all current implementation work.

This is post-v1.0.0 development on `codex/windows-wails`, with no merge or release.

## Design and setup

- Pinned Wails v2.15.0. The [official installation documentation](https://v2.wails.io/docs/gettingstarted/installation/)
  identifies v2 as stable and v3 as beta, and supports Windows ARM64/x64.
- This workstation runs Windows 11 ARM64. Go 1.26.9 was downloaded from go.dev
  into ignored `build/tools/arm64/go`; an x64 toolchain was also used for initial
  compilation. No system PATH or security policy was changed.
- Existing Node 24.19.0 and WebView2 155.0.4283.62 were detected. The plain JS/CSS
  frontend needs no npm build. NSIS, UPX and a C compiler were not needed.
- Wails CLI runs at its pinned version with `go run`; it is not globally installed.
- Windows uses a standard resizable native frame and system title-bar theme.
  Existing UI, in-app file pickers and transfer implementation are retained.
- The desktop asset handler reuses the existing API with strict desktop
  host/origin checks. It is accessible through the private WebView transport,
  without opening a TCP port. Browser builds retain session authentication.
- Store locking, credentials and transfer journals use the existing paths.
  Window close rejects new work, cancels operations and waits for shutdown.

## Passed checks

- `go test ./...` on Windows x64 and native Windows ARM64.
- `go test -tags desktop,production ./...` on native Windows ARM64.
- `go vet -tags desktop,production ./...` and `go mod verify`.
- `node --test tests/*.test.cjs`: 73 passed, one optional browser test skipped.
- Real WebView2 smoke test with `HARBOR_DESKTOP_TEST=1`: frontend/CSS loading,
  private profile/transfer API, connection dialog, Blob upload through WebView2
  to a local HTTPS S3 fixture, and download with byte comparison. The test uses
  temporary SQLite storage and an in-memory vault, not real credentials.
- The full Go suite includes multipart failures/retries, download cancellation,
  interrupted streams, shutdown admission and waiting, and journal restart tests.
- Pinned Wails CLI builds packaged Windows ARM64 and x64 executables, including
  icon, manifest, DPI awareness and development version metadata.
- Packaged native ARM64 executable opened a visible window titled Harbor using
  an isolated database, then exited with code 0 after normal window closure.
- Desktop dependency notices regenerated; `generate_notices.py --check` passes.
- `git diff --check` passes.

## Incomplete checks and limitations

- Python packaging tests: three passed; the version-formatting test's second
  temporary executable was blocked by Windows Application Control (WinError
  4551). No policy was disabled. This is not recorded as a passing check.
- Real cloud-provider transfers through the desktop window, Windows Credential
  Manager end-to-end desktop interaction, and large-file WebView2 staging have
  not been validated. Existing backend tests are evidence for the reused engine,
  not complete coverage of every WebView2 transport condition.
- Windows ARM64 was exercised natively. The packaged x64 executable was also
  exercised under Windows ARM64 emulation; native x64 hardware remains untested.
- Race-detector checks were not run locally; the existing x64 CI job retains
  its race checks. Updated CI has not been executed remotely in this task.
- macOS desktop integration, native OS file dialogs, installers and signing
  remain future work. Existing browser releases and packaging remain available.
- No independent merge reviews were requested or performed; the repository's
  two-agent review gate must be satisfied before any future merge.

## Additional x64 emulation check — 2026-10-16

- Rebuilt `harbor-desktop-amd64.exe` with Go 1.26.9 windows/amd64 and Wails
  v2.15.0. Confirmed the executable's PE machine is `0x8664` (AMD64) while the
  host OS reports ARM64.
- The packaged x64 application opened a visible native window titled Harbor
  against a fresh disposable SQLite database. WebView2 initialized successfully.
  Closing the window normally returned process exit code 0.
- The separate x64 Go WebView2 integration test runner was blocked by Windows
  Application Control (WinError 4551) on both the initial run and an unchanged
  retry. No security settings were modified. Therefore profile/transfer API,
  dialog interaction and upload/download integrity through an x64 WebView2
  host are **not verified**. The earlier ARM64 smoke results cannot establish
  those x64 behaviors.
- The smoke test now checks exact decoded upload bytes, including decoding AWS
  chunked request bodies when present, rather than searching for a substring.
  This strengthened assertion has not yet executed successfully under x64.
- The user closed the ARM64 app. The final x64 inspection launch uses disposable
  settings and leaves only that x64 instance running; real profiles and transfers
  are not accessed by these checks.
