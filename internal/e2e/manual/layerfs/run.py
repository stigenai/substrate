# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The LayerFS actor. It serves /healthz for the wakeup probe. GET /run starts
# the LayerFS test once in the background. GET /result returns its output as
# text/plain: status 202 while the test runs, 200 when it is done.
# The test seals a store from a sample tree (ax_probe init), mounts it twice
# (the first mount installs the store, the second is warm), reads files and
# writes in the overlay. It runs two times: with the work dir on tmpfs, and on
# the container rootfs. Lines that start with RESULT or AX_PROBE carry the
# results. DIAG lines show the processes when a step does not exit in time.
import http.server
import subprocess
import threading

LOG = "/tmp/layerfs-run.log"
lock = threading.Lock()
proc = None

TEST = r'''
set -u
export PATH=/nix/var/nix/profiles/default/bin:/usr/local/bin:/usr/bin:/bin
BIN=/opt/layerfs/bin STEP_TIMEOUT=${STEP_TIMEOUT:-90}
echo "RESULT dev_fuse=$( (stat -c '%F_%t:%T' /dev/fuse 2>/dev/null || echo missing) | tr ' ' _)"
CMD='test "$(cat README)" = hello-layerfs && echo "RESULT read_files=$(find . -type f | wc -l)" && echo overlay-write > new.txt && test "$(cat new.txt)" = overlay-write && echo "RESULT overlay_write=ok"'

# diag prints each process with its state, wait channel, kernel stack and
# open files, and the FUSE mounts. The image has no ps.
diag() {
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" = "$$" ] && continue
    args=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null) || continue
    echo "DIAG pid=$p state=$(awk '/^State:/{print $2}' "$d/status" 2>/dev/null) wchan=$(cat "$d/wchan" 2>/dev/null) args=$args"
    case "$args" in *ax_probe*|*layerfs-daemon*|*find*|*cat*)
      for t in "$d"/task/*; do
        echo "DIAG   task=${t##*/} comm=$(cat "$t/comm" 2>/dev/null) wchan=$(cat "$t/wchan" 2>/dev/null)"
        sed 's/^/DIAG     /' "$t/stack" 2>/dev/null | head -12
      done
      ls -l "$d/fd" 2>/dev/null | sed 's/^/DIAG   fd /' | head -20
    esac
  done
  grep fuse /proc/mounts | sed 's/^/DIAG mount /'
}

# kill_tree kills a process and all of its descendants.
kill_tree() {
  local c
  for c in $(grep -l "^PPid:[[:space:]]*$1\$" /proc/[0-9]*/status 2>/dev/null); do
    c=${c#/proc/}; kill_tree "${c%/status}"
  done
  kill -9 "$1" 2>/dev/null
}

# step runs a command with a time limit. AX_PROBE and RESULT lines pass
# through, with the prefix $P on each key. If the limit is hit, it prints diag first.
step() {
  local name=$1; shift
  local out="$W/$name.out" pid rc i
  "$@" > "$out" 2>&1 &
  pid=$!
  for i in $(seq "$STEP_TIMEOUT"); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  if kill -0 "$pid" 2>/dev/null; then
    echo "DIAG ${P}${name} still running after ${STEP_TIMEOUT}s"
    diag
    kill_tree "$pid"
    wait "$pid"; rc=124
  else
    wait "$pid"; rc=$?
  fi
  grep -E '^(RESULT|AX_PROBE) ' "$out" | sed "s/ \([a-z_]*=\)/ ${P}\1/g"
  grep -vE '^(RESULT|AX_PROBE) |^\s*$' "$out" | tail -15 | sed "s/^/LOG ${P}${name}: /"
  if [ $rc -eq 124 ]; then
    echo "RESULT ${P}${name}=failed: no_exit_after_${STEP_TIMEOUT}s"
  elif [ $rc -ne 0 ]; then
    echo "RESULT ${P}${name}=failed: $(grep -vE '^(RESULT|AX_PROBE) |^\s*$|RUST_BACKTRACE' "$out" | head -1 | tr ' ' _)"
  else
    echo "RESULT ${P}${name}=ok"
  fi
}

# phase runs the whole test with its work dir in $W.
phase() {
  rm -rf "$W/src" "$W/state"; mkdir -p "$W/src"
  # The sample tree is a real tree from the image plus one known file.
  cp -a /usr/lib/python3.11 "$W/src/python3.11"
  echo hello-layerfs > "$W/src/README"
  echo "RESULT ${P}tree_entries=$(find "$W/src" | wc -l) ${P}tree_bytes=$(du -sb "$W/src" | cut -f1)"
  step init "$BIN/ax_probe" init "$W/src" "$W/state"
  # The daemon refuses to run the command as root, so it runs as uid 1001.
  step mount_first "$BIN/ax_probe" mount "$W/state" "$BIN/layerfs-daemon" 1001 "$CMD"
  step mount_warm "$BIN/ax_probe" mount "$W/state" "$BIN/layerfs-daemon" 1001 "$CMD"
}

# Phase 1: a tmpfs work dir in guest RAM. Phase 2: the container rootfs,
# which the guest gets over virtio-fs. Keys from phase 2 start with rootfs_.
W=/tmp/layerfs-tmpfs P=
mkdir -p "$W" && mount -t tmpfs -o size=512m tmpfs "$W" && echo "RESULT workdir=tmpfs" || echo "RESULT workdir=failed"
phase
W=/tmp/layerfs-rootfs P=rootfs_
mkdir -p "$W"
phase
'''


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global proc
        code = 200
        if self.path == "/healthz":
            body = b"ok\n"
        elif self.path == "/run":
            with lock:
                if proc is None:
                    proc = subprocess.Popen(["bash", "-c", TEST], stdout=open(LOG, "wb"), stderr=subprocess.STDOUT)
            body = b"started\n"
        elif self.path == "/result":
            with lock:
                if proc is None:
                    self.send_error(409, "GET /run first")
                    return
                if proc.poll() is None:
                    code = 202
            with open(LOG, "rb") as f:
                body = f.read()
        else:
            self.send_error(404)
            return
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


http.server.ThreadingHTTPServer(("", 80), Handler).serve_forever()
