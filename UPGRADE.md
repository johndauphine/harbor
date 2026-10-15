# Upgrading Harbor

Harbor keeps connection settings in a SQLite database and credentials in the current user's operating-system credential vault. Copying the executable upgrades the application; it does not copy or back up those settings or credentials.

## Before replacing the executable

1. Finish or cancel transfers, then stop every Harbor process using the configuration. Closing its browser tab does not stop the application. Older builds do not honor the new single-instance lock, so they must be stopped explicitly.
2. Keep an offline backup of the configuration directory after the application has stopped. By default this is `.harbor` under your user home directory. If you use `--db`, back up that database and any accompanying SQLite journal files while all processes that use it are stopped. Protect backups because connection names, endpoints and paths can be sensitive.
3. Make sure you can obtain replacement credentials from your storage provider or an existing secure source. A database backup contains no access keys, secret keys or session tokens. It is not a credential backup.
4. Replace the executable, then start it with the same operating-system account and database path. Review the connections and test one before starting important transfers.

Keep the database on a trusted local filesystem. Network-share locking is not verified. Do not rename, replace or delete the database or its `.lock` sidecar while Harbor is running. The sidecar may remain after exit; its existence does not mean Harbor is still running. The operating system releases the actual lock automatically.

## What the current format changes

Existing connection records and their original vault entries remain readable. Opening a legacy database adds the supporting tables without changing its saved connection settings or rotating its credentials. It also binds the configuration to its canonical file location before accessing the credential vault. Later attempts to open a copy or move of that database at a different location stop with an error; symbolic links to the original file remain usable. Saving settings without replacing credentials retains the existing vault entry.

When you replace credentials, Harbor writes a new vault entry and commits its private reference together with the connection settings. It then retires the old entry. Failed saves retain the previously committed connection and credentials. If a save or deletion committed but vault cleanup failed, Harbor reports that distinction. Unlock the vault and restart Harbor to retry the queued cleanup. A startup cleanup failure is reported in the terminal.

Malformed or unreadable configuration now stops startup with an error instead of silently displaying an empty or partial connection list. Preserve the database for recovery; do not assume the missing list means your settings were deleted. Restore a known valid configuration backup at its original location, with every Harbor process stopped, or choose a new empty database with `--db` and recreate the connections.

## Transfer recovery after upgrading

New transfers are journaled in the settings database. A private directory named `<database>.transfers` beside that database holds staged upload data and cached downloads. Keep this directory together with an offline database backup if you need its retryable uploads. Opening the same database after a crash restores interrupted transfers as failed, with Retry available; normal shutdown preserves cancelled transfers too. Retries restart from the beginning and keep the original download destination and overwrite decision. They never start automatically.

Startup removes recorded partial downloads and unreferenced staging files owned by this database. Retryable upload bodies remain until their transfer succeeds or the connection is deleted. Harbor records each multipart upload ID before sending parts and attempts to abort that exact upload on restart or before Retry. If the server or credential vault is unavailable, or the connection settings have changed, cleanup remains pending and is shown in Transfers. Restore access and the original connection settings, then Retry. A connection cannot be deleted while its recorded multipart cleanup is pending.

Transfers interrupted in older builds cannot be reconstructed because those builds did not journal their history or multipart IDs. Do not delete shared legacy cache files while another Harbor instance may be using them. Provider lifecycle rules can clean up incomplete multipart uploads that were never journaled, including a crash between server-side initiation and recording its response.

Downgrading while recoverable transfers exist is unsupported: older executables neither restore this journal nor protect its staged files. Finish or discard that work before switching versions.

## Backups, moving computers and returning to an older build

**Restoring a database does not restore its credentials.** Credential rotation may have removed the vault entry referenced by an older backup. The restored connection can appear in Harbor but fail authentication. Edit it and enter a complete access key and secret key, plus a session token if required. Partial key replacement needs the previous vault entry and cannot repair a missing one.

Moving a database to another location, computer or operating-system account does not move its vault entries, and location-bound databases reject a different path. For a move, preserve the original and start with a new empty database; recreate the connections and re-enter credentials. There is no built-in portable credential export, automatic vault restore or location-rebinding command.

Downgrading a migrated database to a build that predates private credential references is unsupported. Those builds look up keys using the old profile ID and can no longer read rotated credentials. Restoring an old database alone does not make a downgrade safe. Prefer the current build; if you must use an older build, stop all current processes and use a new empty configuration with newly entered credentials. Preserve the existing configuration separately for recovery.

**Do not run copies of the same database as independent profiles, even under different filenames.** Copies retain the same vault references. Updating or deleting a connection in one copy can remove credentials still referenced by another. Current location-bound databases reject copies opened at a different canonical path before reading or modifying the vault. Older backups without a location marker cannot be identified as copies: their first current launch binds wherever they are opened. The per-database process lock cannot protect these old copies or older executables that do not participate in locking. Never open an unbound legacy backup as an independent configuration. For independent configurations, start with a new empty database and recreate the connections rather than copying a populated one.

## What is verified

Automated tests use temporary SQLite databases and synthetic vaults. They cover opening the legacy schema, preserving multiple connection settings and untouched credentials, metadata-only saves, mixed legacy/current profiles after rotation, reopening, failed migration commits, malformed-store startup rejection, and restoring metadata whose old credentials have been retired. They also verify recovery at the original location by entering a complete replacement key pair, rejection of copied/moved bound databases before vault access, and successful reopening through a symbolic link. An opt-in macOS integration test (`HARBOR_KEYCHAIN_TEST=1 go test -run TestMacOSKeychainProfileLifecycle .`) uses a randomly named test-only Keychain service and synthetic credentials to verify save, replacement, reopen and deletion; it removes and verifies removal of every tracked test entry. It never reads the Harbor credential service.

These tests establish application behavior, not recovery of arbitrary damaged databases or restoration of a user's real OS credential vault. Keep recoverable credentials independently and avoid treating a successful configuration backup as a complete disaster-recovery plan.
