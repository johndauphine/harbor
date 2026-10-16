import hashlib
import json
import os
import subprocess
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

from package_release import ROOT, archive_name, package, release_ldflags


class PackagingTests(unittest.TestCase):
    def test_archives_preserve_contents_permissions_and_repeatability(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "binary"
            binary.write_bytes(b"disposable executable fixture")
            for target in ("darwin/arm64", "windows/arm64", "windows/amd64", "linux/amd64"):
                with self.subTest(target=target):
                    metadata = dict(version="v0.1.0-rc.1", target=target, revision="fixture", modified=False,
                                    toolchain="go1.26.9", publisher_signed=False)
                    output = root / archive_name(metadata["version"], target.split("/"))
                    package(binary, output, metadata, 1700000000)
                    digest = hashlib.sha256(output.read_bytes()).digest()
                    package(binary, output, metadata, 1700000000)
                    self.assertEqual(digest, hashlib.sha256(output.read_bytes()).digest())
                    executable = "harbor.exe" if target.startswith("windows") else "harbor"
                    if executable == "harbor":
                        with tarfile.open(output) as archive:
                            entry = archive.getmember(executable)
                            self.assertEqual(entry.mode, 0o755)
                            self.assertEqual((entry.uid, entry.gid), (0, 0))
                            self.assertEqual(archive.extractfile(entry).read(), binary.read_bytes())
                            self.assertEqual(json.load(archive.extractfile("build-info.json")), metadata)
                            self.assertIn(b"Read UPGRADE.md before replacing", archive.extractfile("README.txt").read())
                            for name in ("LICENSE", "THIRD_PARTY_NOTICES.txt", "UPGRADE.md"):
                                self.assertEqual(archive.extractfile(name).read(), (ROOT / name).read_bytes())
                            self.assertEqual(set(archive.getnames()), {executable, "README.txt", "build-info.json", "LICENSE", "THIRD_PARTY_NOTICES.txt", "UPGRADE.md"})
                            self.assertTrue(all(Path(member.name).name == member.name for member in archive.getmembers()))
                    else:
                        with zipfile.ZipFile(output) as archive:
                            self.assertEqual((archive.getinfo(executable).external_attr >> 16) & 0o777, 0o755)
                            self.assertEqual(archive.read(executable), binary.read_bytes())
                            self.assertEqual(json.loads(archive.read("build-info.json")), metadata)
                            self.assertIn(b"Read UPGRADE.md before replacing", archive.read("README.txt"))
                            for name in ("LICENSE", "THIRD_PARTY_NOTICES.txt", "UPGRADE.md"):
                                self.assertEqual(archive.read(name), (ROOT / name).read_bytes())
                            self.assertEqual(set(archive.namelist()), {executable, "README.txt", "build-info.json", "LICENSE", "THIRD_PARTY_NOTICES.txt", "UPGRADE.md"})
                            self.assertTrue(all(Path(name).name == name for name in archive.namelist()))


    def test_release_version_without_toolchain_vcs_metadata(self):
        # Linked Git worktrees use a .git file, which Go 1.26 does not recognize.
        # Model the resulting absent build settings without depending on the
        # surrounding checkout type, then execute the real version formatter.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "version.go").write_bytes((ROOT / "version.go").read_bytes())
            (root / "main.go").write_text('package main\nimport "fmt"\nfunc main() { fmt.Println(versionText()) }\n')
            (root / "go.mod").write_text('module harbor-version-regression\ngo 1.26.9\n')
            executable = root / ("version.exe" if os.name == "nt" else "version")
            revision = "0123456789abcdef0123456789abcdef01234567"
            for modified in ("false", "true"):
                with self.subTest(modified=modified):
                    subprocess.run(["go", "build", "-buildvcs=false", "-ldflags",
                                    release_ldflags("v1.0.0-rc.1", revision, modified == "true"),
                                    "-o", str(executable), "."], cwd=root, check=True)
                    output = subprocess.check_output([str(executable)], text=True)
                    suffix = "-modified" if modified == "true" else ""
                    self.assertIn(f"Harbor v1.0.0-rc.1 ({revision[:12]}{suffix};", output)
                    self.assertNotIn("unknown", output)


if __name__ == "__main__":
    unittest.main()
