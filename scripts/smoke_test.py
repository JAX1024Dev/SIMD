#!/usr/bin/env python3
"""Exercise a built daemon with an absent serial port and an isolated database."""
import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="bin/simd")
    binary = str(Path(parser.parse_args().binary).resolve())
    version = subprocess.check_output([binary, "-version"], text=True).strip()
    assert version.startswith("simd "), version
    with tempfile.TemporaryDirectory(prefix="simd-smoke-") as directory:
        root = Path(directory)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        config = root / "config.yaml"
        config.write_text(f"listen: 127.0.0.1:{port}\ndatabase: {root}/data/simd.db\ndevices:\n  - id: unplugged\n    port: {root}/absent-serial\n")
        token = secrets.token_hex(24)
        env = dict(os.environ, SIMD_API_TOKEN=token)
        subprocess.run([binary, "-config", str(config), "-check-config"], env=env, check=True, stdout=subprocess.DEVNULL)
        with (root / "daemon.log").open("w") as log:
            process = subprocess.Popen([binary, "-config", str(config)], env=env, stdout=log, stderr=log)

            def request(path, authenticated=True, body=None):
                headers = {"Authorization": "Bearer " + token} if authenticated else {}
                if body is not None:
                    headers.update({"Content-Type": "application/json", "Idempotency-Key": "smoke"})
                req = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers=headers, data=body)
                try:
                    response = urllib.request.urlopen(req, timeout=2)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    return response.status, json.load(response)

            try:
                for _ in range(100):
                    if process.poll() is not None:
                        raise RuntimeError("daemon exited before HTTP startup")
                    try:
                        if request("/health/live", False)[0] == 200:
                            break
                    except OSError:
                        pass
                    time.sleep(0.05)
                assert request("/health/live", False) == (200, {"live": True})
                assert request("/health/ready", False) == (503, {"ready": False})
                assert request("/api/v1/messages", False)[0] == 401
                assert request("/api/v1/messages")[1]["items"] == []
                assert request("/api/v1/devices")[1]["items"][0]["id"] == "unplugged"
                assert request("/api/v1/messages", body=b'{"device_id":"unplugged","number":"10086","body":"test"}')[0] == 503
                assert request("/api/v1/unknown")[0] == 404
            finally:
                if process.poll() is None:
                    process.send_signal(signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise
            assert process.returncode == 0, process.returncode
    print("PASS: " + version)
    print("PASS: health, authentication, queries, offline send, errors, graceful shutdown; no modem or Bark access")


if __name__ == "__main__":
    main()
