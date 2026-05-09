package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// MetricSet bundles every Prometheus metric documented in
// contracts/observability.md. Names and label sets are stable; renames
// or removals are MINOR-version events (Constitution V).
//
// Each metric carries the implicit `cluster_id` and `node_id` labels
// via a wrapping registry (see NewMetrics).
type MetricSet struct {
	Registry *prometheus.Registry

	// Connection metrics.
	ConnectionsOpen        prometheus.Gauge
	ConnectionsAcceptedTot prometheus.Counter
	ConnectionsClosedTot   *prometheus.CounterVec
	ConnectionDuration     prometheus.Histogram

	// Query / latency metrics.
	QueryLatency prometheus.Histogram
	ErrorsTotal  *prometheus.CounterVec

	// Coordination metrics.
	LeadershipState         *prometheus.GaugeVec
	LeaderChangesTotal      prometheus.Counter
	LeaseRenewalFailuresTot prometheus.Counter
	NATSRoundTrip           prometheus.Histogram
	NATSDisconnectsTotal    *prometheus.CounterVec
	CoordinationEventsTotal *prometheus.CounterVec

	// LCM control-plane metrics (FR-021..FR-034).
	LCMRequestsTotal        *prometheus.CounterVec
	LCMRequestLatency       *prometheus.HistogramVec
	LCMEngineLatency        *prometheus.HistogramVec
	LCMInFlight             *prometheus.GaugeVec
	LCMAuditEmitFailuresTot *prometheus.CounterVec
	LCMLeaderRouteTotal     *prometheus.CounterVec
}

// latencyBuckets is the histogram bucket set used for latency-shaped
// metrics. Documented in contracts/observability.md § Histogram buckets.
var latencyBuckets = []float64{
	0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10,
}

// connDurationBuckets is the bucket set for connection-lifetime histograms.
var connDurationBuckets = []float64{0.1, 1, 10, 60, 600, 3600}

// NewMetrics constructs and registers the documented metric set on a
// fresh registry. The registry includes the standard process and Go
// collectors (contracts/observability.md § Process metrics).
func NewMetrics(clusterID, nodeID string) *MetricSet {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	constLabels := prometheus.Labels{
		"cluster_id": clusterID,
		"node_id":    nodeID,
	}

	m := &MetricSet{Registry: reg}

	m.ConnectionsOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgman_proxy_connections_open", Help: "Currently open client connections.",
		ConstLabels: constLabels,
	})
	m.ConnectionsAcceptedTot = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pgman_proxy_connections_accepted_total", Help: "Lifetime accepted client connections.",
		ConstLabels: constLabels,
	})
	m.ConnectionsClosedTot = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_connections_closed_total", Help: "Client connections closed, by reason.",
		ConstLabels: constLabels,
	}, []string{"reason"})
	m.ConnectionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "pgman_proxy_connection_duration_seconds", Help: "Client connection lifetime.",
		Buckets: connDurationBuckets, ConstLabels: constLabels,
	})

	m.QueryLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "pgman_proxy_query_latency_seconds", Help: "Round-trip query latency observed at the proxy hop.",
		Buckets: latencyBuckets, ConstLabels: constLabels,
	})
	m.ErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_errors_total", Help: "PostgreSQL errors forwarded to clients, by SQLSTATE.",
		ConstLabels: constLabels,
	}, []string{"sqlstate", "severity"})

	m.LeadershipState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgman_proxy_leadership_state", Help: "Current leadership state for this peer (1 only on the active state).",
		ConstLabels: constLabels,
	}, []string{"state"})
	m.LeaderChangesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pgman_proxy_leader_changes_total", Help: "Lifetime leader changes observed.",
		ConstLabels: constLabels,
	})
	m.LeaseRenewalFailuresTot = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pgman_proxy_lease_renewal_failures_total", Help: "Lifetime NATS lease-renewal failures.",
		ConstLabels: constLabels,
	})
	m.NATSRoundTrip = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "pgman_proxy_nats_round_trip_seconds", Help: "Server-side ping/pong round-trip latency.",
		Buckets: latencyBuckets, ConstLabels: constLabels,
	})
	m.NATSDisconnectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_nats_disconnects_total", Help: "NATS disconnect events, by reason.",
		ConstLabels: constLabels,
	}, []string{"reason"})
	m.CoordinationEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_coordination_events_total", Help: "pg-manager coordination events received, by subject and outcome.",
		ConstLabels: constLabels,
	}, []string{"subject", "outcome"})

	m.LCMRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_lcm_requests_total", Help: "LCM control-plane requests, by operation and outcome.",
		ConstLabels: constLabels,
	}, []string{"operation", "outcome"})
	m.LCMRequestLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pgman_proxy_lcm_request_latency_seconds", Help: "Total LCM request latency (request received → response written).",
		Buckets: latencyBuckets, ConstLabels: constLabels,
	}, []string{"operation", "outcome"})
	m.LCMEngineLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pgman_proxy_lcm_engine_latency_seconds", Help: "Latency inside the pg-manager engine call portion of an LCM request.",
		Buckets: latencyBuckets, ConstLabels: constLabels,
	}, []string{"operation", "outcome"})
	m.LCMInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgman_proxy_lcm_in_flight", Help: "Currently-running LCM requests, by operation.",
		ConstLabels: constLabels,
	}, []string{"operation"})
	m.LCMAuditEmitFailuresTot = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_lcm_audit_emit_failures_total", Help: "Audit-emit failures (non-zero triggers fail-closed mutating-op rejection).",
		ConstLabels: constLabels,
	}, []string{"sink"})
	m.LCMLeaderRouteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgman_proxy_lcm_leader_route_total", Help: "LCM leader-routing dispositions.",
		ConstLabels: constLabels,
	}, []string{"operation", "disposition"})

	reg.MustRegister(
		m.ConnectionsOpen, m.ConnectionsAcceptedTot, m.ConnectionsClosedTot, m.ConnectionDuration,
		m.QueryLatency, m.ErrorsTotal,
		m.LeadershipState, m.LeaderChangesTotal, m.LeaseRenewalFailuresTot,
		m.NATSRoundTrip, m.NATSDisconnectsTotal, m.CoordinationEventsTotal,
		m.LCMRequestsTotal, m.LCMRequestLatency, m.LCMEngineLatency, m.LCMInFlight,
		m.LCMAuditEmitFailuresTot, m.LCMLeaderRouteTotal,
	)
	return m
}
