#!/usr/bin/env bash
#
# Runs the S3 integration tests against a throwaway MinIO server.
#
# MinIO no longer publishes community container images or binaries, so a
# pinned release is built from source with `go install` the first time and the
# binary is cached. The server listens on localhost only, keeps its data in a
# temporary directory, and is stopped when the script exits.
#
# Environment:
#   MINIO_VERSION  MinIO release tag to build (default below)
#   MINIO_BIN      use this minio binary instead of building one
#   MINIO_PORT     port to listen on (default 9123)
#   MINIO_CACHE    where built binaries are cached
#   RUN            go test -run pattern (default: the S3 integration tests)
#
# Extra arguments are passed to `go test`, e.g. `-v`.

set -euo pipefail

MINIO_VERSION="${MINIO_VERSION:-RELEASE.2025-09-07T16-13-09Z}"
MINIO_PORT="${MINIO_PORT:-9123}"
MINIO_CACHE="${MINIO_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/gobackup/minio}"
RUN="${RUN:-Conformance|Integration}"

cd "$(dirname "$0")/.."

if [[ -z "${MINIO_BIN:-}" ]]; then
	MINIO_BIN="$MINIO_CACHE/$MINIO_VERSION/minio"
	if [[ ! -x "$MINIO_BIN" ]]; then
		echo "Building MinIO $MINIO_VERSION (first run only; takes a minute or two)..." >&2
		GOBIN="$(dirname "$MINIO_BIN")" GOFLAGS= go install "github.com/minio/minio@$MINIO_VERSION"
	fi
fi

data_dir="$(mktemp -d)"
log_file="$data_dir.log"
minio_pid=""
cleanup() {
	if [[ -n "$minio_pid" ]]; then
		kill "$minio_pid" 2>/dev/null || true
		wait "$minio_pid" 2>/dev/null || true
	fi
	rm -rf "$data_dir" "$log_file"
}
trap cleanup EXIT

MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin MINIO_BROWSER=off \
	"$MINIO_BIN" server --quiet --address "127.0.0.1:$MINIO_PORT" "$data_dir" >"$log_file" 2>&1 &
minio_pid=$!

endpoint="http://127.0.0.1:$MINIO_PORT"
for _ in $(seq 1 60); do
	if curl -sf "$endpoint/minio/health/live" >/dev/null; then
		break
	fi
	if ! kill -0 "$minio_pid" 2>/dev/null; then
		echo "MinIO exited during startup:" >&2
		cat "$log_file" >&2
		exit 1
	fi
	sleep 0.5
done
if ! curl -sf "$endpoint/minio/health/live" >/dev/null; then
	echo "MinIO did not become healthy at $endpoint:" >&2
	cat "$log_file" >&2
	exit 1
fi

# Point the AWS SDK at MinIO only. Anything from the caller's own AWS setup
# could otherwise take precedence over these credentials.
unset AWS_PROFILE AWS_SESSION_TOKEN AWS_DEFAULT_PROFILE
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
export AWS_REGION=us-east-1
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null
export GOBACKUP_S3_TEST_ENDPOINT="$endpoint"

echo "Running S3 integration tests against MinIO $MINIO_VERSION at $endpoint" >&2
go test -race -count=1 -run "$RUN" "$@" ./...
