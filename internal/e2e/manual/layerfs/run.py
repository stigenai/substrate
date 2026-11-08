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

# The LayerFS actor. It serves /healthz for the wakeup probe. GET /run runs
# the LayerFS test once in the actor and returns its output as text/plain.
# The test seals a store from a sample tree (ax_probe init), mounts it twice
# (the first mount installs the store, the second is warm), reads files and
# writes in the overlay. Lines that start with RESULT or AX_PROBE carry the
# results.
import http.server
import subprocess

TEST = r'''
set -u
export PATH=/nix/var/nix/profiles/default/bin:/usr/local/bin:/usr/bin:/bin
BIN=/opt/layerfs/bin W=/tmp/layerfs
rm -rf "$W"; mkdir -p "$W/src"
echo "RESULT dev_fuse=$( (stat -c '%F_%t:%T' /dev/fuse 2>/dev/null || echo missing) | tr ' ' _)"
# The sample tree is a real tree from the image plus one known file.
cp -a /usr/lib/python3.11 "$W/src/python3.11"
echo hello-layerfs > "$W/src/README"
echo "RESULT tree_entries=$(find "$W/src" | wc -l) tree_bytes=$(du -sb "$W/src" | cut -f1)"
CMD='test "$(cat README)" = hello-layerfs && echo "RESULT read_files=$(find . -type f | wc -l)" && echo overlay-write > new.txt && test "$(cat new.txt)" = overlay-write && echo "RESULT overlay_write=ok"'
step() {
  local name=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '%s\n' "$out" | grep -E '^(RESULT|AX_PROBE) ' || true
  if [ $rc -ne 0 ]; then
    echo "RESULT ${name}=failed: $(printf '%s\n' "$out" | grep -v '^\s*$' | grep -v RUST_BACKTRACE | head -1 | tr ' ' _)"
  else
    echo "RESULT ${name}=ok"
  fi
}
# The daemon refuses to run the command as root, so it runs as uid 1001.
step init "$BIN/ax_probe" init "$W/src" "$W/state"
step mount_first "$BIN/ax_probe" mount "$W/state" "$BIN/layerfs-daemon" 1001 "$CMD"
step mount_warm "$BIN/ax_probe" mount "$W/state" "$BIN/layerfs-daemon" 1001 "$CMD"
'''


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            body = b"ok\n"
        elif self.path == "/run":
            body = subprocess.run(["bash", "-c", TEST], capture_output=True).stdout
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


http.server.ThreadingHTTPServer(("", 80), Handler).serve_forever()
