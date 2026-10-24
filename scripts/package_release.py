#!/usr/bin/env python3
"""Build static Linux binaries and deterministic release archives (stdlib only)."""
import argparse
import datetime
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parent.parent
FILES = [
    "README.md", "CHANGELOG.md", "configs/simd.example.yaml",
    "configs/simd.orangepi-zero3.yaml", "deploy/simd.service",
    "deploy/simd.env.example", "docs/orangepi-zero3.md", "docs/openapi.yaml",
    "docs/requirements.md", "docs/validation.md",
]


def git(*args):
    result = subprocess.run(["git", *args], cwd=ROOT, text=True, capture_output=True)
    return result.stdout.strip() if result.returncode == 0 else ""


def archive(path, prefix, binary, version, epoch):
    entries = [("simd", binary.read_bytes(), 0o755)]
    entries.extend((name, (ROOT / name).read_bytes(), 0o644) for name in FILES)
    entries.append(("VERSION", (version + "\n").encode(), 0o644))
    with path.open("wb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as tar:
            for name, data, mode in sorted(entries):
                info = tarfile.TarInfo(prefix + "/" + name)
                info.size, info.mode, info.mtime = len(data), mode, epoch
                info.uid = info.gid = 0
                tar.addfile(info, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--version", default="dev")
    parser.add_argument("--arch", nargs="+", choices=["amd64", "arm64"], default=["amd64", "arm64"])
    parser.add_argument("--binaries-only", action="store_true")
    args = parser.parse_args()
    if args.version != "dev" and not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?", args.version):
        parser.error("version must be dev or a v-prefixed semantic version")
    commit = git("rev-parse", "--short=12", "HEAD") or "unknown"
    if args.version != "dev" and (commit == "unknown" or git("status", "--porcelain")):
        parser.error("release builds require a committed, clean Git checkout")
    epoch = int(os.environ.get("SOURCE_DATE_EPOCH") or git("show", "-s", "--format=%ct", "HEAD") or "0")
    date = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ldflags = " ".join(["-s -w", "-X esim/internal/buildinfo.Version=" + args.version,
                        "-X esim/internal/buildinfo.Commit=" + commit, "-X esim/internal/buildinfo.Date=" + date])
    (ROOT / "bin").mkdir(exist_ok=True)
    artifacts = []
    if not args.binaries_only:
        (ROOT / "dist").mkdir(exist_ok=True)
    for arch in dict.fromkeys(args.arch):
        binary = ROOT / "bin" / ("simd-linux-" + arch)
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch, GOARM64="v8.0", GOAMD64="v1")
        subprocess.run([args.go, "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags,
                        "-o", str(binary), "./cmd/simd"], cwd=ROOT, env=env, check=True)
        if not args.binaries_only:
            prefix = "simd_" + args.version + "_linux_" + arch
            path = ROOT / "dist" / (prefix + ".tar.gz")
            archive(path, prefix, binary, args.version, epoch)
            artifacts.append(path)
    if artifacts:
        checksums = "".join(hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name + "\n" for path in artifacts)
        (ROOT / "dist" / "SHA256SUMS").write_text(checksums)
        print(checksums, end="")


if __name__ == "__main__":
    main()
