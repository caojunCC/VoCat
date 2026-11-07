#!/usr/bin/env python3
"""Ship a locally built vocat image to the Synology NAS and (re)deploy it.

    python nas/deploy.py                      # deploy the image from nas/.last-build
    python nas/deploy.py vocat:0.3.42-nas.2.x # deploy a specific local image
    python nas/deploy.py --rollback           # swap back to the previous container
    python nas/deploy.py --rollback --restore-db   # ...and restore the DB backup
                                                   # taken by the last deploy

Deploy = docker save | gzip -> NAS, docker load, stop `vocat`, back up the
vocat-data volume to ~/vocat-backups/<stamp>-before-<image>/, keep the old
container as `vocat-prev`, start the new `vocat` with the same flags, then wait
for VoWiFi/IMS registration.

Connection settings come from NAS_HOST / NAS_PORT / NAS_USER (defaults below)
and NAS_PASSWORD (prompted when unset). The password is never stored.
Requires: paramiko (pip install paramiko).
"""
import argparse
import getpass
import os
import subprocess
import sys
import zlib
from pathlib import Path

import paramiko

REPO = Path(__file__).resolve().parent.parent
HOST = os.environ.get("NAS_HOST", "192.168.2.245")
PORT = int(os.environ.get("NAS_PORT", "8022"))
USER = os.environ.get("NAS_USER", "caojun176")
REMOTE_DIR = "vocat-deploy"  # relative to the NAS user's home (on /volume1)

COMMON = r"""
set -eu
D=/usr/local/bin/docker
DATA=/volume1/@docker/volumes/vocat-data/_data
BK_ROOT="$HOME_DIR/vocat-backups"
wait_ims() {
  start=$1; i=0
  while [ $i -lt 36 ]; do
    sleep 5; i=$((i+1))
    row=$(/usr/bin/sqlite3 -readonly -separator ' ' "$DATA/vocat.db" \
      "select ims_ready, updated_at from vowifi_runtime limit 1" 2>/dev/null || true)
    ready=${row%% *}; updated=${row##* }
    if [ "${ready:-0}" = 1 ] && [ "${updated:-0}" -gt "$start" ] 2>/dev/null; then
      echo "IMS registered after $((i*5))s"; return 0
    fi
  done
  echo "WARNING: IMS not registered within 180s; recent logs:"
  $D logs --since 4m vocat 2>&1 | grep -v '"http request"' | tail -15 || true
  return 0
}
status() {
  $D ps -a --filter name='^/vocat' --format '{{.Names}}\t{{.Image}}\t{{.Status}}'
  echo "web: $(curl -s -o /dev/null -m 5 -w '%{http_code}' http://127.0.0.1:7575/ || true)"
}
"""

DEPLOY = COMMON + r"""
IMAGE="$1"
$D image inspect "$IMAGE" >/dev/null
if $D inspect vocat >/dev/null 2>&1; then
  OLD=$($D inspect -f '{{.Config.Image}}' vocat)
  BK="$BK_ROOT/$(date +%Y%m%d-%H%M%S)-before-$(echo "$IMAGE" | tr ':/' '__')"
  mkdir -p "$BK/data"
  $D inspect vocat > "$BK/vocat-container-inspect.json"
  echo "stopping vocat ($OLD)"
  $D stop -t 30 vocat >/dev/null
  cp -a "$DATA/." "$BK/data/"
  echo "DB backup: $BK"
  if $D inspect vocat-prev >/dev/null 2>&1; then $D rm -f vocat-prev >/dev/null; fi
  $D rename vocat vocat-prev
fi
start=$(date +%s)
$D run -d --name vocat --restart unless-stopped --network host --privileged \
  --security-opt label=disable --user root \
  -e VOCAT_CONTAINER=docker -e VOCAT_ADDR=0.0.0.0:7575 \
  -e VOCAT_DATABASE_PATH=/opt/vocat/data/vocat.db \
  -v vocat-data:/opt/vocat/data:z -v /dev:/dev -v /sys:/sys:ro "$IMAGE" >/dev/null
echo "started vocat ($IMAGE); previous container kept as vocat-prev"
wait_ims "$start"
status
"""

ROLLBACK = COMMON + r"""
RESTORE="$1"
$D inspect vocat-prev >/dev/null 2>&1 || { echo "no vocat-prev container to roll back to"; exit 1; }
$D stop -t 30 vocat >/dev/null 2>&1 || true
if [ "$RESTORE" = 1 ]; then
  BK=$(ls -1d "$BK_ROOT"/*-before-* 2>/dev/null | tail -1)
  [ -n "$BK" ] && [ -f "$BK/data/vocat.db" ] || { echo "no DB backup found"; exit 1; }
  rm -f "$DATA/vocat.db-wal" "$DATA/vocat.db-shm"
  cp -a "$BK/data/." "$DATA/"
  echo "restored DB from $BK"
fi
$D rm -f vocat >/dev/null 2>&1 || true
$D rename vocat-prev vocat
start=$(date +%s)
$D start vocat >/dev/null
echo "rolled back to $($D inspect -f '{{.Config.Image}}' vocat)"
wait_ims "$start"
status
"""


class NAS:
    def __init__(self, password):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(HOST, port=PORT, username=USER, password=password, timeout=15)
        self.home = self.run("printf %s \"$HOME\"", capture=True).strip()

    def run(self, command, stdin=b"", capture=False, sudo=False):
        if sudo:
            # -k: always read the password, so it can never fall through to the
            # command's own stdin when sudo has cached credentials.
            command = "sudo -k -S -p '' " + command
            stdin = (self.password + "\n").encode() + stdin
        channel = self.client.get_transport().open_session()
        channel.set_combine_stderr(not capture)
        channel.exec_command(command)
        if stdin:
            channel.sendall(stdin)
        channel.shutdown_write()
        output = bytearray()
        while True:
            chunk = channel.recv(65536)
            if not chunk:
                break
            if capture:
                output += chunk
            else:
                sys.stdout.write(chunk.decode("utf-8", "replace"))
                sys.stdout.flush()
        error = channel.recv_stderr(1 << 20).decode("utf-8", "replace").strip()
        code = channel.recv_exit_status()
        if code != 0:
            sys.exit(f"remote command failed ({code}): {command[:80]}\n{error}")
        return output.decode("utf-8", "replace")

    def sudo_script(self, script, *args):
        quoted = " ".join("'" + a.replace("'", "'\\''") + "'" for a in args)
        self.run(f"env HOME_DIR='{self.home}' sh -s -- {quoted}", stdin=script.encode(), sudo=True)

    def ship(self, image):
        remote = f"{self.home}/{REMOTE_DIR}/image.tar.gz"
        self.run(f"mkdir -p '{self.home}/{REMOTE_DIR}'")
        print(f"shipping {image} -> {HOST}:{remote}", flush=True)
        save = subprocess.Popen(["docker", "save", image], stdout=subprocess.PIPE)
        channel = self.client.get_transport().open_session()
        channel.exec_command(f"cat > '{remote}'")
        gz = zlib.compressobj(6, zlib.DEFLATED, 31)
        sent = 0
        while True:
            chunk = save.stdout.read(1 << 20)
            if not chunk:
                break
            data = gz.compress(chunk)
            channel.sendall(data)
            sent += len(data)
        channel.sendall(gz.flush())
        channel.shutdown_write()
        if channel.recv_exit_status() != 0 or save.wait() != 0:
            sys.exit("image transfer failed")
        print(f"sent {sent / 1e6:.1f} MB; loading on NAS", flush=True)
        self.run(f"/usr/local/bin/docker load -i '{remote}'", sudo=True)
        self.run(f"rm -f '{remote}'")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("image", nargs="?", help="local image (default: nas/.last-build)")
    parser.add_argument("--rollback", action="store_true", help="swap back to vocat-prev")
    parser.add_argument("--restore-db", action="store_true", help="with --rollback: restore the last DB backup")
    parser.add_argument("--no-ship", action="store_true", help="image is already loaded on the NAS")
    args = parser.parse_args()

    password = os.environ.get("NAS_PASSWORD") or getpass.getpass(f"{USER}@{HOST} password: ")
    nas = NAS(password)
    if args.rollback:
        nas.sudo_script(ROLLBACK, "1" if args.restore_db else "0")
        return
    image = args.image
    if not image:
        last = REPO / "nas" / ".last-build"
        if not last.exists():
            sys.exit("no image given and nas/.last-build missing; run nas/build.py first")
        image = last.read_text(encoding="utf-8").strip()
    if not args.no_ship:
        nas.ship(image)
    nas.sudo_script(DEPLOY, image)


if __name__ == "__main__":
    main()
