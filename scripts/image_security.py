# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Reject incomplete toolbox inventories and suppressed/failed scan results."""
import hashlib
import json
from pathlib import Path
import re
import sys


def validate_sbom(sbom):
    if sbom.get("distro", {}).get("id") != "wolfi":
        raise ValueError("expected a Wolfi toolbox inventory")
    artifacts = sbom.get("artifacts", [])
    installed = {p.get("name") for p in artifacts if p.get("type") == "apk"}
    required = {"bash", "zsh", "curl", "openssl", "jq", "gcc", "make", "pkgconf", "openssl-dev", "zlib-dev", "ggshield"}
    if not required.issubset(installed):
        raise ValueError("incomplete APK inventory; refusing a partial toolbox scan")
    if not all(p.get("version") and p.get("locations") for p in artifacts if p.get("type") == "apk"):
        raise ValueError("APK inventory lacks versions or locations")


def validate_report(report):
    if not isinstance(report.get("matches"), list):
        raise ValueError("missing vulnerability matches")
    if report.get("ignoredMatches"):
        raise ValueError("suppressed findings are not allowed in the toolbox gate")
    if any(m.get("vulnerability", {}).get("severity", "").lower() in ("high", "critical")
           for m in report["matches"]):
        raise ValueError("unresolved high/critical vulnerabilities")


def validation_receipt(directory, digest, arch):
    """Bind reports to the entire OCI index, not a mutable tag or similar build."""
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest) or arch not in ("amd64", "arm64"):
        raise ValueError("invalid validation identity")
    directory = Path(directory)
    sbom = (directory / "sbom.json").read_bytes()
    report = (directory / "grype.json").read_bytes()
    validate_sbom(json.loads(sbom))
    validate_report(json.loads(report))
    return {"schema": 1, "digest": digest, "arch": arch, "smoke": "passed",
            "sbom_sha256": hashlib.sha256(sbom).hexdigest(),
            "report_sha256": hashlib.sha256(report).hexdigest()}


def validate_receipts(directory, digest):
    for arch in ("amd64", "arm64"):
        report_dir = Path(directory) / f"publish-{arch}"
        receipt = json.loads((report_dir / "validated.json").read_text(encoding="utf-8"))
        if receipt != validation_receipt(report_dir, digest, arch):
            raise ValueError(f"{arch} validation receipt does not match archive/reports")


if __name__ == "__main__":
    try:
        args = sys.argv[1:]
        if len(args) == 2 and args[0] in ("sbom", "report"):
            data = json.loads(Path(args[1]).read_text(encoding="utf-8"))
            (validate_sbom if args[0] == "sbom" else validate_report)(data)
        elif len(args) == 4 and args[0] == "record":
            # Called by validate-image-archive.sh ONLY after the smoke succeeds.
            receipt = validation_receipt(args[1], args[2], args[3])
            (Path(args[1]) / "validated.json").write_text(json.dumps(receipt) + "\n", encoding="utf-8")
        elif len(args) == 3 and args[0] == "receipts":
            validate_receipts(args[1], args[2])
        else:
            sys.exit("Usage: image_security.py sbom|report FILE | record DIR DIGEST ARCH | receipts DIR DIGEST")
    except (OSError, ValueError, TypeError, AttributeError) as error:
        sys.exit(str(error))
