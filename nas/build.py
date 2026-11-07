#!/usr/bin/env python3
"""Build the vocat image for the NAS from the current checkout.

The upstream Dockerfile is used unchanged; network tweaks (registry mirror,
GOPROXY, npm registry) are applied to a temporary copy so upstream merges stay
conflict-free.

    python nas/build.py                 # build vocat:<version> + vocat:nas-latest
    python nas/build.py --allow-dirty   # build uncommitted changes (tag gets -dirty)

Environment overrides:
    REGISTRY_MIRROR  Docker Hub mirror (default docker.m.daocloud.io; "" = Docker Hub)
    GOPROXY          Go module proxy (default https://goproxy.cn,direct; "" = upstream default)
    NPM_REGISTRY     npm registry (default: upstream default)
"""
import argparse
import datetime
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
LAST_BUILD = REPO / "nas" / ".last-build"


def git(*args):
    return subprocess.check_output(["git", "-C", str(REPO), *args], text=True).strip()


def version_info(allow_dirty):
    dirty = bool(git("status", "--porcelain", "--untracked-files=no"))
    if dirty and not allow_dirty:
        sys.exit("working tree has uncommitted changes; commit them or pass --allow-dirty")
    base = git("describe", "--tags", "--abbrev=0", "--match", "v[0-9]*")
    ahead = git("rev-list", "--count", f"{base}..HEAD")
    sha = git("rev-parse", "--short=7", "HEAD")
    release = base.lstrip("v")
    suffix = f"nas.{ahead}.{sha}" + (".dirty" if dirty else "")
    # Build metadata (+...) keeps vocat's update check comparing against the
    # upstream release; docker tags cannot contain "+".
    return f"{release}+{suffix}", f"{release}-{suffix}"


def patched_dockerfile(text, mirror, goproxy, npm_registry):
    out, stages = [], set()
    for line in text.splitlines():
        if mirror and line.startswith("# syntax=docker/"):
            line = line.replace("# syntax=", f"# syntax={mirror}/", 1)
        match = re.match(r"^(FROM\s+(?:--platform=\S+\s+)?)(\S+)(.*)$", line)
        if mirror and match:
            image = match.group(2)
            first, slash, _ = image.partition("/")
            # Only a first path component can name a registry (host.tld, host:port).
            has_registry = slash and ("." in first or ":" in first or first == "localhost")
            if not has_registry and not image.startswith("$") and image.lower() not in stages:
                if not slash:
                    image = "library/" + image
                line = f"{match.group(1)}{mirror}/{image}{match.group(3)}"
        out.append(line)
        stage = re.search(r"\bAS\s+(\S+)\s*$", line, re.I) if match else None
        if stage:
            stages.add(stage.group(1).lower())
        if match and re.search(r"\bAS\s+go-builder\b", line, re.I) and goproxy:
            out.append(f"ENV GOPROXY={goproxy}")
        if match and re.search(r"\bAS\s+web-builder\b", line, re.I) and npm_registry:
            out.append(f"RUN npm config set registry {npm_registry}")
    return "\n".join(out) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--allow-dirty", action="store_true")
    args = parser.parse_args()

    version, tag = version_info(args.allow_dirty)
    mirror = os.environ.get("REGISTRY_MIRROR", "docker.m.daocloud.io").strip().rstrip("/")
    goproxy = os.environ.get("GOPROXY", "https://goproxy.cn,direct").strip()
    npm_registry = os.environ.get("NPM_REGISTRY", "").strip()

    dockerfile = patched_dockerfile((REPO / "Dockerfile").read_text(encoding="utf-8"), mirror, goproxy, npm_registry)
    build_time = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "Dockerfile.nas"
        path.write_text(dockerfile, encoding="utf-8", newline="\n")
        cmd = [
            "docker", "build", "--platform", "linux/amd64", "-f", str(path),
            "--build-arg", f"VERSION={version}", "--build-arg", f"BUILD_TIME={build_time}",
            "-t", f"vocat:{tag}", "-t", "vocat:nas-latest", str(REPO),
        ]
        print("+", " ".join(cmd), flush=True)
        subprocess.check_call(cmd)
    LAST_BUILD.write_text(f"vocat:{tag}\n", encoding="utf-8")
    print(f"\nbuilt vocat:{tag} (version {version}); recorded in nas/.last-build")


if __name__ == "__main__":
    main()
