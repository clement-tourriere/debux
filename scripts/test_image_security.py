# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
import copy
from datetime import date
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


def reviewed_curl_report(arch="x86_64"):
    return {"distro": {"name": "wolfi", "version": "20230201"}, "ignoredMatches": [], "matches": [{
        "vulnerability": {"id": "CVE-2026-82209", "severity": "High", "namespace": "nvd:cpe"},
        "artifact": {"name": "curl", "type": "apk", "version": "8.22.0-r2",
                     "purl": f"pkg:apk/wolfi/curl@8.22.0-r2?arch={arch}&distro=wolfi-20230201"},
        "matchDetails": [{"type": "cpe-match", "matcher": "apk-matcher"}],
    }]}


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


    def test_reviewed_curl_finding_remains_in_raw_report(self):
        for arch in ("aarch64", "x86_64"):
            with self.subTest(arch=arch):
                report = reviewed_curl_report(arch)
                original = copy.deepcopy(report)
                validate_report(report, today=date(2026, 9, 10))
                self.assertEqual(report, original)

    def test_curl_review_has_a_fixed_expiry(self):
        report = reviewed_curl_report()
        validate_report(report, today=date(2026, 10, 9))
        for day in (date(2026, 9, 9), date(2026, 10, 10), date(2027, 1, 1)):
            with self.subTest(day=day), self.assertRaises(ValueError):
                validate_report(report, today=day)

    def test_curl_review_does_not_apply_to_other_findings(self):
        for section, changes in {
            "vulnerability": {"id": "CVE-OTHER", "severity": "Critical", "namespace": "wolfi:distro"},
            "artifact": {"name": "libcurl", "type": "binary", "version": "8.21.0-r0", "purl": "pkg:apk/alpine/curl@8.22.0-r2"},
        }.items():
            for key, value in changes.items():
                with self.subTest(section=section, key=key):
                    report = reviewed_curl_report()
                    report["matches"][0][section][key] = value
                    with self.assertRaises(ValueError):
                        validate_report(report, today=date(2026, 9, 10))
        for version in ("8.22.0-r0", "8.22.0-r3", "8.23.0-r0", ""):
            report = reviewed_curl_report()
            report["matches"][0]["artifact"]["version"] = version
            with self.subTest(version=version), self.assertRaises(ValueError):
                validate_report(report, today=date(2026, 9, 10))
        for distro in ({}, {"name": "alpine", "version": "20230201"}, {"name": "wolfi", "version": "unknown"}):
            report = reviewed_curl_report()
            report["distro"] = distro
            with self.subTest(distro=distro), self.assertRaises(ValueError):
                validate_report(report, today=date(2026, 9, 10))
        for details in ([], [{"type": "exact-direct-match", "matcher": "apk-matcher"}],
                        [{"type": "cpe-match", "matcher": "stock-matcher"}]):
            report = reviewed_curl_report()
            report["matches"][0]["matchDetails"] = details
            with self.subTest(details=details), self.assertRaises(ValueError):
                validate_report(report, today=date(2026, 9, 10))
        with self.assertRaises(ValueError):
            validate_report(reviewed_curl_report("armv7"), today=date(2026, 9, 10))

    def test_curl_review_cannot_hide_another_high_or_suppressed_finding(self):
        for severity in ("High", "Critical"):
            report = reviewed_curl_report()
            report["matches"].append({"vulnerability": {"id": "CVE-OTHER", "severity": severity}})
            with self.subTest(severity=severity), self.assertRaises(ValueError):
                validate_report(report, today=date(2026, 9, 10))
        report = reviewed_curl_report()
        report["ignoredMatches"] = [copy.deepcopy(report["matches"][0])]
        with self.assertRaises(ValueError):
            validate_report(report, today=date(2026, 9, 10))


class ScanImageTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "bin").mkdir()
        (self.root / "sbom-source.json").write_text(json.dumps(sample_sbom()))
        (self.root / "grype-source.json").write_text(json.dumps({"matches": [], "ignoredMatches": []}))
        self.env = {"PATH": str(self.root / "bin") + os.pathsep + os.environ["PATH"],
                    "HOME": str(self.root), "TEST_PYTHON": sys.executable,
                    "DEBUX_SECURITY_REPORT_DIR": str(self.root / "reports")}
        for name, body in {
            "go": '''#!/bin/sh
case "$*" in
  *syft*) cat "$HOME/sbom-source.json"; exit "${SYFT_STATUS:-0}";;
  *grype*) cat "$HOME/grype-source.json"; exit "${GRYPE_STATUS:-0}";;
  *) exit 99;;
esac
''',
            "uv": '#!/bin/sh\nshift 2\nexec "$TEST_PYTHON" "$@"\n',
        }.items():
            path = self.root / "bin" / name
            path.write_text(body)
            path.chmod(0o755)

    def run_scan(self):
        script = Path(__file__).resolve().with_name("scan-image.sh")
        return subprocess.run(["bash", str(script), "docker:fixture"], env=self.env,
                              capture_output=True, text=True)

    def test_clean_report_passes(self):
        result = self.run_scan()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_high_finding_blocks_and_preserves_report(self):
        report = {"matches": [{"vulnerability": {"id": "CVE-OTHER", "severity": "High"}}]}
        (self.root / "grype-source.json").write_text(json.dumps(report))
        self.assertNotEqual(self.run_scan().returncode, 0)
        self.assertEqual(json.loads((self.root / "reports/grype.json").read_text()), report)

    def test_scanner_error_cannot_pass_with_a_clean_report(self):
        for stage in ("SYFT_STATUS", "GRYPE_STATUS"):
            with self.subTest(stage=stage):
                self.env[stage] = "42"
                self.assertNotEqual(self.run_scan().returncode, 0)
                del self.env[stage]


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
