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
KILL_AFTER=""     # seconds into the run to SIGKILL a world node, empty to skip
DRAIN_AFTER=""    # seconds into the run to SIGTERM a world node, empty to skip
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
    --kill-after=*) KILL_AFTER="${arg#*=}" ;;
    --drain-after=*) DRAIN_AFTER="${arg#*=}" ;;
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
world_pids=()
for i in $(seq 1 "$WORLDS"); do
  port=$((9200 + i))
  DATABASE_URL="$DATABASE_URL" "$out/mmo" \
    --roles=world --node-id="world-$i" \
    --admin-addr="0.0.0.0:$port" \
    --redis-addr="$REDIS_ADDR" --nats-url="$NATS_URL" \
    --log-json > "$out/world-$i.log" 2>&1 &
  pids+=($!)
  world_pids+=($!)
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

# Chaos: kill a world node while everyone is playing.
#
# SIGKILL, not SIGTERM. A graceful shutdown is a different test -- the point
# here is the case nobody gets to prepare for: the process is gone without
# releasing a lease, checkpointing, or telling anybody, and recovery has to come
# from state that was already durable.
if [ -n "$KILL_AFTER" ]; then
  (
    sleep "$KILL_AFTER"
    victim="${world_pids[0]}"
    echo
    echo ">>> killing world-1 (pid $victim) after ${KILL_AFTER}s"
    kill -9 "$victim" 2>/dev/null || true
    echo ">>> world-1 is gone; its characters are owned by nobody until their leases lapse"
    echo
  ) &
  pids+=($!)
fi

# The other half of chaos: ask a world node to leave, rather than shooting it.
#
# SIGTERM, which is what a rolling deploy sends. Everything the kill case has to
# recover from -- lease waits, phantom rooms, progress since the last checkpoint
# -- is avoidable when the process knows it is going, and this is the run that
# says whether it was avoided.
if [ -n "$DRAIN_AFTER" ]; then
  (
    sleep "$DRAIN_AFTER"
    victim="${world_pids[0]}"
    echo
    echo ">>> asking world-1 (pid $victim) to drain after ${DRAIN_AFTER}s"
    kill -TERM "$victim" 2>/dev/null || true
  ) &
  pids+=($!)
fi

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
  if ! curl -sf --max-time 5 "http://localhost:$port/metrics" > "$out/metrics-$i.txt" 2>/dev/null; then
    echo "  gone -- this node is not answering, which is the point if it was the one killed"
    continue
  fi
  python3 "$root/deploy/tickstats.py" < "$out/metrics-$i.txt"
done

if [ -n "$DRAIN_AFTER" ]; then
  echo
  echo "=== how the drain went ==="
  # A grep that matches nothing exits 1, and this script runs under `set -e`:
  # without the guard, a run where the drain logged nothing would fail here
  # rather than report that it logged nothing.
  grep -hoE '"msg":"(draining|drained)"[^}]*' "$out"/world-1.log | sed 's/^/  /' || echo "  the node logged no drain at all"
fi


if [ -n "$KILL_AFTER" ]; then
  echo
  echo "=== what the survivors said about the node that died ==="
  grep -ihoE '"msg":"[^"]*(lease|ownership|not reachable|refused|unreachable|no responder)[^"]*"' \
    "$out"/world-*.log "$out"/gateway-*.log | sort | uniq -c | sort -rn | head -8 || echo "  nothing"

  echo
  echo "=== is the directory still offering the dead node work? ==="
  if command -v docker >/dev/null; then
    docker exec mmo-redis-1 redis-cli ZRANGEBYSCORE 'mmo:{dir}:alive' \
      "$(python3 -c 'import time; print(int(time.time()*1000))')" +inf | sed 's/^/  live: /'
  fi
fi

echo
echo "=== what the gateways did ==="
for i in $(seq 1 "$GATEWAYS"); do
  port=$((9300 + i))
  if ! curl -sf --max-time 5 "http://localhost:$port/metrics" > "$out/gwmetrics-$i.txt" 2>/dev/null; then
    echo "  gateway-$i is not answering"
    continue
  fi
  python3 - "$out/gwmetrics-$i.txt" "gateway-$i" <<'PYEOF'
import re, sys

text = open(sys.argv[1]).read()
def value(name):
    m = re.search(r'^' + name + r'(?:\{[^}]*\})?\s+(\S+)$', text, re.M)
    return float(m.group(1)) if m else 0.0

sent = value('mmo_gateway_snapshots_sent_total')
dropped = value('mmo_gateway_outbound_dropped_total')
inputs = value('mmo_gateway_inputs_received_total')
refused = value('mmo_gateway_inputs_dropped_total')
conns = value('mmo_gateway_connections')

# Dropped output is the one that matters: it is the server giving up on a
# client rather than letting a tick block, and it is invisible from the
# client's side except as a world that updates less often than it should.
share = (dropped / (sent + dropped) * 100) if sent + dropped else 0
print("  %s: %d connections, %.0f snapshots sent, %.0f dropped (%.1f%%), "
      "%.0f inputs, %.0f refused"
      % (sys.argv[2], conns, sent, dropped, share, inputs, refused))
PYEOF
done

echo
echo "=== errors ==="
grep -ihoE '"level":"(ERROR|WARN)","msg":"[^"]+"' "$out"/world-*.log "$out"/gateway-*.log \
  | sort | uniq -c | sort -rn | head -12 || echo "  none"
