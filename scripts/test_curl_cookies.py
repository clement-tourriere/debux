# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Loopback-only regression for CVE-2026-82209, using the baked curl/libcurl.

Based on upstream curl test 2318:
https://github.com/curl/curl/commit/95c1e8915dce64606bd753fd47fc0bd236e31cd6
"""
import http.server
import subprocess
import threading


def check_cookie_isolation():
    requests = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            requests.append((self.headers.get("Host"), self.headers.get("Cookie")))
            self.send_response(200)
            self.send_header("Content-Length", "0")
            if len(requests) == 1:
                self.send_header("Set-Cookie", "sid=DOMAIN_SECRET; Domain=github.io; Path=/")
            self.end_headers()

        def log_message(self, *args):
            pass

    curl = "/usr/bin/curl"
    version = subprocess.check_output([curl, "--disable", "--version"], text=True, timeout=10)
    if "PSL" not in version.split():
        raise RuntimeError("baked curl must have Public Suffix List support")
    server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    port = server.server_port
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        subprocess.run([
            curl, "--disable", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "10",
            "--resolve", f"github.io:{port}:127.0.0.1",
            "--resolve", f"attacker.github.io:{port}:127.0.0.1",
            "--cookie", "",
            f"http://github.io:{port}/", f"http://attacker.github.io:{port}/", f"http://github.io:{port}/",
        ], check=True, timeout=40)
    finally:
        server.shutdown()
        thread.join()
        server.server_close()
    expected = [(f"github.io:{port}", None), (f"attacker.github.io:{port}", None),
                (f"github.io:{port}", "sid=DOMAIN_SECRET")]
    if requests != expected:
        raise RuntimeError(f"CVE-2026-82209 regression: cookie scope is not host-only: {requests!r}")
    print("curl PSL cookie isolation passed (CVE-2026-82209)")


if __name__ == "__main__":
    check_cookie_isolation()
