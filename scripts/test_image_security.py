# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
import copy
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from image_security import validate_report, validate_sbom


class ImageSecurityTest(unittest.TestCase):
    def setUp(self):
        self.sbom = {"distro": {"id": "wolfi"}, "artifacts": [
            {"name": name, "type": "apk", "version": "1-r1", "locations": [{"path": "/lib/apk/db/installed"}]}
            for name in ("bash", "zsh", "curl", "openssl", "jq", "gcc", "make", "pkgconf", "openssl-dev", "zlib-dev", "ggshield")
        ]}

    def test_complete_inventory(self):
        validate_sbom(self.sbom)

    def test_wrong_distro(self):
        self.sbom["distro"]["id"] = "debian"
        with self.assertRaises(ValueError):
            validate_sbom(self.sbom)

    def test_missing_inventory(self):
        for replacement in ([], *[
                [p for p in self.sbom["artifacts"] if p["name"] != missing["name"]]
                for missing in self.sbom["artifacts"]]):
            with self.subTest(artifacts=replacement), self.assertRaises(ValueError):
                validate_sbom({**self.sbom, "artifacts": replacement})

    def test_missing_package_identity(self):
        for key in ("type", "version", "locations"):
            sbom = copy.deepcopy(self.sbom)
            del sbom["artifacts"][0][key]
            with self.subTest(key=key), self.assertRaises(ValueError):
                validate_sbom(sbom)

    def test_report_keeps_lower_severity_visible(self):
        validate_report({"matches": [{"vulnerability": {"severity": "Medium"}}], "ignoredMatches": []})

    def test_report_rejects_security_failures(self):
        for severity in ("High", "Critical"):
            with self.subTest(severity=severity), self.assertRaises(ValueError):
                validate_report({"matches": [{"vulnerability": {"severity": severity}}]})

    def test_report_rejects_suppression_or_missing_matches(self):
        for report in ({}, {"matches": None}, {"matches": [], "ignoredMatches": [{}]}):
            with self.subTest(report=report), self.assertRaises(ValueError):
                validate_report(report)


class PublishImageTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "bin").mkdir()
        (self.root / "scripts").mkdir()
        self.script = Path(__file__).resolve().parent / "publish-image-archive.sh"
        self.digest = "sha256:" + hashlib.sha256(b'{"schemaVersion":2}\n').hexdigest()
        self.env = {
            "PATH": str(self.root / "bin") + os.pathsep + os.environ["PATH"],
            "HOME": str(self.root), "IMAGE_TAGS": "ghcr.io/clement-tourriere/debux:0.9.0",
        }
        self.executable("bin/skopeo", '''#!/bin/sh
if [ "$1" = inspect ]; then printf '%s\\n' '{"schemaVersion":2}'; exit 0; fi
printf '%s\\n' "$*" >> "$HOME/pushes"
exit "${COPY_STATUS:-0}"
''')
        self.executable("scripts/validate-image-archive.sh", '''#!/bin/sh
echo validated > "$HOME/validated"
exit "${VALIDATION_STATUS:-0}"
''')

    def executable(self, name, content):
        path = self.root / name
        path.write_text(content)
        path.chmod(0o755)

    def run_publish(self, digest=None):
        return subprocess.run(["bash", str(self.script), "toolbox.tar", digest or self.digest],
                              cwd=self.root, env=self.env, capture_output=True, text=True)

    def test_copies_validated_bytes_with_attestations(self):
        result = self.run_publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / "validated").exists())
        self.assertIn("copy --all --preserve-digests", (self.root / "pushes").read_text())

    def test_failed_validation_never_publishes(self):
        self.env["VALIDATION_STATUS"] = "42"
        self.assertEqual(self.run_publish().returncode, 42)
        self.assertFalse((self.root / "pushes").exists())

    def test_digest_mismatch_never_publishes(self):
        self.assertNotEqual(self.run_publish("sha256:" + "0" * 64).returncode, 0)
        self.assertFalse((self.root / "pushes").exists())

    def test_unexpected_tags_never_publish(self):
        self.env["IMAGE_TAGS"] += "\nghcr.io/other/project:latest"
        self.assertNotEqual(self.run_publish().returncode, 0)
        self.assertFalse((self.root / "pushes").exists())

    def test_copy_failure_propagates(self):
        self.env["COPY_STATUS"] = "57"
        self.assertEqual(self.run_publish().returncode, 57)


if __name__ == "__main__":
    unittest.main()
