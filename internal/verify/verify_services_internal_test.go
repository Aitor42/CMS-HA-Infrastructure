package verify

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

// mariaDBVerifier builds a Verifier whose database NodePort dial lands on a
// real loopback listener, so checkMariaDB reaches the kubectl-exec retry loop
// without a lab.
func mariaDBVerifier(t *testing.T, fake *testutil.FakeExecutor) (*Verifier, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{{Name: "internal-master1", IP: "127.0.0.1"}},
		},
		// FakeSecret assembles an obviously fake fixture value: the check only
		// strings-quotes it into the kubectl script it would run remotely.
		Database: config.DatabaseConfig{Password: testutil.FakeSecret("database"), Port: port},
	}
	return &Verifier{cfg: cfg, ssh: fake}, func() { _ = ln.Close() }
}

// TestCheckMariaDBRetriesThroughSlowAttempts replays the 2026-11-21 nightly:
// the API server answers too slowly for the per-attempt timeout twice, then
// the cluster heals and the round-trip succeeds. The check must retry through
// the blip instead of failing the verification on it.
func TestCheckMariaDBRetriesThroughSlowAttempts(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	attempts := 0
	fake.RunFunc = func(_ context.Context, _, _ string) (string, string, int, error) {
		attempts++
		if attempts <= 2 {
			// What a starved API server looks like from kubectl: the attempt
			// runs out of time before the pod ever answers.
			return "", "The connection to the server 127.0.0.1:6443 was refused", -1, context.DeadlineExceeded
		}
		return "1\n2\n5\n", "", 0, nil
	}
	v, cleanup := mariaDBVerifier(t, fake)
	defer cleanup()

	o := v.checkMariaDB(context.Background())
	if !o.pass {
		t.Fatalf("checkMariaDB() = %+v, want pass after %d attempts", o, attempts)
	}
	if attempts != 3 {
		t.Errorf("ran %d attempts, want 3 (two slow attempts, then success)", attempts)
	}
	if !strings.Contains(o.detail, "write round-trip ok") {
		t.Errorf("detail %q does not report the write round-trip", o.detail)
	}
}

// TestCheckMariaDBFailsWhenDatabaseIsBroken keeps every attempt failing and
// cancels the phase context after the second one, so the test does not sit
// through the whole retry budget: a database that is really broken must fail
// the check no matter how generous the budget is.
func TestCheckMariaDBFailsWhenDatabaseIsBroken(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	fake.RunFunc = func(_ context.Context, _, _ string) (string, string, int, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return "", "ERROR 1045 (28000): Access denied for user 'wp_user'", 1, nil
	}
	v, cleanup := mariaDBVerifier(t, fake)
	defer cleanup()

	o := v.checkMariaDB(ctx)
	if o.pass {
		t.Fatalf("checkMariaDB() passed with a broken database")
	}
	if !strings.Contains(o.detail, "SQL round-trip failed") {
		t.Errorf("detail %q does not report the failed round-trip", o.detail)
	}
}
