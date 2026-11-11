"""Exercise artifact installation without touching the user's home."""
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent

class InstallTests(unittest.TestCase):
    def test_update_and_uninstall_preserve_unowned_data(self):
        with tempfile.TemporaryDirectory(prefix="loom install ") as tmp:
            prefix = pathlib.Path(tmp)
            command = ["python3", str(ROOT / "tools/install-linux.py"), "--prefix", tmp,
                       "--data-dir", str(prefix / "share")]
            sentinel = prefix / "history.db"
            sentinel.write_text("user data")
            subprocess.run(command, check=True, capture_output=True)
            subprocess.run(command, check=True, capture_output=True)
            desktop = prefix / "share/applications/loom.desktop"
            subprocess.run(["desktop-file-validate", str(desktop)], check=True)
            subprocess.run(command + ["--uninstall"], check=True, capture_output=True)
            self.assertFalse((prefix / "bin/loom-gui").exists())
            self.assertEqual(sentinel.read_text(), "user data")

    def test_uninstall_refuses_modified_file(self):
        with tempfile.TemporaryDirectory(prefix="loom install ") as tmp:
            prefix = pathlib.Path(tmp)
            command = ["python3", str(ROOT / "tools/install-linux.py"), "--prefix", tmp,
                       "--data-dir", str(prefix / "share")]
            subprocess.run(command, check=True, capture_output=True)
            desktop = prefix / "share/applications/loom.desktop"
            desktop.write_text("user replacement")
            result = subprocess.run(command + ["--uninstall"], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(desktop.read_text(), "user replacement")
            self.assertTrue((prefix / "bin/loom-gui").exists())

if __name__ == "__main__":
    unittest.main()
