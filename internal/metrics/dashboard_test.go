package metrics_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrl-research/mmo/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// The checked-in dashboard against the metrics the server actually exports.
//
// A dashboard is the one piece of code nothing compiles and nobody tests, and
// its failure mode is the worst kind: a panel that renders an empty graph,
// which is indistinguishable from a system doing nothing. mmo_world_instances
// was registered and never set for months, and the way that was found was
// somebody happening to look.
//
// So: every metric the dashboard names has to exist, and every metric the
// server exports should be on the dashboard or deliberately not.
const dashboardPath = "../../deploy/grafana/mmo.json"

// metricNames pulls every mmo_* series out of the dashboard's queries.
//
// Regex rather than a PromQL parser: the only thing being checked is whether
// the names are real, and a parser would be a dependency for the part of the
// query that is easiest to get right.
var metricRef = regexp.MustCompile(`mmo_[a-z_]+`)

func dashboardMetrics(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("reading the dashboard: %v", err)
	}

	// Parsed as well as scanned, because a dashboard that is not valid JSON is
	// one Grafana silently refuses to load.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", filepath.Base(dashboardPath), err)
	}

	var found []string
	for _, name := range metricRef.FindAllString(string(raw), -1) {
		// A histogram is queried through its _bucket series; the registered
		// name is the one without the suffix.
		name = strings.TrimSuffix(name, "_bucket")
		name = strings.TrimSuffix(name, "_sum")
		name = strings.TrimSuffix(name, "_count")
		if !slices.Contains(found, name) {
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found
}

// exported returns every metric name the server registers.
func exported(t *testing.T) []string {
	t.Helper()

	registry := prometheus.NewRegistry()
	m := metrics.New(registry)

	// Touched so the label-partitioned families exist. A HistogramVec with no
	// observations reports nothing at all, so without this half the dashboard's
	// metrics would look absent.
	m.ObserveTick("test", 0, 1, 1)
	m.TickOverruns.WithLabelValues("test").Add(0)
	m.RoomEntities.WithLabelValues("test").Set(0)
	m.RoomPlayers.WithLabelValues("test").Set(0)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	names := make([]string, 0, len(families))
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "mmo_") {
			names = append(names, f.GetName())
		}
	}
	slices.Sort(names)
	return names
}

// Every metric the dashboard asks for exists.
func TestDashboardOnlyUsesRealMetrics(t *testing.T) {
	have := exported(t)

	for _, want := range dashboardMetrics(t) {
		if !slices.Contains(have, want) {
			t.Errorf("the dashboard queries %s, which the server does not export.\n"+
				"exported: %s", want, strings.Join(have, ", "))
		}
	}
}

// Every metric the server exports is on the dashboard.
//
// The other direction, and the one that catches a metric added and then
// forgotten -- which is how a system ends up instrumented and unobserved. A
// metric that genuinely does not belong on a dashboard goes in the list below
// with a reason, so the decision is written down rather than implied by
// absence.
func TestEveryMetricIsOnTheDashboard(t *testing.T) {
	// Nothing so far. Add a name and a reason rather than leaving a gap.
	offDashboard := map[string]string{}

	used := dashboardMetrics(t)

	for _, name := range exported(t) {
		if reason, ok := offDashboard[name]; ok {
			if reason == "" {
				t.Errorf("%s is excluded from the dashboard with no reason given", name)
			}
			continue
		}
		if !slices.Contains(used, name) {
			t.Errorf("%s is exported but no panel uses it; "+
				"add a panel, or list it in offDashboard with a reason", name)
		}
	}
}

// A map that has never overrun reports zero overruns, not nothing.
//
// A counter that springs into existence on its first increment reads as "no
// data" until something goes wrong -- which on a dashboard is the same shape as
// a renamed metric or a server nobody is scraping. The one time you need to
// trust it is the one time it has never been touched.
func TestOverrunsReportZeroBeforeAnythingOverruns(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := metrics.New(registry)

	// A comfortably fast tick: nothing overran.
	m.ObserveTick("henesys", time.Millisecond, 10, 2)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}

	for _, f := range families {
		if f.GetName() != "mmo_room_tick_overruns_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetValue() == "henesys" {
					if got := metric.GetCounter().GetValue(); got != 0 {
						t.Errorf("overruns for a healthy map = %v, want 0", got)
					}
					return
				}
			}
		}
	}
	t.Error("a map that ticked without overrunning reports no overrun series at all, " +
		"so a dashboard cannot tell it apart from a metric that is missing")
}
