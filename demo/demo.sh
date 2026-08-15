#!/bin/sh
# Records the session shown at the top of the README.
#
# Everything here is real: a real agentswap binary, a real daemon, real
# requests. Only the upstream is a stand-in, because the demo has to show an
# account being refused on purpose and that is not a thing to arrange against
# somebody's actual quota.
#
#   ./demo/demo.sh              # print the session
#   ./demo/demo.sh | tee out    # ...and keep it for the renderer
#
# Then: ./demo/render.py < out > docs/demo.svg
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'kill ${daemon_pid-0} ${upstream_pid-0} 2>/dev/null || true; rm -rf "$work"' EXIT INT TERM

export AGENTSWAP_HOME="$work/config"
export CLAUDE_CONFIG_DIR="$work/claude"
export CODEX_HOME="$work/codex"
mkdir -p "$AGENTSWAP_HOME" "$CLAUDE_CONFIG_DIR" "$CODEX_HOME"

as="$work/agentswap"
(cd "$root" && go build -o "$as" ./cmd/agentswap)

# A stand-in upstream: the first account is out of quota for the next two
# hours, the second answers. This is what a real rate limit looks like on the
# wire.
cat > "$work/upstream.py" <<'PY'
import http.server, json

class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        if self.headers.get('X-Api-Key') == 'personal':
            self.send_response(429)
            self.send_header('Anthropic-Ratelimit-Unified-Status', 'rejected')
            self.send_header('Retry-After', '7200')
            self.end_headers()
            self.wfile.write(b'{"error":{"type":"rate_limit_error"}}')
            return
        body = json.dumps({"content": [{"type": "text",
            "text": "Done. The parser now handles nested groups."}]}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Anthropic-Ratelimit-Unified-5h-Utilization', '12')
        self.send_header('Anthropic-Ratelimit-Unified-7d-Utilization', '41')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass

http.server.HTTPServer(('127.0.0.1', 8799), H).serve_forever()
PY
python3 "$work/upstream.py" & upstream_pid=$!
sleep 1

# Two pooled accounts, as `agentswap import` would leave them.
"$as" add-key anthropic --key personal --id personal --label personal \
	--base-url http://127.0.0.1:8799 --priority 0 >/dev/null
"$as" add-key anthropic --key work --id work --label work \
	--base-url http://127.0.0.1:8799 --priority 1 >/dev/null

"$as" serve --addr 127.0.0.1:8798 > "$work/daemon.log" 2>&1 & daemon_pid=$!
sleep 1.5

say() { printf '$ %s\n' "$1"; }

say 'agentswap status'
"$as" status --addr 127.0.0.1:8798
echo

say 'claude "refactor the parser"'
curl -s -X POST http://127.0.0.1:8798/anthropic/v1/messages \
	-H 'content-type: application/json' \
	-d '{"model":"claude-opus-5","messages":[{"role":"user","content":"refactor the parser"}]}' |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["content"][0]["text"])'
echo

printf '# personal ran out mid-request. agentswap moved to work and carried on:\n'
grep -E 'exhausted, rotating|served' "$work/daemon.log" |
	sed -E 's/^time=[^ ]+ //; s/level=INFO msg=//; s/ resets=[^ ]+//' |
	sed -E 's/"//g'
echo

say 'agentswap status'
"$as" status --addr 127.0.0.1:8798
