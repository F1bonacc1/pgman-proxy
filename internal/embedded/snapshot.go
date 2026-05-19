package embedded

import "time"

// StatusSnapshot is the on-demand view of the embedded NATS server's
// state surfaced through the control-plane `Status` LCM operation
// (contracts/observability.md § Status response). The shape is stable
// — renames are MINOR-version events for the project (Constitution V).
type StatusSnapshot struct {
	ServerName            string     `json:"server_name"`
	Ready                 bool       `json:"ready"`
	ClientListenAddr      string     `json:"client_listen_addr"`
	RoutesListenAddr      string     `json:"routes_listen_addr,omitempty"`
	TLSEnabled            bool       `json:"tls_enabled"`
	RoutesMeshed          int        `json:"routes_meshed"`
	ReplicasFactor        int        `json:"replicas_factor"`
	ReplicasOverridden    bool       `json:"replicas_overridden"`
	JetStreamStorageBytes int64      `json:"jetstream_storage_bytes"`
	StorageDegraded       *string    `json:"storage_degraded"`
	LastRouteUpAt         *time.Time `json:"last_route_up_at"`
	LastRouteDownAt       *time.Time `json:"last_route_down_at"`
	LastReloadAt          *time.Time `json:"last_reload_at"`
}

// Snapshot captures the current observable state of the embedded NATS
// server. Safe to call after Start() returns; returns a zero-value
// snapshot before that.
//
// Several fields (StorageDegraded, LastRouteUpAt, etc.) are not yet
// wired by this package and remain nil until a full event-hook
// integration lands (US3 follow-up). The schema is stable so the
// control-plane Status response can already include the sub-block.
func (s *Server) Snapshot() StatusSnapshot {
	if s == nil || s.srv == nil {
		return StatusSnapshot{}
	}
	snap := StatusSnapshot{
		ServerName:       s.srv.Name(),
		Ready:            s.readyClosed.Load(),
		ClientListenAddr: s.srv.ClientURL(),
		// Deduplicated peer count — matches the mesh-readiness gate
		// (WaitForRouteMesh) so the gauge agrees with the boot
		// criterion. Raw s.srv.NumRoutes() overcounts because nats-server
		// opens two TCP connections per peer pair during mesh formation.
		RoutesMeshed: s.NumUniqueRoutes(),
	}
	if addr := s.srv.ClusterAddr(); addr != nil {
		snap.RoutesListenAddr = addr.String()
	}
	return snap
}
