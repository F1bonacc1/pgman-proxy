// Supervisor presence detection — feature 003 / US6 (RD-009).
//
// `pgmctl restart --target=proxy` puts the receiving peer in a
// self-terminate flow: respond 200, drain, os.Exit(0). That sequence
// is only safe when a process supervisor will bring the binary back
// up afterward — under bare `./pgman-proxy` invocation we'd just
// vanish.
//
// SupervisorPresence reports whether we believe the binary is
// running under such a supervisor. The detection is best-effort:
// false negatives (we don't recognise the supervisor) cost an
// operator one explicit `--proxy-assume-supervised` override; false
// positives (we mis-classify ourselves as supervised when we
// aren't) cost a stranded cluster, so the heuristics lean
// conservative.

package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// SupervisorPresence is the runtime-detected supervisor classification.
type SupervisorPresence string

const (
	// SupervisorNone means no recognised supervisor was detected.
	// Self-restart is refused unless proxy.assume_supervised=true.
	SupervisorNone SupervisorPresence = "none"

	// SupervisorTini indicates we are running under tini (PID 1 is
	// `tini` or a child of an init system that re-execs the binary
	// on exit). Common in Docker / Kubernetes pods.
	SupervisorTini SupervisorPresence = "tini"

	// SupervisorSystemd means systemd's notify socket is plumbed
	// through to this process (NOTIFY_SOCKET env var set), or
	// our parent is systemd / pid 1 is systemd with auto-restart.
	SupervisorSystemd SupervisorPresence = "systemd"

	// SupervisorProcessCompose means we are managed by f1bonacc1/
	// process-compose, the dev rig. It exports PROCESS_COMPOSE_*
	// env vars to managed processes.
	SupervisorProcessCompose SupervisorPresence = "process-compose"

	// SupervisorKubernetes means we are running inside a Kubernetes
	// pod (KUBERNETES_SERVICE_HOST env var set). The Kubelet
	// restarts containers per the pod's restartPolicy.
	SupervisorKubernetes SupervisorPresence = "kubernetes"

	// SupervisorAssumed is the override path: operator declared
	// `proxy.assume_supervised=true` in config. Used when an
	// unrecognised supervisor is in play.
	SupervisorAssumed SupervisorPresence = "assumed"
)

// DetectSupervisor walks the heuristics in order, returning the
// first match. `assumeSupervised` short-circuits to SupervisorAssumed
// when set, regardless of what the heuristics would have said —
// operator intent wins.
func DetectSupervisor(assumeSupervised bool) SupervisorPresence {
	if assumeSupervised {
		return SupervisorAssumed
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return SupervisorKubernetes
	}
	if os.Getenv("NOTIFY_SOCKET") != "" {
		return SupervisorSystemd
	}
	// process-compose exports a handful of identifying env vars to
	// managed children. PC_PROC_LOG_DIR / PC_PROC_NAME / PC_PROC_PID
	// are the stable surface. Match on any of them.
	for _, k := range []string{"PC_PROC_LOG_DIR", "PC_PROC_NAME", "PC_PROC_PID"} {
		if os.Getenv(k) != "" {
			return SupervisorProcessCompose
		}
	}
	if pid1IsTini() {
		return SupervisorTini
	}
	if pid1IsSystemd() {
		// In LXC/systemd-spawn etc. pid 1 is systemd without
		// NOTIFY_SOCKET being passed to us. Treat as supervised.
		return SupervisorSystemd
	}
	return SupervisorNone
}

// pid1IsTini reads /proc/1/comm and reports whether the binary name
// matches "tini" (or its common variants). Loopback-safe on non-
// Linux: returns false.
func pid1IsTini() bool {
	name := readProcComm(1)
	return name == "tini" || strings.HasSuffix(name, "/tini")
}

// pid1IsSystemd reads /proc/1/comm and reports whether pid 1 is
// systemd. systemd is "systemd" in /proc/N/comm.
func pid1IsSystemd() bool {
	return readProcComm(1) == "systemd"
}

// readProcComm reads /proc/<pid>/comm and trims trailing whitespace.
// Returns "" on any error (including non-Linux platforms where
// /proc doesn't exist).
func readProcComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// itoa is a tiny strconv.Itoa avoid a strconv dep churn in this file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
