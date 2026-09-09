# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

from image_security import validate_report, validate_sbom, validation_receipt


def sample_sbom():
    return {"distro": {"id": "wolfi"}, "artifacts": [
        {"name": name, "type": "apk", "version": "1-r1", "locations": [{"path": "/lib/apk/db/installed"}]}
        for name in ("bash", "zsh", "curl", "openssl", "jq", "gcc", "make", "pkgconf", "openssl-dev", "zlib-dev", "ggshield")
    ]}


class ImageSecurityTest(unittest.TestCase):
    def setUp(self):
        self.sbom = sample_sbom()

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
            "UV_PYTHON": sys.executable,
        }
        self.write_reports()
        self.executable("bin/skopeo", '''#!/bin/sh
if [ "$1" = inspect ]; then printf '%s\\n' '{"schemaVersion":2}'; exit 0; fi
printf '%s\\n' "$*" >> "$HOME/pushes"
exit "${COPY_STATUS:-0}"
''')
        self.executable("scripts/validate-image-archive.sh", '''#!/bin/sh
echo validated > "$HOME/validated"
exit "${VALIDATION_STATUS:-0}"
''')

    def write_reports(self):
        for arch in ("amd64", "arm64"):
            directory = self.root / "dist/security" / f"publish-{arch}"
            directory.mkdir(parents=True, exist_ok=True)
            (directory / "sbom.json").write_text(json.dumps(sample_sbom()))
            (directory / "grype.json").write_text(json.dumps({"matches": [], "ignoredMatches": []}))
            (directory / "validated.json").write_text(json.dumps(validation_receipt(directory, self.digest, arch)))

    def executable(self, name, content):
        path = self.root / name
        path.write_text(content)
        path.chmod(0o755)

    def run_publish(self, digest=None, receipts=False):
        args = ["bash", str(self.script), "toolbox.tar", digest or self.digest]
        if receipts:
            args.append(str(self.root / "dist/security"))
        return subprocess.run(args, cwd=self.root, env=self.env, capture_output=True, text=True)

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

    def test_immutable_native_results_avoid_repeating_emulated_builds(self):
        result = self.run_publish(receipts=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / "validated").exists())
        self.assertIn("copy --all --preserve-digests", (self.root / "pushes").read_text())

    def test_missing_or_tampered_native_results_never_publish(self):
        for arch in ("amd64", "arm64"):
            for fault in ("missing", "digest", "arch", "smoke", "schema", "sbom", "report"):
                with self.subTest(arch=arch, fault=fault):
                    self.write_reports()
                    directory = self.root / "dist/security" / f"publish-{arch}"
                    receipt = directory / "validated.json"
                    if fault == "missing":
                        receipt.unlink()
                    elif fault in ("sbom", "report"):
                        name = "sbom.json" if fault == "sbom" else "grype.json"
                        with (directory / name).open("a") as stream:
                            stream.write("\n")
                    else:
                        data = json.loads(receipt.read_text())
                        data[fault] = "wrong"
                        receipt.write_text(json.dumps(data))
                    self.assertNotEqual(self.run_publish(receipts=True).returncode, 0)
                    self.assertFalse((self.root / "pushes").exists())

    def run_native_validation(self, arch):
        self.executable("scripts/scan-image.sh", '#!/bin/sh\nexit "${SCAN_STATUS:-0}"\n')
        self.executable("scripts/test-image.sh", '#!/bin/sh\nexit "${SMOKE_STATUS:-0}"\n')
        shutil.copyfile(self.script.with_name("image_security.py"), self.root / "scripts/image_security.py")
        return subprocess.run(["bash", str(self.script.with_name("validate-image-archive.sh")),
                               "toolbox.tar", self.digest, arch],
                              cwd=self.root, env=self.env, capture_output=True, text=True)

    def test_native_success_records_only_the_tested_architecture(self):
        for arch in ("amd64", "arm64"):
            (self.root / "dist/security" / f"publish-{arch}/validated.json").unlink()
        result = self.run_native_validation("arm64")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / "dist/security/publish-arm64/validated.json").exists())
        self.assertFalse((self.root / "dist/security/publish-amd64/validated.json").exists())
        self.assertNotEqual(self.run_publish(receipts=True).returncode, 0)
        self.assertNotIn("docker://ghcr.io/", (self.root / "pushes").read_text())

    def test_failed_native_scan_or_smoke_removes_old_success(self):
        for stage in ("SCAN_STATUS", "SMOKE_STATUS"):
            with self.subTest(stage=stage):
                self.write_reports()
                self.env[stage] = "42"
                result = self.run_native_validation("arm64")
                del self.env[stage]
                self.assertEqual(result.returncode, 42, result.stderr)
                self.assertFalse((self.root / "dist/security/publish-arm64/validated.json").exists())
                self.assertNotEqual(self.run_publish(receipts=True).returncode, 0)
                if (self.root / "pushes").exists():
                    self.assertNotIn("docker://ghcr.io/", (self.root / "pushes").read_text())


if __name__ == "__main__":
    unittest.main()
