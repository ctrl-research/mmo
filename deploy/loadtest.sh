#!/usr/bin/env bash
#
# Run a load test against a locally split cluster.
#
# Starts world nodes and gateways as separate processes against one Postgres,
# Redis and NATS, drives bots at them, and reports what each world node's tick
# loop did while that was happening.
#
# Everything runs on one machine, which is the point and the limit: it proves
# the distributed paths carry real traffic, and it measures this machine rather
# than the design. The bots are the expensive half -- a thousand of them decode
# twenty thousand snapshots a second -- so a tick p99 measured here is a tick
# p99 measured while competing with its own load generator.
#
#   deploy/loadtest.sh --bots=1000 --worlds=3 --gateways=2
#
set -euo pipefail

BOTS=200
WORLDS=3
GATEWAYS=2
RAMP=30s
DURATION=90s
PREFIX=load
DATABASE_URL="${DATABASE_URL:-postgres://mmo:devpassword@localhost:5433/mmo?sslmode=disable}"
REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
NATS_URL="${NATS_URL:-nats://localhost:4222}"

# A shared signing key, because tickets are issued over HTTP by one gateway and
# redeemed on the socket by whichever one the client lands on. Without it every
# gateway past the first rejects the others' tickets, which is the single thing
# stopping the deployed gateway from being scaled today.
SESSION_SECRET="loadtest-signing-key-at-least-32-bytes-long"

for arg in "$@"; do
  case $arg in
    --bots=*)     BOTS="${arg#*=}" ;;
    --worlds=*)   WORLDS="${arg#*=}" ;;
    --gateways=*) GATEWAYS="${arg#*=}" ;;
    --ramp=*)     RAMP="${arg#*=}" ;;
    --duration=*) DURATION="${arg#*=}" ;;
    --prefix=*)   PREFIX="${arg#*=}" ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$(mktemp -d)"
pids=()

cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  echo "logs in $out"
}
trap cleanup EXIT

echo "building"
go build -o "$out/mmo" "$root/cmd/mmo"
go build -o "$out/mmobot" "$root/cmd/mmobot"

# A clean directory. Nodes that registered in a previous run are still listed
# and placement would send rooms to processes that are no longer there.
#
# The directory keys only. Clearing everything under mmo: takes the lease
# counter with it, and that counter is compared against tokens already written
# to Postgres -- so restarting it at one fences every character that has played
# before, and the run reads as the server dropping half its players with "your
# character was claimed elsewhere". The server now reseeds the counter from the
# database at startup, which makes this survivable rather than merely avoided;
# it is still not this script's business to delete.
if command -v docker >/dev/null && docker ps --format '{{.Names}}' | grep -q '^mmo-redis-1$'; then
  docker exec mmo-redis-1 redis-cli EVAL \
    "local ks=redis.call('KEYS','mmo:{dir}*'); for i=1,#ks do redis.call('DEL',ks[i]) end; return #ks" 0 >/dev/null
  echo "cleared the room directory"

  # Wait out the previous run's leases.
  #
  # A killed server does not release them, so for LeaseTTL after a run its
  # characters are still owned by a process that no longer exists. Bots reusing
  # those names are refused with "this character is already in play", which
  # reads as the server rejecting a third of its traffic -- and it is really
  # just the previous run not being over yet. Fast when there is nothing to
  # wait for, which is every run but the second.
  for _ in $(seq 1 400); do
    held=$(docker exec mmo-redis-1 redis-cli EVAL \
      "return #redis.call('KEYS','mmo:lease:char:*')" 0)
    [ "$held" = "0" ] && break
    echo "  waiting for $held lease(s) from a previous run to expire"
    sleep 2
  done
fi

wait_ready() {
  local log=$1 what=$2
  for _ in $(seq 1 600); do
    grep -q "server ready" "$log" && return 0
    sleep 0.1
  done
  echo "$what never became ready; last lines:" >&2
  tail -5 "$log" >&2
  exit 1
}

world_admin=()
for i in $(seq 1 "$WORLDS"); do
  port=$((9200 + i))
  DATABASE_URL="$DATABASE_URL" "$out/mmo" \
    --roles=world --node-id="world-$i" \
    --admin-addr="0.0.0.0:$port" \
    --redis-addr="$REDIS_ADDR" --nats-url="$NATS_URL" \
    --log-json > "$out/world-$i.log" 2>&1 &
  pids+=($!)
  world_admin+=("$port")
done
for i in $(seq 1 "$WORLDS"); do wait_ready "$out/world-$i.log" "world-$i"; done
echo "$WORLDS world nodes up"

gateway_urls=()
for i in $(seq 1 "$GATEWAYS"); do
  port=$((8300 + i))
  DATABASE_URL="$DATABASE_URL" SESSION_SECRET="$SESSION_SECRET" "$out/mmo" \
    --roles=gateway --node-id="gateway-$i" \
    --addr=":$port" --admin-addr="0.0.0.0:$((9300 + i))" \
    --redis-addr="$REDIS_ADDR" --nats-url="$NATS_URL" \
    --dev-auth --log-json > "$out/gateway-$i.log" 2>&1 &
  pids+=($!)
  gateway_urls+=("http://localhost:$port")
done
for i in $(seq 1 "$GATEWAYS"); do wait_ready "$out/gateway-$i.log" "gateway-$i"; done
echo "$GATEWAYS gateways up: ${gateway_urls[*]}"

# Bots are split across the gateways, which is what a load balancer would do.
# One mmobot per gateway rather than one process for all of them: a single
# process holding a thousand sockets is a bottleneck of its own, and the point
# is to load the server.
#
# A prefix per gateway, so the two sets of bots are different characters. Give
# them the same names and the lease does its job: one set gets in and the other
# is told the character is already in play, which reads as a server that only
# accepts half its traffic.
per=$(( (BOTS + GATEWAYS - 1) / GATEWAYS ))
echo "driving $BOTS bots ($per per gateway)"

bot_pids=()
for i in $(seq 1 "$GATEWAYS"); do
  "$out/mmobot" --server="${gateway_urls[$((i-1))]}" \
    --bots="$per" --ramp="$RAMP" --duration="$DURATION" \
    --prefix="${PREFIX}g${i}" \
    --report=15s > "$out/bots-$i.log" 2>&1 &
  bot_pids+=($!)
  pids+=($!)
done

for pid in "${bot_pids[@]}"; do wait "$pid" || true; done

echo
for i in $(seq 1 "$GATEWAYS"); do
  echo "=== gateway $i ==="
  tail -n 14 "$out/bots-$i.log"
done

echo
echo "=== what the world nodes did ==="
for i in $(seq 1 "$WORLDS"); do
  port="${world_admin[$((i-1))]}"
  echo "--- world-$i ---"
  curl -sf "http://localhost:$port/metrics" | python3 "$root/deploy/tickstats.py" || echo "  (no metrics)"
done

echo
echo "=== errors ==="
grep -ihoE '"level":"(ERROR|WARN)","msg":"[^"]+"' "$out"/world-*.log "$out"/gateway-*.log \
  | sort | uniq -c | sort -rn | head -12 || echo "  none"
