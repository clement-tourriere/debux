# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Reject incomplete toolbox inventories and suppressed/failed scan results."""
import json
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


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("sbom", "report"):
        sys.exit("Usage: image_security.py sbom|report FILE")
    try:
        with open(sys.argv[2], encoding="utf-8") as stream:
            data = json.load(stream)
        (validate_sbom if sys.argv[1] == "sbom" else validate_report)(data)
    except (OSError, ValueError, TypeError, AttributeError) as error:
        sys.exit(str(error))
