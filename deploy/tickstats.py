"""Summarise a world node's tick histogram from its /metrics output.

Quantiles are computed from the bucket counts the same way Prometheus does --
linear interpolation inside the bucket the quantile falls in -- so the numbers
here and the numbers on the dashboard mean the same thing.
"""

import re
import sys
from collections import defaultdict


def main() -> None:
    text = sys.stdin.read()

    buckets: dict[str, list[tuple[float, float]]] = defaultdict(list)
    counts: dict[str, float] = {}
    overruns: dict[str, float] = {}
    players: dict[str, float] = {}
    frozen: dict[str, float] = {}
    entities: dict[str, float] = {}
    instances = 0.0

    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        m = re.match(r'^(\w+)\{?([^}]*)\}?\s+(\S+)$', line)
        if not m:
            continue
        name, labels, value = m.group(1), m.group(2), m.group(3)
        try:
            v = float(value)
        except ValueError:
            continue

        label = dict(re.findall(r'(\w+)="([^"]*)"', labels))
        mapID = label.get("map", "")

        if name == "mmo_room_tick_duration_seconds_bucket":
            buckets[mapID].append((float(label["le"]), v))
        elif name == "mmo_room_tick_duration_seconds_count":
            counts[mapID] = v
        elif name == "mmo_room_tick_overruns_total":
            overruns[mapID] = v
        elif name == "mmo_room_players":
            players[mapID] = v
        elif name == "mmo_room_frozen":
            frozen[mapID] = v
        elif name == "mmo_room_entities":
            entities[mapID] = v
        elif name == "mmo_world_instances":
            instances = v

    if not counts:
        print("  no rooms ticked on this node")
        return

    print(f"  {'map':<12} {'ticks':>9} {'p50':>8} {'p99':>8} {'p999':>8} "
          f"{'max≤':>8} {'over':>6} {'players':>8} {'entities':>9}")

    total_over = 0.0
    for mapID in sorted(counts):
        bs = sorted(buckets[mapID])
        q = lambda p: quantile(bs, counts[mapID], p)
        over = overruns.get(mapID, 0.0)
        total_over += over
        print(f"  {mapID:<12} {counts[mapID]:>9.0f} "
              f"{ms(q(0.50)):>8} {ms(q(0.99)):>8} {ms(q(0.999)):>8} "
              f"{ms(highest(bs)):>8} {over:>6.0f} "
              f"{players.get(mapID, 0):>8.0f} {frozen.get(mapID, 0):>7.0f} "
              f"{entities.get(mapID, 0):>9.0f}")

    print(f"  instances {instances:.0f}, overruns {total_over:.0f}")


def quantile(buckets: list[tuple[float, float]], total: float, p: float) -> float:
    """Prometheus-style histogram_quantile over cumulative buckets."""
    if total == 0:
        return 0.0
    want = p * total
    previous_le, previous_count = 0.0, 0.0
    for le, count in buckets:
        if count >= want:
            if le == float("inf"):
                return previous_le
            if count == previous_count:
                return le
            # Interpolate within the bucket, which is what Prometheus does and
            # why a p99 can read as a value no tick actually took.
            span = count - previous_count
            return previous_le + (le - previous_le) * (want - previous_count) / span
        previous_le, previous_count = le, count
    return previous_le


def highest(buckets: list[tuple[float, float]]) -> float:
    """The lowest bucket bound that contains every observation.

    Not a real maximum -- a histogram does not keep one -- but it bounds it,
    which is what matters when the question is whether anything went over
    budget.
    """
    total = buckets[-1][1] if buckets else 0.0
    for le, count in buckets:
        if count >= total:
            return le
    return 0.0


def ms(seconds: float) -> str:
    if seconds == float("inf"):
        return "inf"
    if seconds >= 1:
        return f"{seconds:.1f}s"
    return f"{seconds * 1000:.2f}ms"


if __name__ == "__main__":
    main()
