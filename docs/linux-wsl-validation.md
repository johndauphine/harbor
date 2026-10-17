# Linux WSL validation — 2026-10-16

## Environment and scope

Used the existing Ubuntu WSL2 distribution: Ubuntu 26.04.1 LTS, Linux
6.18.54.1-microsoft-standard-WSL2, native `aarch64` on this Windows ARM64 host.
WSLg exposes both DISPLAY and a Wayland socket. No distribution, Windows
optional feature, GUI package, credential service or system compiler was installed.

Downloaded Go 1.26.9 for Linux ARM64 from go.dev into ignored
`build/tools/linux-arm64/go`. Build caches stay under ignored `build/tools`.
No real Harbor configuration or credentials were used. The existing Windows
x64 Harbor inspection session was left running and untouched.

These checks cover the existing Linux **browser/server** application. The new
Wails desktop entry point remains Windows-only: `desktop_disabled.go` selects
browser/server mode on Linux even when the desktop tag is supplied. WSLg
availability alone does not establish Linux Wails desktop support.

## Passed

- Native Linux ARM64 `go test -timeout 5m ./...` (6.085 seconds of test runtime,
  excluding initial compilation). Includes local HTTPS S3 fixtures, upload and
  download integrity, cancellation, retry, interrupted streams, shutdown and
  transfer-journal recovery regressions. Opt-in external-provider and multi-GiB
  tests were not enabled.
- Native Linux ARM64 `go vet ./...`.
- Native Linux ARM64 build: `build/bin/harbor-linux-arm64`, ELF machine 183.
- Cross-build for Linux x64: `build/bin/harbor-linux-amd64`, ELF machine 62.
- Native ARM64 binary reports Go 1.26.9 and `linux/arm64` with `--version`.
- Four Python packaging/notice unit tests passed under Ubuntu.
- `python3 scripts/generate_notices.py --check` passed under Ubuntu.
- Built ARM64 executable started twice with one disposable SQLite database and
  an isolated HOME/cache directory. Each launch passed:
  - embedded Harbor page loading and authenticated profile/transfer API calls;
  - rejection of missing authorization, foreign Origin and unsupported method;
  - confirmation that its listener binds only to IPv4 loopback;
  - rejection of a second process opening the same settings database;
  - normal SIGTERM shutdown, returning exit code 0.
- Restart after graceful shutdown successfully reopened the isolated database.

## Limits and next step

Linux x64 was built but not run: this WSL environment is ARM64 and no Linux x64
emulator was found. Existing release packaging supports Linux x64; this ARM64
exercise does not add an ARM64 release asset or declare a new supported target.

No Linux graphical browser, native Wails window, real Secret Service credential
vault or cloud-provider workflow was exercised. Tests use synthetic storage and
credentials. Race checks were not run because no Linux C compiler is installed.

The smallest next step for Linux desktop coverage is to implement a Linux Wails
entry point, then add its GTK/WebKitGTK development dependencies to this existing
WSLg environment and run a real window/transfer smoke test. A new distribution or
Windows feature change is unnecessary. Nothing was merged or released.
