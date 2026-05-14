// Package pgmctl_integration holds integration tests for the pgmctl
// CLI against a real 3-peer pgman-proxy fixture cluster, wired via
// process-compose (see ../../process-compose.yaml).
//
// Tests are gated by build tag `integration` and run via:
//
//	make integration
package pgmctl_integration
