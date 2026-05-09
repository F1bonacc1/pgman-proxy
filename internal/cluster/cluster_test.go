package cluster

import "testing"

func TestClassifyOutcome(t *testing.T) {
	tests := map[string]string{
		"pgmanager.demo.auto_rebootstrap.detected": "delivered",
		"pgmanager.demo.auto_rebootstrap.refused":  "refused",
		"pgmanager.demo.auto_demote.failed":        "failed",
		"pgmanager.demo.divergence.parked":         "delivered",
		"pgmanager.demo.conninfo.reconciled":       "delivered",
	}
	for subject, want := range tests {
		t.Run(subject, func(t *testing.T) {
			if got := classifyOutcome(subject); got != want {
				t.Errorf("classifyOutcome(%q) = %q, want %q", subject, got, want)
			}
		})
	}
}
