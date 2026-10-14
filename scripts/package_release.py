#!/usr/bin/env python3
"""Build unsigned, versioned Harbor archives. Never publishes a release."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile

from generate_notices import render_notices

ROOT = Path(__file__).resolve().parents[1]
TARGETS = (("darwin", "arm64"), ("windows", "arm64"), ("windows", "amd64"))


def run(*args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, text=True, encoding="utf-8", **kwargs).strip()


def archive_name(version, target):
    system, arch = target
    suffix = "tar.gz" if system == "darwin" else "zip"
    return f"harbor-{version}-{system}-{arch}.{suffix}"


def package(binary, output, metadata, timestamp):
    """Normalize archive ownership, permissions, member order and timestamps."""
    system, arch = metadata["target"].split("/")
    executable = "harbor.exe" if system == "windows" else "harbor"
    instructions = (
        f"Harbor {metadata['version']} for {system}/{arch}\n\n"
        "This executable is unsigned and has not been notarized.\n"
        "Verify the archive against SHA256SUMS from your trusted download source.\n"
        "Open a terminal in this folder and run "
        + (".\\harbor.exe" if system == "windows" else "./harbor")
        + ". Harbor opens your default browser. If it cannot, open the printed local URL yourself. Use --no-browser to skip automatic opening.\n"
        "Keep that URL private. Press Ctrl+C in the terminal to stop Harbor.\n"
        "Use --version for build details and --help for command-line options.\n"
        "Settings default to .harbor/profiles.db under your user home directory.\n"
        "Credentials use the operating system credential vault.\n"
        "Publisher signing is not established by a checksum.\n"
        "See LICENSE for Harbor and THIRD_PARTY_NOTICES.txt for upstream notices.\n"
    )
    members = {
        executable: (binary.read_bytes(), 0o755),
        "README.txt": (instructions.encode(), 0o644),
        "build-info.json": ((json.dumps(metadata, sort_keys=True, indent=2) + "\n").encode(), 0o644),
    }
    for name in ("LICENSE", "THIRD_PARTY_NOTICES.txt"):
        document = ROOT / name
        if not document.is_file():
            raise RuntimeError(f"Required distribution document is missing: {name}")
        members[name] = (document.read_bytes(), 0o644)
    if system == "darwin":
        with output.open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                    for name, (data, mode) in sorted(members.items()):
                        entry = tarfile.TarInfo(name)
                        entry.size, entry.mode, entry.mtime = len(data), mode, timestamp
                        entry.uid = entry.gid = 0
                        entry.uname = entry.gname = ""
                        archive.addfile(entry, io.BytesIO(data))
    else:
        # The ZIP epoch avoids host timezone and build-time differences.
        with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, (data, mode) in sorted(members.items()):
                entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                entry.create_system = 3
                entry.external_attr = (0o100000 | mode) << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(entry, data, compresslevel=9)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="vX.Y.Z[-rc.N] or development label; no tag is created")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    parser.add_argument("--target", choices=["/".join(target) for target in TARGETS], help="build just one supported target")
    parser.add_argument("--allow-dirty", action="store_true", help="permit development builds from modified source")
    args = parser.parse_args()
    if not re.fullmatch(r"(?:v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?|development-[A-Za-z0-9.-]+)", args.version):
        parser.error("use a safe version label such as v0.1.0-rc.1 or development-local")
    dirty = bool(run("git", "status", "--porcelain"))
    if dirty and not (args.allow_dirty and args.version.startswith("development-")):
        parser.error("candidate/release archives require a clean checkout; --allow-dirty is only for development labels")
    notices = ROOT / "THIRD_PARTY_NOTICES.txt"
    if not notices.exists() or notices.read_bytes() != render_notices().encode("utf-8"):
        parser.error("third-party notices are missing or stale; regenerate, review and commit them before packaging")
    revision = run("git", "rev-parse", "HEAD")
    timestamp = int(run("git", "show", "-s", "--format=%ct", "HEAD"))
    toolchain = run("go", "env", "GOVERSION")
    args.output.mkdir(parents=True, exist_ok=True)
    checksums = []
    targets = [tuple(args.target.split("/"))] if args.target else TARGETS
    with tempfile.TemporaryDirectory(prefix="harbor-package-") as temporary:
        built = []
        for system, arch in targets:
            binary = Path(temporary) / f"harbor-{system}-{arch}"
            environment = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=true", "-ldflags",
                            f"-X main.version={args.version}", "-o", str(binary), "."],
                           cwd=ROOT, env=environment, check=True)
            metadata = dict(version=args.version, revision=revision, modified=dirty,
                            toolchain=toolchain, target=f"{system}/{arch}", publisher_signed=False)
            built.append((binary, metadata))
        # Build before writing output so a custom output directory cannot change
        # Go's embedded Git modified state halfway through a clean release build.
        if not dirty and (run("git", "rev-parse", "HEAD") != revision or run("git", "status", "--porcelain")):
            raise SystemExit("Source changed during the build; no candidate archives were created")
        for binary, metadata in built:
            system, arch = metadata["target"].split("/")
            output = args.output / archive_name(args.version, (system, arch))
            package(binary, output, metadata, timestamp)
            checksums.append(f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")
            print(output.name)
    (args.output / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")
    print("Unsigned archives and SHA256SUMS prepared; nothing was published.")


if __name__ == "__main__":
    main()
