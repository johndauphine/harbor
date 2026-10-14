#!/usr/bin/env python3
"""Verify one locally runnable archive using an isolated configuration directory."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import re
import signal
import subprocess
import tarfile
import tempfile
import threading
import urllib.request
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    args = parser.parse_args()
    archive = args.archive.resolve()
    sums = (archive.parent / "SHA256SUMS").read_text().splitlines()
    expected = dict(line.split(None, 1)[::-1] for line in sums)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected.get(archive.name):
        raise SystemExit("Archive checksum did not match SHA256SUMS")
    with tempfile.TemporaryDirectory(prefix="harbor-archive-check-") as temporary:
        root = Path(temporary)
        unpacked = root / "unpacked"
        unpacked.mkdir()
        if archive.name.endswith(".tar.gz"):
            with tarfile.open(archive) as contents:
                for member in contents.getmembers():
                    if Path(member.name).name != member.name or not member.isfile():
                        raise SystemExit("Unexpected archive member")
                    target = unpacked / member.name
                    target.write_bytes(contents.extractfile(member).read())
                    target.chmod(member.mode & 0o777)
        else:
            with zipfile.ZipFile(archive) as contents:
                for member in contents.infolist():
                    if Path(member.filename).name != member.filename or member.is_dir():
                        raise SystemExit("Unexpected archive member")
                    (unpacked / member.filename).write_bytes(contents.read(member))
        metadata = json.loads((unpacked / "build-info.json").read_text())
        machine = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
        host = f"{'windows' if os.name == 'nt' else platform.system().lower()}/{machine}"
        if host != metadata["target"]:
            raise SystemExit(f"Archive requires {metadata['target']}; current host is {host}")
        executable = unpacked / ("harbor.exe" if os.name == "nt" else "harbor")
        version = subprocess.check_output([str(executable), "--version"], text=True, encoding="utf-8").strip()
        if (metadata["version"] not in version or metadata["target"] not in version
                or metadata["toolchain"] not in version or metadata["revision"][:12] not in version
                or ("-modified" in version) != metadata["modified"]):
            raise SystemExit("Executable version does not match archive metadata")
        settings = root / "settings" / "profiles.db"
        # The application sees only a fresh home/cache and explicit fresh database.
        environment = dict(os.environ, HOME=str(root), USERPROFILE=str(root),
                           XDG_CACHE_HOME=str(root / "cache"), LOCALAPPDATA=str(root / "cache"))
        flags = subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0
        process = subprocess.Popen([str(executable), "--no-browser", "--db", str(settings)], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, encoding="utf-8", env=environment, creationflags=flags)
        lines = queue.Queue()
        def read_lines():
            for line in process.stdout:
                lines.put(line)
        threading.Thread(target=read_lines, daemon=True).start()
        try:
            local_url = None
            for _ in range(5):
                line = lines.get(timeout=15)
                match = re.search(r"Open (http://127\.0\.0\.1:[0-9]+/)", line)
                if match:
                    local_url = match.group(1)
                    break
            if local_url is None:
                raise RuntimeError("Application did not print a loopback URL")
            with urllib.request.urlopen(local_url, timeout=5) as response:
                if response.status != 200 or b"Harbor" not in response.read():
                    raise RuntimeError("Embedded browser UI did not load")
            if not settings.exists():
                raise RuntimeError("Isolated settings database was not created")
            process.send_signal(signal.CTRL_BREAK_EVENT if os.name == "nt" else signal.SIGINT)
            process.wait(timeout=35)
            if process.returncode != 0:
                raise RuntimeError("Application did not stop cleanly")
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        print(f"PASS: checksum, extracted permissions/version, isolated startup, browser page and shutdown ({host}).")
        print(version)


if __name__ == "__main__":
    main()
