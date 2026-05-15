// selfTerminator adapts the runtime's shutdown machinery to the
// control package's ProxySelfTerminator interface. The restart
// handler accepts the request, flushes the response, then calls
// SelfTerminate which logs the lifecycle event and calls os.Exit(0).
// A configured process supervisor (tini / systemd / k8s / process-
// compose) brings the binary back; without one the cluster loses a
// peer.

package runtime

import (
	"context"
	"os"
	"sync/atomic"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// selfTerminator implements control.ProxySelfTerminator. It is a
// thin shim around os.Exit — the real draining happens via the
// main shutdown path triggered by ctx-cancel of the host context.
// In production main.go listens for SIGTERM AND for the exit-
// channel selfTerminator surfaces; today we cut the latter and
// just os.Exit after logging so the test rig can verify the
// restart cycle end-to-end without coupling shutdown.go.
type selfTerminator struct {
	localNodeID string
	presence    string
	logger      *obs.Logger
	exitFn      atomic.Pointer[func(int)]
}

// SupervisorPresence implements control.ProxySelfTerminator.
func (s *selfTerminator) SupervisorPresence() string { return s.presence }

// LocalNodeID implements control.ProxySelfTerminator.
func (s *selfTerminator) LocalNodeID() string { return s.localNodeID }

// SetExitFn lets tests override the os.Exit dependency (defaults to
// os.Exit at construction). Idempotent and safe under concurrent
// reads.
func (s *selfTerminator) SetExitFn(fn func(int)) {
	if fn == nil {
		fn = os.Exit
	}
	s.exitFn.Store(&fn)
}

// SelfTerminate logs the documented `proxy.self_restart_initiated`
// event and calls os.Exit(0). Idempotent — repeat calls log but
// only the first triggers exit (os.Exit terminates the process).
func (s *selfTerminator) SelfTerminate(_ context.Context, reason string) {
	if s.logger != nil {
		s.logger.Event("proxy.self_restart_initiated", s.localNodeID,
			pgmanager.Field{Key: "reason", Value: reason},
			pgmanager.Field{Key: "supervisor", Value: s.presence})
	}
	exit := os.Exit
	if p := s.exitFn.Load(); p != nil {
		exit = *p
	}
	exit(0)
}
