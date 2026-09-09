# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Run with: uv run --script scripts/test_release.py.

Exercise release scripts without GitHub credentials, tags, or publishing.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import tomllib
import unittest

ROOT = Path(__file__).resolve().parents[1]


class ReleaseTests(unittest.TestCase):
    def run_bump(self, status, output):
        script = tomllib.loads((ROOT / "mise.toml").read_text())["tasks"]["release:bump"]["run"]
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)
            (path / ".cz.toml").write_text('version = "0.8.4"\n')
            git = path / "git"
            git.write_text('#!/bin/sh\ncase "$1" in\n tag) echo v0.8.4;;\n log) echo abc123;;\nesac\nexit 0\n')
            cz = path / "cz"
            cz.write_text('#!/bin/sh\nprintf "%s\\n" "$TEST_OUTPUT"\nexit "$TEST_STATUS"\n')
            uvx = path / "uvx"
            # Assert the pinned tool invocation, then run only the fake cz.
            # Tests must never download a tool or invoke the real Commitizen.
            uvx.write_text('#!/bin/sh\n[ "$1 $2 $3 $4" = "--python 3.11 --from commitizen==4.13.7" ] || exit 99\nshift 4\nexec "$@"\n')
            git.chmod(0o755)
            cz.chmod(0o755)
            uvx.chmod(0o755)
            env = dict(os.environ, PATH=f"{temp}:{os.environ['PATH']}", TEST_OUTPUT=output, TEST_STATUS=str(status))
            return subprocess.run(["sh", "-c", script], cwd=temp, env=env, capture_output=True, text=True, timeout=10)

    def test_bump_propagates_unexpected_failure(self):
        result = self.run_bump(42, "tagging failed")
        self.assertEqual(result.returncode, 42, result.stdout + result.stderr)

    def test_bump_accepts_only_known_noop(self):
        for reason in ("NO_COMMITS_FOUND", "NO_COMMITS_TO_BUMP"):
            result = self.run_bump(21, f"[{reason}]")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_bump_success(self):
        self.assertEqual(self.run_bump(0, "bumped").returncode, 0)

    def test_manual_release_requires_exact_tag_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            env = dict(os.environ, EVENT_NAME="workflow_dispatch", INPUT_VERSION="0.8.4", GITHUB_REF="refs/tags/v0.8.3", GITHUB_OUTPUT=str(Path(temp) / "output"))
            result = subprocess.run(["bash", str(ROOT / "scripts/release-meta.sh")], cwd=temp, env=env, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("must match", result.stdout)
            self.assertFalse(Path(env["GITHUB_OUTPUT"]).exists())

    def test_release_accepts_only_the_triggering_commit(self):
        for tag_sha, expected in (("workflow-commit", 0), ("moved-tag", 1)):
            with tempfile.TemporaryDirectory() as temp:
                path = Path(temp)
                git = path / "git"
                git.write_text('#!/bin/sh\ncase "$1" in\n rev-list) echo "$TEST_TAG_SHA";;\n rev-parse) echo workflow-commit;;\nesac\nexit 0\n')
                git.chmod(0o755)
                output = path / "output"
                env = dict(os.environ, PATH=f"{temp}:{os.environ['PATH']}", EVENT_NAME="workflow_dispatch", INPUT_VERSION="0.8.4", GITHUB_REF="refs/tags/v0.8.4", GITHUB_SHA="workflow-commit", GITHUB_OUTPUT=str(output), TEST_TAG_SHA=tag_sha)
                result = subprocess.run(["bash", str(ROOT / "scripts/release-meta.sh")], cwd=temp, env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                if expected == 0:
                    self.assertEqual(output.read_text(), "tag=v0.8.4\nversion=0.8.4\nsha=workflow-commit\n")
                else:
                    self.assertFalse(output.exists())

    def test_invalid_version_rejected_before_git(self):
        env = dict(os.environ, EVENT_NAME="workflow_dispatch", INPUT_VERSION="oops;command", GITHUB_REF="refs/tags/v0.8.4")
        result = subprocess.run(["bash", str(ROOT / "scripts/release-meta.sh")], env=env, capture_output=True, text=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Invalid version", result.stdout)


if __name__ == "__main__":
    unittest.main()
