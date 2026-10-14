import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

from package_release import ROOT, archive_name, package


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
                            for name in ("LICENSE", "THIRD_PARTY_NOTICES.txt"):
                                self.assertEqual(archive.extractfile(name).read(), (ROOT / name).read_bytes())
                            self.assertEqual(set(archive.getnames()), {executable, "README.txt", "build-info.json", "LICENSE", "THIRD_PARTY_NOTICES.txt"})
                            self.assertTrue(all(Path(member.name).name == member.name for member in archive.getmembers()))
                    else:
                        with zipfile.ZipFile(output) as archive:
                            self.assertEqual((archive.getinfo(executable).external_attr >> 16) & 0o777, 0o755)
                            self.assertEqual(archive.read(executable), binary.read_bytes())
                            self.assertEqual(json.loads(archive.read("build-info.json")), metadata)
                            for name in ("LICENSE", "THIRD_PARTY_NOTICES.txt"):
                                self.assertEqual(archive.read(name), (ROOT / name).read_bytes())
                            self.assertEqual(set(archive.namelist()), {executable, "README.txt", "build-info.json", "LICENSE", "THIRD_PARTY_NOTICES.txt"})
                            self.assertTrue(all(Path(name).name == name for name in archive.namelist()))


if __name__ == "__main__":
    unittest.main()
