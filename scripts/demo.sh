#!/usr/bin/env bash
# Runs a self-update end to end on this machine: start 1.0.0, publish 1.1.0,
# and watch the running program replace itself. Everything lives in .demo/.
set -euo pipefail
cd "$(dirname "$0")/.."

DEMO=.demo
PORT=${PORT:-8787}
OS=$(go env GOOS)
ARCH=$(go env GOARCH)
EXT=""
[ "$OS" = windows ] && EXT=".exe"
ARTIFACT="app-$OS-$ARCH$EXT"
APP="$PWD/$DEMO/install/app$EXT"

rm -rf "$DEMO"
mkdir -p "$DEMO/install" "$DEMO/bin"

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

cleanup() {
	# Scope cleanup to this checkout, including the child launched at handover.
	pkill -f "$APP" 2>/dev/null || true
	[ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
	return 0
}
trap cleanup EXIT

step "Generating a signing key"
go build -o "$DEMO/bin/" ./cmd/release ./cmd/update-server
"$DEMO/bin/release" keygen --key-id demo --out "$DEMO/keys"
PUBKEYS=$(cat "$DEMO/keys/demo.pub")

build() {
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "-X nametag-updater/internal/version.Version=$1 -X nametag-updater/internal/version.PublicKeys=$PUBKEYS" \
		-o "$DEMO/dist/$1/$ARTIFACT" ./cmd/app
}

publish() {
	"$DEMO/bin/release" publish --version "$1" --dist "$DEMO/dist/$1" \
		--key "$DEMO/keys/demo.key" --releases "$DEMO/releases"
}

step "Building and installing 1.0.0"
build 1.0.0
cp "$DEMO/dist/1.0.0/$ARTIFACT" "$APP"

step "Starting the update server on 127.0.0.1:$PORT (log: $DEMO/server.log)"
"$DEMO/bin/update-server" --addr "127.0.0.1:$PORT" --releases "$DEMO/releases" 2>"$DEMO/server.log" &
SERVER_PID=$!
sleep 1
if ! kill -0 "$SERVER_PID" 2>/dev/null; then
	echo "update server failed to start; inspect $DEMO/server.log" >&2
	exit 1
fi

step "Starting app 1.0.0"
"$APP" --server "http://127.0.0.1:$PORT" --interval 2s --healthy-after 3s --heartbeat 1s &
sleep 4

step "Publishing 1.1.0 while 1.0.0 is running"
build 1.1.0
publish 1.1.0
DEMO_READY=0
for attempt in {1..30}; do
	DEMO_VERSION=$("$APP" --version)
	DEMO_STATE=$(cat "$DEMO/install/.updater/state.json" 2>/dev/null || true)
	if [ "$DEMO_VERSION" = "1.1.0" ] &&
		[[ "$DEMO_STATE" == *'"high_water": "1.1.0"'* ]] &&
		[[ "$DEMO_STATE" != *'"pending"'* ]]; then
		DEMO_READY=1
		break
	fi
	sleep 1
done
if [ "$DEMO_READY" -ne 1 ]; then
	echo "demo failed: version 1.1.0 was not installed and confirmed within 30 seconds" >&2
	exit 1
fi

step "Result"
echo "installed binary reports version: $("$APP" --version)"
echo "updater state:"
cat "$DEMO/install/.updater/state.json"
echo
