import pathlib
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).resolve().parents[1] / "package-layer.py"


class PackageLayerTests(unittest.TestCase):
    def test_archive_has_only_binary_and_notices_with_linux_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            binary = root / "gateway"
            binary.write_bytes(b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0")
            for name in ("LICENSE", "NOTICE", "THIRD-PARTY-LICENSES.md"):
                (root / name).write_text(name, encoding="utf-8")
            (root / "credentials.env").write_text("must-not-be-packaged", encoding="utf-8")
            archives = [root / "first.tar", root / "second.tar"]
            for output in archives:
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), str(root), str(binary), str(output)],
                    capture_output=True, check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(archives[0].read_bytes(), archives[1].read_bytes())
            with tarfile.open(archives[0]) as archive:
                self.assertEqual(
                    archive.getnames(),
                    ["new-api", "licenses/LICENSE", "licenses/NOTICE",
                     "licenses/THIRD-PARTY-LICENSES.md"],
                )
                for member in archive.getmembers():
                    self.assertEqual(member.mode, 0o755 if member.name == "new-api" else 0o644)
                    self.assertEqual((member.uid, member.gid, member.mtime), (0, 0, 0))
                self.assertEqual(archive.extractfile("new-api").read(), binary.read_bytes())

    def test_non_linux_amd64_input_is_rejected_before_output_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for header in (b"MZ", b"\x7fELF\x01\x01" + b"\0" * 14):
                binary = root / "gateway"
                binary.write_bytes(header)
                output = root / "layer.tar"
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), str(root), str(binary), str(output)],
                    capture_output=True, check=False,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())

    def test_missing_notice_prevents_output_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            binary = root / "gateway"
            binary.write_bytes(b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0")
            output = root / "layer.tar"
            result = subprocess.run(
                [sys.executable, str(SCRIPT), str(root), str(binary), str(output)],
                capture_output=True, check=False,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
