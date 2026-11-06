#!/usr/bin/env bash
# Drives the Web UI in a real browser, against test/webui-demo, which uses a fake target. Needs no root, kernel modules or VM, only
# Podman. Pass a directory to keep the screenshots in. The demo's files need reflink, so this must run on XFS or btrfs.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ENGINE=${ENGINE:-podman}
GO_IMAGE=docker.io/library/golang:1.26-alpine
# The npm package below has to match this image's browsers.
PLAYWRIGHT_VERSION=1.50.0
BROWSER_IMAGE=mcr.microsoft.com/playwright:v$PLAYWRIGHT_VERSION-noble
PORT=${PORT:-18421}
CONTAINER=nvweaver-webui-browser
# WORK_IN is where its files go, which has to have reflink.
WORK=$(mktemp -d -p "${WORK_IN:-$ROOT}" .webui-demo-XXXXXX)
SHOTS=${1:-$WORK/shots}
mkdir -p "$SHOTS"
DEMOS=

cleanup() {
	# shellcheck disable=SC2086 # Several process IDs.
	[ -n "$DEMOS" ] && kill $DEMOS 2>/dev/null
	$ENGINE rm -f "$CONTAINER" >/dev/null 2>&1
	rm -rf "$WORK"
}
trap cleanup EXIT

if command -v go >/dev/null; then
	(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/demo" ./test/webui-demo) || exit 1
else
	$ENGINE run --rm -v "$ROOT:/src:ro,Z" -v "$WORK:/out:Z" -w /src -e CGO_ENABLED=0 "$GO_IMAGE" go build -buildvcs=false -o /out/demo ./test/webui-demo || exit 1
fi
start_demo() { # LOG PORT DIRECTORY [flags]
	local log=$1 port=$2 dir=$3
	shift 3
	mkdir -p "$dir"
	"$WORK/demo" -listen "127.0.0.1:$port" -dir "$dir" "$@" >"$log" 2>&1 &
	DEMOS="$DEMOS $!"
	for _ in $(seq 1 60); do
		grep -q 'Demo running' "$log" && return 0
		sleep 0.5
	done
	echo "the demo didn't start"
	cat "$log"
	exit 1
}
start_demo "$WORK/demo.log" "$PORT" "$WORK/files"
# A second one with a token, for signing in.
TOKEN=webui-test-token
start_demo "$WORK/demo2.log" "$((PORT + 1))" "$WORK/files2" -token "$TOKEN"
# A third with users too, for signing in with a password. This was made by a real htpasswd: htpasswd -nbB -C 5 admin "correct horse".
USERS=$WORK/users
# shellcheck disable=SC2016 # The dollar signs are part of the hash.
printf '%s\n' 'admin:$2y$05$S59rbe.HBNjlebyPt896r.18ioUcnHVsPZfgr3rL.nNQujixCO3by' >"$USERS"
chmod 600 "$USERS"
start_demo "$WORK/demo3.log" "$((PORT + 2))" "$WORK/files3" -token "$TOKEN" -users "$USERS"

# The browser's user inside the container needs to write the screenshots.
chmod 777 "$SHOTS"
$ENGINE run --rm --name "$CONTAINER" --network host -v "$ROOT/test/webui:/test:ro,Z" -v "$SHOTS:/shots:Z" -w /tmp "$BROWSER_IMAGE" sh -c "
	npm install --silent --no-audit --no-fund playwright@$PLAYWRIGHT_VERSION >/dev/null 2>&1 || exit 1
	cp /test/ui-test.mjs . && node ui-test.mjs http://127.0.0.1:$PORT /shots http://127.0.0.1:$((PORT + 1)) $TOKEN http://127.0.0.1:$((PORT + 2))"
