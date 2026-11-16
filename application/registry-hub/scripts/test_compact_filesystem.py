import fcntl
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("compact-filesystem.py")
SPEC = importlib.util.spec_from_file_location("compact_filesystem", SCRIPT)
COMPACTION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(COMPACTION)


class CompactionTests(unittest.TestCase):
    def test_compaction_rejects_root_and_relative_paths(self):
        for root in ("/", "//", "."):
            with self.subTest(root=root), self.assertRaises(ValueError):
                COMPACTION.compact(root, apply=True)

    def test_compaction_preserves_objects_symlinks_staging_and_pool(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "old" / "nested").mkdir(parents=True)
            (root / "old" / "nested" / "record.lock").touch()
            (root / "payload").write_bytes(b"data")
            (root / "nonempty.lock").write_bytes(b"data")
            (root / ".staging-abandoned").write_bytes(b"pending")
            (root / "link.lock").symlink_to(root / "payload")
            pool = root / COMPACTION.LOCK_DIRECTORY
            pool.mkdir()
            (pool / "0000.lock").touch()
            dry_run = COMPACTION.compact(directory)
            self.assertEqual(dry_run["legacyLocks"], 1)
            self.assertEqual(dry_run["emptyDirectories"], 2)
            self.assertTrue((root / "old" / "nested" / "record.lock").exists())
            applied = COMPACTION.compact(directory, apply=True)
            self.assertEqual(applied["legacyLocks"], 1)
            self.assertFalse((root / "old").exists())
            self.assertEqual((root / "payload").read_bytes(), b"data")
            self.assertEqual((root / "nonempty.lock").read_bytes(), b"data")
            self.assertEqual((root / ".staging-abandoned").read_bytes(), b"pending")
            self.assertTrue((root / "link.lock").is_symlink())
            self.assertTrue((pool / "0000.lock").exists())

    def test_compaction_refuses_an_active_writer(self):
        with tempfile.TemporaryDirectory() as directory:
            descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
                with self.assertRaises(BlockingIOError):
                    COMPACTION.compact(directory, apply=True)
            finally:
                os.close(descriptor)

    def test_apply_requires_offline_acknowledgement(self):
        with tempfile.TemporaryDirectory() as directory:
            legacy = Path(directory) / "old.lock"
            legacy.touch()
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--root", directory, "--apply"],
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 2)
            self.assertTrue(legacy.exists())


if __name__ == "__main__":
    unittest.main()
