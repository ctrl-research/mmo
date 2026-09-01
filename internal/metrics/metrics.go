// Package metrics exposes Prometheus instrumentation.
//
// A simulation you cannot see inside is one you cannot tune, so this is wired
// up from M0 rather than added when something goes wrong. The measurement that
// matters most is tick duration: the 50 ms budget is an SLO, and a room that
// overruns it must be split or capacity-limited, because no amount of extra
// hardware will fix a room that cannot keep up with real time.
package metrics

import (
	"sync"

	"github.com/ctrl-research/mmo/internal/world/room"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds every collector. One instance is created per process and
// passed to the components that report into it.
type Metrics struct {
	TickDuration *prometheus.HistogramVec
	TickOverruns *prometheus.CounterVec
	RoomEntities *prometheus.GaugeVec
	RoomPlayers  *prometheus.GaugeVec
	RoomFrozen   *prometheus.GaugeVec

	// population is what each room last reported, so the exported per-map
	// gauges can be totals rather than whichever room ticked last.
	popMu      sync.Mutex
	population map[string]map[uint64]roomPopulation

	Instances       prometheus.Gauge
	Connections     prometheus.Gauge
	ConnectionsMade prometheus.Counter

	InputsReceived  prometheus.Counter
	InputsDropped   prometheus.Counter
	SnapshotsSent   prometheus.Counter
	SnapshotBytes   prometheus.Counter
	OutboundDropped prometheus.Counter
}

// New registers every collector on r and returns them.
func New(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		population: make(map[string]map[uint64]roomPopulation),
		TickDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "mmo",
			Subsystem: "room",
			Name:      "tick_duration_seconds",
			Help:      "Wall-clock time to simulate one tick, by map.",
			// Bucketed around the 50 ms budget so the p99 alerting threshold
			// (25 ms, half the budget) falls on a boundary rather than being
			// interpolated across a wide bucket.
			Buckets: []float64{
				0.0005, 0.001, 0.002, 0.005, 0.010, 0.015,
				0.025, 0.035, 0.050, 0.075, 0.100, 0.250,
			},
		}, []string{"map"}),

		TickOverruns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "room", Name: "tick_overruns_total",
			Help: "Ticks that exceeded the 50 ms budget, by map.",
		}, []string{"map"}),

		RoomEntities: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mmo", Subsystem: "room", Name: "entities",
			Help: "Entities currently simulated, by map. Under per-player mob " +
				"layering this scales with layers times mobs, so it is the " +
				"leading indicator of tick cost.",
		}, []string{"map"}),

		RoomFrozen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mmo", Subsystem: "room", Name: "frozen",
			Help: "Players in a room being sent nothing, by map. A frozen " +
				"player has dropped their connection or is mid-transfer: their " +
				"body stays in the world so it does not blink in and out, but " +
				"the snapshot phase skips them. Worth watching because from " +
				"outside it looks exactly like the server failing to keep up.",
		}, []string{"map"}),

		RoomPlayers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "mmo", Subsystem: "room", Name: "players",
			Help: "Players currently in a room, by map.",
		}, []string{"map"}),

		Instances: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "mmo", Subsystem: "world", Name: "instances",
			Help: "Live room instances on this node.",
		}),

		Connections: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "connections",
			Help: "Open WebSocket connections.",
		}),

		ConnectionsMade: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "connections_total",
			Help: "WebSocket connections accepted since start.",
		}),

		InputsReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "inputs_received_total",
			Help: "Intent messages accepted from clients.",
		}),

		InputsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "inputs_dropped_total",
			Help: "Intent messages rejected: malformed, stale, or rate limited.",
		}),

		SnapshotsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "snapshots_sent_total",
			Help: "Snapshots written to clients.",
		}),

		SnapshotBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "snapshot_bytes_total",
			Help: "Bytes of snapshot payload written to clients.",
		}),

		OutboundDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mmo", Subsystem: "gateway", Name: "outbound_dropped_total",
			Help: "Messages dropped because a client's send queue was full. " +
				"Non-zero means a client cannot keep up and will be " +
				"disconnected rather than allowed to stall the tick loop.",
		}),
	}

	r.MustRegister(
		m.TickDuration, m.TickOverruns, m.RoomEntities, m.RoomPlayers,
		m.RoomFrozen,
		m.Instances, m.Connections, m.ConnectionsMade,
		m.InputsReceived, m.InputsDropped,
		m.SnapshotsSent, m.SnapshotBytes, m.OutboundDropped,
	)
	return m
}

// roomPopulation is one room's last reported headcount.
type roomPopulation struct {
	entities, players, frozen int
}

// ObserveTick records one tick's cost. It satisfies room.Observer, which is
// how the simulation reports measurements without depending on Prometheus.
func (m *Metrics) ObserveTick(r room.TickReport) {
	mapID := r.MapID
	m.TickDuration.WithLabelValues(mapID).Observe(r.Duration.Seconds())

	// Summed across this node's instances of the map rather than Set from each.
	//
	// A node hosting three channels of one map used to call Set three times a
	// tick with a label of only the map, so the gauge kept whichever channel
	// ticked last -- the population read as one channel's worth however many
	// there were, and the dashboard understated it by however many it did not
	// count. The per-instance figures are kept here and only the total is
	// exported, which keeps one series per map rather than one per room: rooms
	// come and go, and a gauge labelled by instance would accumulate a series
	// for every channel the process had ever run.
	m.perInstance(mapID, r)

	// Resolved whether or not it overran, which is what creates the series at
	// zero for a map that is behaving. A counter that only exists once it has
	// been incremented reads as "no data" on a dashboard and in an alert --
	// indistinguishable from a metric that was renamed or a server that is not
	// being scraped, which is the opposite of what it means.
	overruns := m.TickOverruns.WithLabelValues(mapID)
	if r.Duration > room.TickBudget {
		overruns.Inc()
	}
}

// perInstance records one room's population and re-exports the map's total.
func (m *Metrics) perInstance(mapID string, r room.TickReport) {
	m.popMu.Lock()
	defer m.popMu.Unlock()

	rooms, ok := m.population[mapID]
	if !ok {
		rooms = make(map[uint64]roomPopulation)
		m.population[mapID] = rooms
	}
	rooms[r.Instance] = roomPopulation{
		entities: r.Entities, players: r.Players, frozen: r.Frozen,
	}

	var entities, players, frozen int
	for _, p := range rooms {
		entities += p.entities
		players += p.players
		frozen += p.frozen
	}

	m.RoomEntities.WithLabelValues(mapID).Set(float64(entities))
	m.RoomPlayers.WithLabelValues(mapID).Set(float64(players))
	m.RoomFrozen.WithLabelValues(mapID).Set(float64(frozen))
}

// ForgetRoom drops an instance's contribution to its map's totals.
//
// Called when a room retires. Without it a room that stood empty and let itself
// go keeps counting whatever it held on its last tick, and the population only
// ever rises.
func (m *Metrics) ForgetRoom(mapID string, instance uint64) {
	m.popMu.Lock()
	defer m.popMu.Unlock()

	rooms := m.population[mapID]
	if rooms == nil {
		return
	}
	delete(rooms, instance)

	var entities, players, frozen int
	for _, p := range rooms {
		entities += p.entities
		players += p.players
		frozen += p.frozen
	}
	m.RoomEntities.WithLabelValues(mapID).Set(float64(entities))
	m.RoomPlayers.WithLabelValues(mapID).Set(float64(players))
	m.RoomFrozen.WithLabelValues(mapID).Set(float64(frozen))
}

var _ room.Observer = (*Metrics)(nil)
