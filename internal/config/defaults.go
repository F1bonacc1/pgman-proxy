// Mode-aware listener defaults (US2 / FR-013).
//
// Sidecar deployments share a network namespace with their colocated
// PostgreSQL pod / process. Binding the proxy or the observability
// surface on all-interfaces would expose them to anything reachable
// from outside the pod — defeating the point of running as a sidecar.
//
// Standalone and microservice deployments are network peers in their
// own right; binding all-interfaces is the right default so neighbour
// processes (smoke-test clients, scrape jobs, control-plane callers)
// can reach the listener.
//
// The contract: when DeploymentMode == "sidecar" AND a listener address
// is at an all-interfaces form (`0.0.0.0:<port>`, `[::]:<port>`, bare
// `:<port>`), rewrite it to `127.0.0.1:<port>`. Operator-pinned
// addresses (already-loopback, hostnames, specific IPv4) pass through
// unchanged — explicit beats implicit.

package config

import "strings"

// ApplyModeDefaults rewrites listener addresses according to
// DeploymentMode. Returns the mutated config. Idempotent.
func ApplyModeDefaults(cfg Config) Config {
	if cfg.DeploymentMode != DeploymentModeSidecar {
		return cfg
	}
	cfg.Proxy.ListenAddr = preferLoopbackIfAllInterfaces(cfg.Proxy.ListenAddr)
	cfg.Obs.HealthAddr = preferLoopbackIfAllInterfaces(cfg.Obs.HealthAddr)
	cfg.Obs.MetricsAddr = preferLoopbackIfAllInterfaces(cfg.Obs.MetricsAddr)
	cfg.Control.ListenAddr = preferLoopbackIfAllInterfaces(cfg.Control.ListenAddr)
	return cfg
}

// preferLoopbackIfAllInterfaces rewrites `0.0.0.0:N`, `[::]:N`, and
// bare `:N` to `127.0.0.1:N`. Other forms (already-loopback, hostname,
// non-default IPv4 etc.) pass through unchanged so operator-pinned
// addresses are honoured.
func preferLoopbackIfAllInterfaces(addr string) string {
	if addr == "" {
		return addr
	}
	switch {
	case strings.HasPrefix(addr, "0.0.0.0:"):
		return "127.0.0.1:" + addr[len("0.0.0.0:"):]
	case strings.HasPrefix(addr, "[::]:"):
		return "127.0.0.1:" + addr[len("[::]:"):]
	case strings.HasPrefix(addr, ":"):
		return "127.0.0.1" + addr
	}
	return addr
}
