// Package metrics exposes Prometheus instrumentation.
//
// A simulation you cannot see inside is one you cannot tune, so this is wired
// up from M0 rather than added when something goes wrong. The measurement that
// matters most is tick duration: the 50 ms budget is an SLO, and a room that
// overruns it must be split or capacity-limited, because no amount of extra
// hardware will fix a room that cannot keep up with real time.
package metrics

import (
	"time"

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
		m.Instances, m.Connections, m.ConnectionsMade,
		m.InputsReceived, m.InputsDropped,
		m.SnapshotsSent, m.SnapshotBytes, m.OutboundDropped,
	)
	return m
}

// ObserveTick records one tick's cost. It satisfies room.Observer, which is
// how the simulation reports measurements without depending on Prometheus.
func (m *Metrics) ObserveTick(mapID string, d time.Duration, entities, players int) {
	m.TickDuration.WithLabelValues(mapID).Observe(d.Seconds())
	m.RoomEntities.WithLabelValues(mapID).Set(float64(entities))
	m.RoomPlayers.WithLabelValues(mapID).Set(float64(players))

	// Resolved whether or not it overran, which is what creates the series at
	// zero for a map that is behaving. A counter that only exists once it has
	// been incremented reads as "no data" on a dashboard and in an alert --
	// indistinguishable from a metric that was renamed or a server that is not
	// being scraped, which is the opposite of what it means.
	overruns := m.TickOverruns.WithLabelValues(mapID)
	if d > room.TickBudget {
		overruns.Inc()
	}
}

var _ room.Observer = (*Metrics)(nil)
