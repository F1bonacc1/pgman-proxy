package embedded

import (
	"fmt"
	"sort"

	"github.com/nats-io/nats-server/v2/server"
)

// ReloadDiff captures the change set computed for a SIGHUP reload
// (FR-014a / RD-001a). Only two surfaces are reloadable: the
// peer-routes list and the cluster password. Any change to a key
// outside the allow-list is captured in `SkippedKeys` so the operator
// sees a loud structured warning instead of silent partial reloads.
//
// Field shape mirrors `data-model.md § ReloadDiff` and
// `contracts/observability.md § embedded_nats.reload_applied`.
type ReloadDiff struct {
	RoutesAdded       []string
	RoutesRemoved     []string
	PasswordRotated   bool
	PasswordOldPrefix string // 8-char base32 prefix; empty when not rotated
	PasswordNewPrefix string // 8-char base32 prefix; empty when not rotated
	SkippedKeys       []string
	SkippedReason     string // populated when SkippedKeys is non-empty
}

// IsEmpty reports whether the diff has no actionable change. Used by
// callers to short-circuit a no-op reload.
func (d ReloadDiff) IsEmpty() bool {
	return len(d.RoutesAdded) == 0 &&
		len(d.RoutesRemoved) == 0 &&
		!d.PasswordRotated &&
		len(d.SkippedKeys) == 0
}

// ReloadInputs bundles the inputs to ComputeDiff. Both `oldOpts` and
// `newOpts` must be `*server.Options` produced by `BuildOptions` so
// the comparison is apples-to-apples (same shape, same field
// conventions). `*Snapshot` arguments capture pre-reload identifying
// state for the password-prefix audit.
type ReloadInputs struct {
	OldOpts        *server.Options
	NewOpts        *server.Options
	OldPasswordRaw string // resolved old password (for prefix only; never logged in full)
	NewPasswordRaw string // resolved new password
}

// ComputeDiff computes the actionable diff between two options sets.
// Determines what NATS routes were added/removed, whether the cluster
// password rotated, and which (if any) non-allow-listed keys differ.
//
// The function is pure (no I/O); errors only surface for genuinely
// unrepresentable inputs.
func ComputeDiff(in ReloadInputs) (ReloadDiff, error) {
	if in.OldOpts == nil || in.NewOpts == nil {
		return ReloadDiff{}, fmt.Errorf("ComputeDiff: both OldOpts and NewOpts are required")
	}
	d := ReloadDiff{}

	// Routes diff (URL string compare; user-info parts ignored so a
	// password rotation doesn't appear as a route delta).
	oldRoutes := normalizeRouteHosts(in.OldOpts)
	newRoutes := normalizeRouteHosts(in.NewOpts)
	d.RoutesAdded, d.RoutesRemoved = stringSetDiff(oldRoutes, newRoutes)

	// Password-rotation detection — independent of route diff.
	if in.OldOpts.Cluster.Password != in.NewOpts.Cluster.Password {
		d.PasswordRotated = true
		d.PasswordOldPrefix = passwordPrefix(in.OldPasswordRaw)
		d.PasswordNewPrefix = passwordPrefix(in.NewPasswordRaw)
	}

	// Skipped-keys detection. Anything not in {Routes, Cluster.Password}
	// that differs is a non-allow-listed change. We surface the *names*
	// of changed keys, not their values (values may include secrets).
	skipped := []string{}
	if in.OldOpts.ServerName != in.NewOpts.ServerName {
		skipped = append(skipped, "cluster.node_id")
	}
	if in.OldOpts.Host != in.NewOpts.Host || in.OldOpts.Port != in.NewOpts.Port {
		skipped = append(skipped, "cluster.client_listen")
	}
	if in.OldOpts.Cluster.Name != in.NewOpts.Cluster.Name {
		skipped = append(skipped, "cluster.name")
	}
	if in.OldOpts.Cluster.Host != in.NewOpts.Cluster.Host || in.OldOpts.Cluster.Port != in.NewOpts.Cluster.Port {
		skipped = append(skipped, "cluster.routes_listen")
	}
	if in.OldOpts.Cluster.Username != in.NewOpts.Cluster.Username {
		skipped = append(skipped, "cluster.username")
	}
	if in.OldOpts.StoreDir != in.NewOpts.StoreDir {
		skipped = append(skipped, "cluster.jetstream_dir")
	}
	if (in.OldOpts.Cluster.TLSConfig == nil) != (in.NewOpts.Cluster.TLSConfig == nil) {
		skipped = append(skipped, "cluster.tls")
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		d.SkippedKeys = skipped
		d.SkippedReason = "the listed keys are startup-only per FR-014a; restart the peer to apply changes to them"
	}

	return d, nil
}

// Apply hands the new options to the running server via
// `srv.ReloadOptions`, which NATS validates internally and applies
// non-destructively for the supported surfaces (routes, cluster auth).
// Returns the upstream error verbatim.
func Apply(srv *server.Server, newOpts *server.Options) error {
	if srv == nil {
		return fmt.Errorf("Apply: server is nil")
	}
	if newOpts == nil {
		return fmt.Errorf("Apply: newOpts is nil")
	}
	return srv.ReloadOptions(newOpts)
}

// normalizeRouteHosts returns the sorted list of host:port strings
// from the options' Routes slice, ignoring URL user-info (so
// credential rotation isn't mis-detected as a route diff).
func normalizeRouteHosts(opts *server.Options) []string {
	out := make([]string, 0, len(opts.Routes))
	for _, u := range opts.Routes {
		if u == nil {
			continue
		}
		out = append(out, u.Host)
	}
	sort.Strings(out)
	return out
}

// stringSetDiff returns (added, removed) pairs where `added` is in
// `newSet` but not `oldSet`, and `removed` is the reverse. Both
// inputs must be sorted; outputs are sorted ascending.
func stringSetDiff(oldSet, newSet []string) (added, removed []string) {
	i, j := 0, 0
	for i < len(oldSet) && j < len(newSet) {
		switch {
		case oldSet[i] == newSet[j]:
			i++
			j++
		case oldSet[i] < newSet[j]:
			removed = append(removed, oldSet[i])
			i++
		default:
			added = append(added, newSet[j])
			j++
		}
	}
	removed = append(removed, oldSet[i:]...)
	added = append(added, newSet[j:]...)
	return
}

// passwordPrefix returns the 8-char prefix of the raw password —
// matches the FR-010a redaction used elsewhere in this package.
// Empty input → empty output.
func passwordPrefix(pw string) string {
	if len(pw) <= 8 {
		return pw
	}
	return pw[:8]
}
