from pathlib import Path
import tempfile
import unittest

from generate_notices import json_stream, notice_documents


class NoticeTests(unittest.TestCase):
    def test_notices_follow_compiled_package_ancestors(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = root / "internal" / "included" / "child"
            package.mkdir(parents=True)
            (root / "LICENSE").write_bytes(b"Original module license\n")
            (root / "NOTICE.txt").write_bytes(b"Original copyright\n")
            (package.parent / "LICENSE").write_bytes(b"Bundled component license\n")
            excluded = root / "testdata"
            excluded.mkdir()
            (excluded / "LICENSE").write_bytes(b"Unused test fixture license\n")
            (root / "license-check.yml").write_bytes(b"not an attribution document")
            paths = notice_documents(root, {package})
            self.assertEqual([p.relative_to(root).as_posix() for p in paths],
                             ["LICENSE", "NOTICE.txt", "internal/included/LICENSE"])
            self.assertEqual(paths[0].read_bytes(), b"Original module license\n")

    def test_go_json_stream_handles_consecutive_package_objects(self):
        self.assertEqual(list(json_stream(' {"ImportPath":"a"}\n\n{"ImportPath":"b"}\n')),
                         [{"ImportPath": "a"}, {"ImportPath": "b"}])


if __name__ == "__main__":
    unittest.main()
