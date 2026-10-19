// selfTerminator adapts the runtime's shutdown machinery to the
// control package's ProxySelfTerminator interface. The restart
// handler accepts the request, flushes the response, then calls
// SelfTerminate, which logs the lifecycle event and starts the same
// graceful shutdown a SIGTERM does; main exits 0 once the drain ends.
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

// selfTerminator implements control.ProxySelfTerminator. The draining
// happens on main's shutdown path: shutdown cancels the host context,
// exactly as SIGTERM does (proxy-self-terminate.md step 8). Before
// issue #11 it called os.Exit(0) directly, which skipped the drain:
// Postgres was SIGKILLed with the container and the lease was left to
// expire. exitFn remains only as the fallback when no shutdown hook is
// wired.
type selfTerminator struct {
	localNodeID string
	presence    string
	logger      *obs.Logger
	shutdown    func()
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
// event and starts the graceful shutdown. Idempotent: repeat calls log
// but cancelling the host context twice is a no-op.
func (s *selfTerminator) SelfTerminate(_ context.Context, reason string) {
	if s.logger != nil {
		s.logger.Event("proxy.self_restart_initiated", s.localNodeID,
			pgmanager.Field{Key: "reason", Value: reason},
			pgmanager.Field{Key: "supervisor", Value: s.presence})
	}
	if s.shutdown != nil {
		s.shutdown()
		return
	}
	exit := os.Exit
	if p := s.exitFn.Load(); p != nil {
		exit = *p
	}
	exit(0)
}
