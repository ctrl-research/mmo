# mmo

A self-hosted, horizontally scalable 2D MMO — drawing on MapleStory's side-scrolling combat and channels, Path of Exile's passive tree and stat math, and Old School RuneScape's secondary skills.

**Status: M1 complete** — the combat vertical slice runs. Two players share a map, see each other, and hunt their own private mobs: kill, gain experience, level up, and loot. Client-side prediction is verified bit-identical to the server. See `docs/roadmap.md`.

## Design goals

1. **Playable on one box.** `docker compose up` runs a complete game for a handful of players. No Kubernetes, no message broker.
2. **Scales to 1000+ concurrent players without a rewrite.** Going from one node to many is a config change plus two interface implementations, not a redesign.
3. **Server-authoritative.** The client renders state and sends intents. It never decides an outcome.

## Stack

| Layer | Choice | Why |
|---|---|---|
| Server | Go 1.26 | Single static binary, cheap goroutine-per-room concurrency, low-latency GC |
| Simulation | Pure Go, fixed-point, → TinyGo WASM | The client runs *the same code* as the server, so prediction never drifts |
| Client | TypeScript + PixiJS + Vite | Thin renderer; complex MMO UI built in DOM where it is strongest |
| Transport | WebSocket, Protobuf, 20 Hz | WebTransport is the upgrade path if loss becomes a problem |
| Durable state | Postgres | Checkpoint store, not live simulation state |
| Ephemeral state | Redis | Presence, sessions, character leases, room directory |
| Bus | in-process channels → NATS | Swapped behind one interface at scale |
| Content | TOML / JSON / Tiled | No content change requires writing Go |

## Documentation

| | |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Room-instance scaling model, process roles, tick loop, the two swappable seams |
| [`docs/protocol.md`](docs/protocol.md) | Wire format, handshake, prediction and reconciliation, interpolation |
| [`docs/data-model.md`](docs/data-model.md) | Persistence, the single-writer invariant, anti-duplication, stat and progression math |
| [`docs/content-pipeline.md`](docs/content-pipeline.md) | Content as data, the skill effect DSL, mobs, drops, maps, passive tree |
| [`docs/roadmap.md`](docs/roadmap.md) | Milestones M0–M9 with exit criteria |

## The three ideas that matter

Read the docs for detail, but the design rests on three things:

**The room instance is the unit of scale.** Players interact almost exclusively within the room they are standing in. Rooms are independent, single-goroutine, lock-free tick loops; everything crossing rooms is low-frequency and goes over a bus. Scaling is distributing rooms across nodes.

**The client runs the server's simulation code.** `internal/world/sim` is pure, fixed-point, dependency-free Go compiled to WASM. Client-side prediction replays the identical implementation, so predicted and authoritative state agree bit-for-bit instead of drifting into rubber-banding.

**Content is data, not code.** Items, mobs, skills, passives, drops, and maps are schema-validated files in git. Adding a skill is twelve lines of TOML, not a Go function — which is the difference between shipping ten skills and shipping three hundred.

## Running it

```
mise install
make client-install
make run
```

Then open the URL `make run` prints — **http://localhost:8080** by default.
Enter a name and connect; open a second tab to see two players share a room.

That is one process serving the game and the client from a single port, with no
proxy and nothing else to line up.

**If port 8080 is taken** — it often is, since Docker, Lima, and similar hold it
on many machines — pass a different one:

```
make run PORT=8088
```

`--dev-auth`, which `make run` passes for you, issues game tickets with no
identity check, because OIDC arrives in M2. Never enable it on anything
reachable by someone you do not trust.

### Live reload

The setup above rebuilds the client only when you run it. For live reload while
editing client code, run two processes **in two terminals**:

```
# terminal 1 — game server
./bin/mmo --dev-auth --addr=:8080 --log-level=debug

# terminal 2 — Vite dev server, proxying to it
MMO_SERVER=http://localhost:8080 npm --prefix client run dev
```

Open **the URL Vite prints**, not the server's. It is usually
http://localhost:5173, but Vite moves to the next free port if that one is
taken and only says so in its output.

Two things to keep aligned, since both are easy to get wrong:

- The port must match on both sides. `MMO_SERVER` tells Vite where to proxy; if
  it points somewhere else, that other process answers and the client reports
  that the address is not the game server.
- The server here has no `--client-dir`, so it serves the API and WebSocket but
  no page. Opening the server's own port gives a 404 — that is expected, and it
  is why you open Vite's URL instead.

`make dev` prints these instructions if you forget them.

### Testing

```
make test               # Go tests
make test-race          # ...with the race detector
make test-conformance   # the WASM build must match the Go build exactly
make lint               # vet, protobuf lint, client typecheck
```

Toolchain versions are pinned in `.tool-versions`; `mise install` (or
`asdf install`) sets them up, and CI reads the same file.

`make test-conformance` is the one that makes prediction trustworthy: it replays
the golden corpus through the compiled WebAssembly and fails on a single
differing bit.

## Contributing

`main` is protected: all changes via PR with review. See `CONTRIBUTING.md` and `AGENTS.md`.
