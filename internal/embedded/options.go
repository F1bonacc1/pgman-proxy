package embedded

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/nats-io/nats-server/v2/server"
)

// OptionsInput bundles the inputs to BuildOptions. It is purposely
// distinct from `internal/config.Config` so this package can be unit-
// tested without dragging in the project-wide config struct (and so
// the options builder is a pure function with explicit dependencies).
type OptionsInput struct {
	NodeID       string // pgman-proxy node ID; becomes NATS ServerName + audit identity
	ClusterName  string // shared cluster name; guards against unrelated clusters meshing
	DeclaredSize int    // 1+ ; drives RD-004 derivation in callers, but also sets routes_listen defaults

	ClientHost string // default 127.0.0.1; loopback-only in HA mode (FR-018)
	ClientPort int    // default 4222

	// RoutesEnabled controls whether the embedded server opens a
	// cluster-routes listener at all. Single-peer deployments may
	// disable it; multi-peer MUST enable it.
	RoutesEnabled bool
	RoutesHost    string // default 0.0.0.0 in HA mode
	RoutesPort    int    // default 6222

	// RoutePeers is the operator-supplied address list of sibling
	// cluster-route listeners (host:port). Self-loops are warned and
	// excluded by callers; this field carries the already-cleaned set.
	RoutePeers []string

	// Credential is the resolved shared cluster username + password
	// (RD-001a). Required when RoutesEnabled and the routes listener is
	// non-loopback (FR-009); MAY be zero-value for pure single-peer.
	Credential ClusterCredential

	// TLS material for the cluster-routes listener. Required when
	// RoutesEnabled and the routes listener is non-loopback unless
	// PlaintextExplicitAck is true (FR-010b).
	TLSCertFile          string
	TLSKeyFile           string
	TLSCAFile            string
	PlaintextExplicitAck bool

	// JetStreamDir is the durable-storage location. Empty enables
	// in-memory JetStream (single-peer convenience only); multi-peer
	// configurations MUST supply a path (caller validates).
	JetStreamDir string
}

// BuildOptions translates the operator's intent into a NATS
// `*server.Options`. It is a pure function: no I/O beyond reading TLS
// material when both filenames are supplied. Returns an error only on
// genuinely-unrepresentable input (e.g., unparseable route URL); all
// configuration-shape errors are caught upstream by validate.go.
func BuildOptions(in OptionsInput) (*server.Options, error) {
	if in.NodeID == "" {
		return nil, errors.New("BuildOptions: NodeID is required (drives ServerName + audit identity)")
	}
	if in.ClusterName == "" {
		return nil, errors.New("BuildOptions: ClusterName is required (cluster guard)")
	}

	opts := &server.Options{
		ServerName: in.NodeID,
		Host:       defaultStr(in.ClientHost, "127.0.0.1"),
		Port:       defaultInt(in.ClientPort, 4222),

		// We own logging via the existing internal/obs logger; suppress
		// nats-server's default stderr/timestamp behaviour.
		NoLog:   true,
		NoSigs:  true,
		Logtime: false,

		JetStream: true,
		StoreDir:  in.JetStreamDir, // empty → in-memory
	}

	if in.RoutesEnabled {
		opts.Cluster = server.ClusterOpts{
			Name:     in.ClusterName,
			Host:     defaultStr(in.RoutesHost, "0.0.0.0"),
			Port:     defaultInt(in.RoutesPort, 6222),
			Username: in.Credential.Username,
			Password: in.Credential.Password,
		}

		// Build TLS config for cluster routes if material is supplied.
		// FR-010b: required on non-loopback unless PlaintextExplicitAck.
		// Validation upstream rejects the missing-TLS case for non-
		// loopback binds; here we only assemble what was supplied.
		if in.TLSCertFile != "" && in.TLSKeyFile != "" {
			tlsCfg, err := buildClusterTLSConfig(in.TLSCertFile, in.TLSKeyFile, in.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("cluster TLS: %w", err)
			}
			opts.Cluster.TLSConfig = tlsCfg
		}

		if len(in.RoutePeers) > 0 {
			urls, err := parseRouteURLs(in.RoutePeers, in.Credential)
			if err != nil {
				return nil, fmt.Errorf("parse route peers: %w", err)
			}
			opts.Routes = urls
		}
	}

	return opts, nil
}

// buildClusterTLSConfig loads the cluster server cert + private key,
// and (if supplied) a CA bundle that the embedded server will use to
// verify sibling-presented certificates on inbound routes. Per RD-007:
// the CA bundle prevents the trivial "any cert is accepted" mode; the
// system trust store is intentionally NOT consulted (cross-trust
// accidents are too easy on a coordination plane).
func buildClusterTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if caFile != "" {
		caBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("ca file %q: no PEM certificates parsed", caFile)
		}
		cfg.ClientCAs = pool
		cfg.RootCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// parseRouteURLs converts host:port (or already-formed nats-route URL)
// strings into the *url.URL form that nats-server expects on
// Options.Routes. The returned URLs embed the cluster credential so
// outbound route connections present the right username/password.
func parseRouteURLs(peers []string, cred ClusterCredential) ([]*url.URL, error) {
	urls := make([]*url.URL, 0, len(peers))
	for _, p := range peers {
		raw := strings.TrimSpace(p)
		if raw == "" {
			continue
		}
		// Accept both "host:port" and "nats-route://host:port" / "nats://host:port" forms.
		if !strings.Contains(raw, "://") {
			raw = "nats-route://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %w", p, err)
		}
		if cred.Username != "" {
			u.User = url.UserPassword(cred.Username, cred.Password)
		}
		urls = append(urls, u)
	}
	return urls, nil
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
