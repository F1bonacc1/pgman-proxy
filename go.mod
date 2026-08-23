module github.com/f1bonacc1/pgman-proxy

// Go 1.26.7 minimum — earlier versions hit the stdlib findings
// surfaced by `.github/workflows/govulncheck.yml`: GO-2026-5037
// (crypto/x509) and GO-2026-5039 (net/textproto) fixed in go1.26.4,
// GO-2026-5856 (crypto/tls) fixed in go1.26.5, and the go1.26.6 batch
// — GO-2026-6218 (net/url), GO-2026-6090 (crypto/tls), GO-2026-6089
// (net/http), GO-2026-6088 (encoding/xml), GO-2026-5972
// (encoding/asn1), GO-2026-5026 (net/http via x/net/idna). 1.26.7 is
// the current patch of that line and carries no further advisories.
// Bump in lockstep with new govulncheck stdlib findings. setup-go in
// CI uses this directive as the install version, so a `toolchain`
// line would be redundant.
go 1.26.7

// The wrapped pg-manager engine is pinned to a tagged release. pg-manager is
// a PRIVATE repo, so every build fetches it with GOPRIVATE + token auth:
// local, CI, and GoReleaser via a git credential on the runner; the bundled
// image via a BuildKit secret mount (see .github/workflows/{ci,govulncheck,
// release,release-image}.yml and deploy/docker/Dockerfile.bundle). The
// credential is never persisted to an image layer, exported cache, or
// provenance attestation.
//
// COUPLED BUMP: pg-manager's own go.mod pins github.com/jackc/pgx/v5,
// and MVS raises this module's pgx to match. v0.6.0 requires pgx
// v5.10.0, which is what the require block below carries. A pg-manager
// bump that moves pgx must raise it here in the SAME commit, or the
// integration image fails to build with "missing go.sum entry for
// ... pgx/v5 (imported by
// github.com/f1bonacc1/pg-manager/internal/pgproto)". That build applies
// a `replace` onto the sibling checkout (tests/integration/Dockerfile),
// so it resolves pg-manager's requirements against THIS module's go.sum.
require (
	github.com/f1bonacc1/pg-manager v0.6.0
	github.com/fatih/color v1.19.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/mattn/go-isatty v0.0.22
	github.com/nats-io/nats-server/v2 v2.14.0
	github.com/nats-io/nats.go v1.51.0
	github.com/oklog/ulid/v2 v2.1.1
	github.com/prometheus/client_golang v1.23.2
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)
