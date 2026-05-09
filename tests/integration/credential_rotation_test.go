// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US3 (feature 002 / FR-010a / RD-001a) — single-step cluster
// credential rotation. Replaces the original three-step NKey
// rotation procedure with the simpler shared-credential flow:
// update password on every peer, SIGHUP each, NATS re-handshakes.
//
// Spec coverage:
//   * `routes_meshed=2` MUST stay constant on every peer through
//     the rotation (cluster never loses quorum).
//   * After the rotation, the SIGHUP outcome counter advances by
//     three (one per peer).
//
// The test currently runs in a degraded mode because the compose
// harness uses static env-var-supplied passwords that can't be
// mutated mid-run without container restart. The full credential-
// rotation flow lands when the harness supports a writable secrets
// file mounted into each peer container — tracked as T040
// follow-up.

package integration

import (
	"testing"
)

func TestCredentialRotation_SingleStep(t *testing.T) {
	t.Skip("requires writable secrets file mounted into peer containers (compose harness change pending) — tracked in T040 follow-up")

	// Sketch of the full flow:
	//   1. Generate a new cluster password via `pgman-proxy
	//      cluster-secret-gen` (run on the host).
	//   2. For each peer:
	//        - rewrite /run/pgman-proxy/secrets.env with the new value
	//        - `docker compose exec -T <peer> kill -HUP 1`
	//        - assert `pgman_proxy_embedded_nats_routes_meshed=2` on
	//          all three peers throughout (no quorum loss).
	//   3. Assert each peer's
	//      `pgman_proxy_embedded_nats_sighup_reload_outcomes_total{result="applied"}`
	//      counter incremented exactly once.
	//   4. Inspect the structured log on each peer for
	//      `embedded_nats.reload_applied{password_rotated=true,
	//       password_old_prefix=..., password_new_prefix=...}`.
}
