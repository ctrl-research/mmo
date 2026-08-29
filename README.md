# mmo

A self-hosted, horizontally scalable 2D MMO — drawing on MapleStory's side-scrolling combat and channels, Path of Exile's passive tree and stat math, and Old School RuneScape's secondary skills.

**Status: M5 complete** — a world of three connected zones, with people in it. Chat on five channels, parties that merge their members' mob layers and share experience, guilds with ranks and a message of the day, and a friends list. Walk through a portal and the character is handed to a room on a different world node, over the bus, with the same protocol that will carry it between processes. Switch channels, unlock waypoints by walking over them, and fast-travel from the world map. Kill a mob, get a rare with tiered affixes, equip it, and watch your stats change by exactly what the tooltip predicted. Log out and back in to find everything where you left it. See `docs/roadmap.md`.

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
| Art | Generated pixel sprites (`make sprites`) | No artist; a generator that is read and reviewed beats placeholder rectangles |

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
make services      # Postgres and Redis
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

`make run` passes `--dev-auth`, which signs players in with no identity check
at all. See below for the alternatives.

### Signing in

Three ways, in order of preference.

**Local accounts** (username and password, held by this server) are on by
default. The allowlist admits nobody until you add someone:

```
mmo allow jonathan          # allow a local username to register
mmo allowlist               # list every rule
mmo revoke jonathan         # remove one
mmo passwd jonathan         # set a password from the terminal
```

There is also `mmo give`, for testing a drop without farming for it, or for
making good after a genuine loss. It goes through the same generator and the
same audit journal as a real drop:

```
mmo give Sigrun weapon.iron_sword --rarity=rare --ilvl=40
```

They can then register at the login screen with that username. Passwords are
Argon2id, sign-in attempts are throttled per account and per address, and an
unknown username is indistinguishable from a wrong password.

Turn it off with `--local-auth=false` if you would rather not have this server
holding password hashes at all.

**Development sign-in** (`--dev-auth`) has no identity check whatsoever and
allowlists whoever asks. It exists so the game is playable in one command.
Never enable it on anything reachable by someone you do not trust.

### Signing in with an OIDC provider

The server is an OIDC relying party, so any provider publishing a discovery
document works. Copy `deploy/providers.example.toml`, fill in the client IDs,
and start with:

```
./bin/mmo --providers=providers.toml --public-url=https://your.host --secure-cookies
```

Client secrets are read from the environment (`client_secret = "env:NAME"`), so
the file can be committed while the secrets are not. The redirect URI
registered with each provider must be exactly `<public-url>/auth/callback`.

**The allowlist admits nobody until you add an entry.** That is deliberate —
"empty means open" fails toward a server anyone can join:

```
mmo allow --provider='' --kind=email you@example.com
mmo allow --provider='' --kind=email_domain your-company.com
```

`--kind` is `subject`, `email`, or `email_domain`; an empty `--provider`
matches any. Removing an entry takes effect at that player's next sign-in,
because the allowlist is re-checked every time rather than only at signup.

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
