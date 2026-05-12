// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

// TestPolicyFromConfig_AutoRecoveryWiredThrough is the regression for
// Gap O. The Policy literal in start.go used to drop
// cfg.Policy.AutoDemote and cfg.Policy.AutoRebootstrap on the floor,
// so an operator who set them in YAML or env (e.g.,
// PGMAN_PROXY_POLICY_AUTO_DEMOTE_ENABLED=true) saw no behavioral
// change. The pg-manager reconciler then detected ex-primary
// divergence (DivergenceParkedEvent) but never acted, leaving a
// warm-restarted ex-primary as a parked split-brain that the proxy
// only avoided routing to by accident of leader-election state. This
// test pins the contract: when the AutoRecoveryCfg flags are true,
// policyFromConfig must propagate them into the pgmanager.Policy.
func TestPolicyFromConfig_AutoRecoveryWiredThrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                string
		demoteEnabled       bool
		rebootstrapEnabled  bool
		wantDemoteOn        bool
		wantRebootstrapOn   bool
	}{
		{name: "both off (default)", demoteEnabled: false, rebootstrapEnabled: false, wantDemoteOn: false, wantRebootstrapOn: false},
		{name: "demote on only", demoteEnabled: true, rebootstrapEnabled: false, wantDemoteOn: true, wantRebootstrapOn: false},
		{name: "rebootstrap on only", demoteEnabled: false, rebootstrapEnabled: true, wantDemoteOn: false, wantRebootstrapOn: true},
		{name: "both on", demoteEnabled: true, rebootstrapEnabled: true, wantDemoteOn: true, wantRebootstrapOn: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{
				Policy: config.PolicyConfig{
					AutoDemote:      config.AutoDemoteCfg{Enabled: tc.demoteEnabled},
					AutoRebootstrap: config.AutoRecoveryCfg{Enabled: tc.rebootstrapEnabled},
				},
			}
			got := policyFromConfig(cfg)
			if got.AutoDemote.Enabled != tc.wantDemoteOn {
				t.Errorf("AutoDemote.Enabled = %v, want %v", got.AutoDemote.Enabled, tc.wantDemoteOn)
			}
			if got.AutoRebootstrap.Enabled != tc.wantRebootstrapOn {
				t.Errorf("AutoRebootstrap.Enabled = %v, want %v", got.AutoRebootstrap.Enabled, tc.wantRebootstrapOn)
			}
			// manager.New rejects with ErrConfigInvalid when AutoDemote
			// is enabled but ProbeFailureThreshold is zero — verify the
			// helper applies the documented default so the cluster
			// actually boots.
			if tc.wantDemoteOn && got.AutoDemote.ProbeFailureThreshold <= 0 {
				t.Errorf("AutoDemote.ProbeFailureThreshold = %d, want > 0 when Enabled (manager.New rejects zero)", got.AutoDemote.ProbeFailureThreshold)
			}
			if !tc.wantDemoteOn && got.AutoDemote.ProbeFailureThreshold != 0 {
				t.Errorf("AutoDemote.ProbeFailureThreshold = %d, want 0 when disabled (no implicit defaults)", got.AutoDemote.ProbeFailureThreshold)
			}
		})
	}
}
