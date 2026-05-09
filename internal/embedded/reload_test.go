package embedded

import (
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// optsForReloadTest builds a minimal *server.Options at a known shape
// so the diff tests can mutate single fields in isolation.
func optsForReloadTest(routeHosts []string, password string) *server.Options {
	urls := make([]*url.URL, 0, len(routeHosts))
	for _, h := range routeHosts {
		u, _ := url.Parse("nats-route://" + h)
		urls = append(urls, u)
	}
	return &server.Options{
		ServerName: "peer-a",
		Host:       "127.0.0.1",
		Port:       4222,
		Cluster: server.ClusterOpts{
			Name:     "demo-cluster",
			Host:     "127.0.0.1",
			Port:     6222,
			Username: "demo-cluster",
			Password: password,
		},
		Routes:   urls,
		StoreDir: "/var/lib/pgman-proxy/jetstream",
	}
}

func TestComputeDiff_NoChange(t *testing.T) {
	old := optsForReloadTest([]string{"peer-b:6222", "peer-c:6222"}, "pw-original-1234")
	new_ := optsForReloadTest([]string{"peer-b:6222", "peer-c:6222"}, "pw-original-1234")
	d, err := ComputeDiff(ReloadInputs{
		OldOpts: old, NewOpts: new_, OldPasswordRaw: "pw-original-1234", NewPasswordRaw: "pw-original-1234",
	})
	if err != nil {
		t.Fatalf("ComputeDiff: %v", err)
	}
	if !d.IsEmpty() {
		t.Errorf("expected empty diff, got: %+v", d)
	}
}

func TestComputeDiff_RouteAdded(t *testing.T) {
	old := optsForReloadTest([]string{"peer-b:6222"}, "pw-original-1234")
	new_ := optsForReloadTest([]string{"peer-b:6222", "peer-c:6222"}, "pw-original-1234")
	d, err := ComputeDiff(ReloadInputs{OldOpts: old, NewOpts: new_, OldPasswordRaw: "pw-original-1234", NewPasswordRaw: "pw-original-1234"})
	if err != nil {
		t.Fatalf("ComputeDiff: %v", err)
	}
	if len(d.RoutesAdded) != 1 || d.RoutesAdded[0] != "peer-c:6222" {
		t.Errorf("RoutesAdded = %v, want [peer-c:6222]", d.RoutesAdded)
	}
	if len(d.RoutesRemoved) != 0 {
		t.Errorf("RoutesRemoved = %v, want []", d.RoutesRemoved)
	}
	if d.PasswordRotated {
		t.Error("PasswordRotated should be false on a route-only diff")
	}
}

func TestComputeDiff_RouteRemoved(t *testing.T) {
	old := optsForReloadTest([]string{"peer-b:6222", "peer-c:6222"}, "pw-original-1234")
	new_ := optsForReloadTest([]string{"peer-b:6222"}, "pw-original-1234")
	d, _ := ComputeDiff(ReloadInputs{OldOpts: old, NewOpts: new_})
	if len(d.RoutesRemoved) != 1 || d.RoutesRemoved[0] != "peer-c:6222" {
		t.Errorf("RoutesRemoved = %v, want [peer-c:6222]", d.RoutesRemoved)
	}
}

func TestComputeDiff_PasswordRotated(t *testing.T) {
	old := optsForReloadTest([]string{"peer-b:6222"}, "pw-original-1234")
	new_ := optsForReloadTest([]string{"peer-b:6222"}, "pw-rotated-abcd")
	d, _ := ComputeDiff(ReloadInputs{
		OldOpts: old, NewOpts: new_,
		OldPasswordRaw: "pw-original-1234",
		NewPasswordRaw: "pw-rotated-abcd",
	})
	if !d.PasswordRotated {
		t.Fatal("PasswordRotated = false, want true")
	}
	if d.PasswordOldPrefix != "pw-origi" {
		t.Errorf("OldPrefix = %q, want %q", d.PasswordOldPrefix, "pw-origi")
	}
	if d.PasswordNewPrefix != "pw-rotat" {
		t.Errorf("NewPrefix = %q, want %q", d.PasswordNewPrefix, "pw-rotat")
	}
}

func TestComputeDiff_SkippedKeys_IneligibleChange(t *testing.T) {
	old := optsForReloadTest([]string{"peer-b:6222"}, "pw-original-1234")
	new_ := optsForReloadTest([]string{"peer-b:6222"}, "pw-original-1234")
	new_.ServerName = "peer-b" // ineligible: cluster.node_id changes
	new_.Cluster.Name = "renamed-cluster"
	new_.Port = 4333

	d, _ := ComputeDiff(ReloadInputs{OldOpts: old, NewOpts: new_})

	wantKeys := []string{"cluster.client_listen", "cluster.name", "cluster.node_id"}
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(d.SkippedKeys, wantKeys) {
		t.Errorf("SkippedKeys = %v, want %v", d.SkippedKeys, wantKeys)
	}
	if d.SkippedReason == "" {
		t.Error("SkippedReason should be set when SkippedKeys is non-empty")
	}
}

func TestComputeDiff_NilOpts(t *testing.T) {
	_, err := ComputeDiff(ReloadInputs{OldOpts: nil, NewOpts: optsForReloadTest(nil, "pw-original-1234")})
	if err == nil {
		t.Error("expected error on nil OldOpts")
	}
}

func TestComputeDiff_RouteCredentialChangeIsNotARouteDiff(t *testing.T) {
	// Embed different credentials in the route URL but same host:port.
	// Should NOT show up as a route diff (host:port is the only signal).
	old := optsForReloadTest([]string{"peer-b:6222"}, "old-pw-1234567")
	new_ := optsForReloadTest([]string{"peer-b:6222"}, "new-pw-7654321")
	if old.Routes[0].User == nil {
		old.Routes[0].User = url.UserPassword("demo-cluster", "old-pw-1234567")
	}
	if new_.Routes[0].User == nil {
		new_.Routes[0].User = url.UserPassword("demo-cluster", "new-pw-7654321")
	}
	d, _ := ComputeDiff(ReloadInputs{
		OldOpts: old, NewOpts: new_,
		OldPasswordRaw: "old-pw-1234567",
		NewPasswordRaw: "new-pw-7654321",
	})
	if len(d.RoutesAdded) != 0 || len(d.RoutesRemoved) != 0 {
		t.Errorf("expected no route diff for credential-only change; got added=%v removed=%v", d.RoutesAdded, d.RoutesRemoved)
	}
	if !d.PasswordRotated {
		t.Error("PasswordRotated should be true on a credential change")
	}
}
