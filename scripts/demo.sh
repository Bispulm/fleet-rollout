#!/usr/bin/env bash
# The demo: a fleet of simulated devices, each one the real agent on a flaky network, takes a
# good release through every stage. Then a release that fails on 30% of devices halts in its
# first stage, and every device it reached goes back to what it ran before.
#
# Needs Go, Docker with Compose, make, and openssl. Run it from the repository root, or with
# make demo. DEVICES sets the fleet size, 10,000 by default. It starts Postgres with make
# dev-up and leaves it running; make dev-reset stops it and deletes the data.
set -euo pipefail

DEVICES="${DEVICES:-10000}"
POLL_INTERVAL="${POLL_INTERVAL:-10s}"
SOAK="${SOAK:-30s}"
# A stage's failure rate counts once 90% of the first stage has reported: 90 for 10,000 devices.
# A few devices crash mid-install on this network and count as failures, and a smaller sample
# could mistake that for a bad release.
MIN_OBSERVATIONS=$((DEVICES * 9 / 1000 > 1 ? DEVICES * 9 / 1000 : 1))
API=http://127.0.0.1:18100
PG_PORT="${ROLLOUT_POSTGRES_PORT:-15431}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/fleet-rollout-demo.XXXXXX")"
BIN="$ROOT/bin"
pids=()
DATABASE=""

cleanup() {
  local rc=$?
  for pid in ${pids[@]+"${pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  if [ "$rc" != 0 ] && [ -s "$WORK/rolloutd.log" ]; then
    echo "The demo failed. rolloutd's log ends:" >&2
    tail -n 15 "$WORK/rolloutd.log" >&2
  fi
  if [ -n "$DATABASE" ]; then
    docker compose exec -T postgres psql -q -U rollout -d postgres -c "DROP DATABASE $DATABASE WITH (FORCE)" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n==> %s\n' "$*"; }
fail() {
  echo "demo: $*" >&2
  exit 1
}

cd "$ROOT"
if curl -s -o /dev/null "$API/readyz"; then
  fail "something is already serving $API; stop it first (make obs-down stops the observability stack)"
fi
say "Starting Postgres and building"
make dev-up build >/dev/null

"$BIN/rolloutctl" key generate --private-key "$WORK/release.key" --public-key "$WORK/release.pub" >/dev/null
NAME="rollout_demo_$(openssl rand -hex 4)"
docker compose exec -T postgres psql -q -U rollout -d postgres -c "CREATE DATABASE $NAME" >/dev/null
DATABASE="$NAME"

export ROLLOUTD_DATABASE_URL="postgres://rollout:rollout_dev@127.0.0.1:$PG_PORT/$DATABASE?sslmode=disable"
export ROLLOUTD_ADMIN_TOKEN="$(openssl rand -hex 24)"
export ROLLOUT_ADMIN_TOKEN="$ROLLOUTD_ADMIN_TOKEN"
export ROLLOUTD_TRUST_KEYS_FILE="$WORK/release.pub"
export ROLLOUTD_ARTIFACT_DIR="$WORK/artifacts"
export ROLLOUTD_POLL_INTERVAL="$POLL_INTERVAL"
export ROLLOUT_API_URL="$API"
"$BIN/rolloutd" migrate 2>"$WORK/migrate.log" || {
  cat "$WORK/migrate.log" >&2
  fail "migration failed"
}
"$BIN/rolloutd" serve 2>"$WORK/rolloutd.log" &
ROLLOUTD=$!
pids+=($ROLLOUTD)
ready=""
for _ in $(seq 50); do
  kill -0 "$ROLLOUTD" 2>/dev/null || fail "rolloutd exited at startup"
  if curl -fs -o /dev/null "$API/readyz"; then
    ready=1
    break
  fi
  sleep 0.2
done
[ -n "$ready" ] || fail "rolloutd wasn't ready within 10 seconds"

say "Registering $DEVICES devices, each running release 1"
"$BIN/rolloutctl" fleet create lab-demo >/dev/null
"$BIN/rolloutctl" device register-synthetic --fleet lab-demo --count "$DEVICES" --platform linux/arm64 \
  --tokens-file "$WORK/tokens.txt" >/dev/null

push() { # version release
  head -c 65536 /dev/urandom >"$WORK/app-$1.bin"
  "$BIN/rolloutctl" artifact push --name demo-app --version "$1" --platform linux/arm64 --release "$2" \
    --file "$WORK/app-$1.bin" --key "$WORK/release.key" -o json | sed -n 's/^  "id": "\(.*\)",$/\1/p'
}
GOOD="$(push 2.0.0 2)"
BAD="$(push 3.0.0 3)"

say "Starting the fleet: real agents, with dropped and lost requests, corrupted downloads, crashes, and outages"
# Release 3 fails its health check on 30% of devices. Every other release is fine.
"$BIN/rollout-sim" fleet --api "$API" --tokens-file "$WORK/tokens.txt" --trust-keys "$WORK/release.pub" --initial-release 1 \
  --preset flaky --bad-release-rate 0.3 --bad-release 3 >"$WORK/fleet.txt" 2>"$WORK/fleet.log" &
FLEET=$!
pids+=($FLEET)

create() { # name artifact [flags...]
  local name="$1" artifact="$2"
  shift 2
  "$BIN/rolloutctl" rollout create --fleet lab-demo --name "$name" --artifact "$artifact" \
    --stages 1,10,50,100 --soak "$SOAK" --max-failure-rate 5% --min-observations "$MIN_OBSERVATIONS" "$@" -o json |
    sed -n 's/^  "id": "\(.*\)",$/\1/p'
}

say "Rolling out release 2: four stages, $SOAK soaks, halting above 5% failures once a stage has $MIN_OBSERVATIONS outcomes"
ROLLOUT="$(create good-2-0-0 "$GOOD")"
"$BIN/rolloutctl" rollout start "$ROLLOUT" >/dev/null
"$BIN/rolloutctl" rollout status "$ROLLOUT" --watch --interval 5s | grep -E '^[0-9]{2}:|^STAGE|^[1-4] '

say "Rolling out release 3, which fails on 30% of devices, with rollback on breach"
ROLLOUT="$(create bad-3-0-0 "$BAD" --on-breach rollback)"
"$BIN/rolloutctl" rollout start "$ROLLOUT" >/dev/null
# The watch fails because the rollout doesn't complete, which is the point here. Its status says why, and the
# check after it makes sure it ended the way it should, not some other way.
"$BIN/rolloutctl" rollout status "$ROLLOUT" --watch --interval 2s 2>/dev/null |
  grep -E '^[0-9]{2}:|^(Halted|Failures|Rollback):|^STAGE|^[1-4] ' || true
STATE="$("$BIN/rolloutctl" rollout status "$ROLLOUT" -o json | sed -n 's/^  "state": "\(.*\)",$/\1/p')"
[ "$STATE" = rolled_back ] || fail "the bad release ended ${STATE:-unknown}, not rolled_back"

say "Waiting for the fleet to settle, then stopping it"
sleep 20
kill -INT "$FLEET"
wait "$FLEET" || true
cat "$WORK/fleet.txt"
