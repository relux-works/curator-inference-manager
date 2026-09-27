//go:build darwin

package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This extraction owns the real broker and engine process. The Process-A
// observation graph remains in its existing host; this test keeps the broker
// half of the former integration witness by proving live readiness, an
// independent second lease, release isolation, and crash cleanup against a
// real broker process and a locally built HTTP engine fixture.
func TestSharedRuntimeBrokerReadinessIndependentLeasesAndCrashCleanup(t *testing.T) {
	testRoot, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(testRoot) })
	project := filepath.Join(testRoot, "project")
	home := filepath.Join(testRoot, "home")
	cache := filepath.Join(home, "Library", "Caches")
	for _, directory := range []string{project, cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	runtimeExecutable := buildSharedFakeRuntime(t, testRoot)
	body := validPiProfileWithArgv(t, sharedTestProfileName, runtimeExecutable, port, []string{"serve", "--model", "Model"}, 8)
	body += fmt.Sprintf(`
[agents.pi.profiles.%q.runtime.sharing]
mode = "shared"
linger_seconds = 0
max_leases = 4
max_segment_bytes = 1048576
max_segments = 7
heartbeat_interval_seconds = 1
lease_stale_seconds = 5
restart_limit = 3
restart_initial_backoff_seconds = 1
restart_max_backoff_seconds = 4
stable_run_seconds = 10
quarantine_seconds = 30
broker_start_timeout_seconds = 40
	resource_pressure_mode = "disabled"
`, sharedTestProfileName)
	writePiProjectConfig(t, project, body)
	options := SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: sharedTestProfileName}
	status, err := SharedRuntimeStatusReport(options)
	if err != nil {
		t.Fatal(err)
	}
	if status.Broker.State != "absent" || len(status.Leases) != 0 {
		t.Fatalf("cold status unexpectedly found a broker: %#v", status)
	}
	lease, err := AcquireSharedRuntimeLease(context.Background(), options, "RUN-independent-lease", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	monitor := lease.Monitor()
	released := false
	defer func() {
		if !released {
			lease.Close()
			<-monitor
		}
	}()
	leaseRecord := lease.Record()
	if leaseRecord.EngineIdentity.Name != sharedTestProfileName || leaseRecord.LeaseToken == "" || leaseRecord.LeaseToken == leaseRecord.LeaseID || leaseRecord.LeaseLifetime != "client_process" {
		t.Fatalf("lease omitted additive engine identity or process-lifetime token: %#v", leaseRecord)
	}
	status, err = SharedRuntimeStatusReport(options)
	if err != nil {
		t.Fatal(err)
	}
	if status.Broker.State != "serving" || status.Runtime == nil || status.Runtime.PID != leaseRecord.RuntimePID || len(status.Leases) != 1 || status.EngineIdentity.Name != sharedTestProfileName || status.EngineIdentity.Key == "" {
		t.Fatalf("cold ensure did not start an attested engine and first lease: %#v", status)
	}
	for _, statusLease := range status.Leases {
		if statusLease.LeaseToken != "" {
			t.Fatal("status exposed a bearer lease token")
		}
	}

	peer := startSharedLeaseHelper(t, project, home, "RUN-engine-peer")
	t.Cleanup(func() {
		if peer.command.ProcessState == nil {
			_ = peer.command.Process.Kill()
			_ = peer.command.Wait()
		}
	})
	status, err = SharedRuntimeStatusReport(options)
	if err != nil {
		t.Fatal(err)
	}
	if status.Broker.State != "serving" || status.Runtime == nil || status.Runtime.PID != leaseRecord.RuntimePID || status.Runtime.PID != peer.info.RuntimePID || len(status.Leases) != 2 {
		t.Fatalf("independent lease changed engine identity or was not accounted: %#v", status)
	}

	for _, attack := range []struct {
		name  string
		token string
	}{
		{name: "forged_token", token: "forged-token"},
		{name: "lease_id_is_not_bearer_token", token: leaseRecord.LeaseID},
		{name: "truncated_issued_token_prefix", token: leaseRecord.LeaseToken[:8]},
		{name: "one_char_flipped_full_length_token", token: flipLastTokenCharacter(leaseRecord.LeaseToken)},
	} {
		t.Run("release_refuses_"+attack.name, func(t *testing.T) {
			err := ReleaseSharedRuntimeLease(options, attack.token)
			var shared *SharedRuntimeError
			if !errors.As(err, &shared) || shared.Code != "protocol_violation" {
				t.Fatalf("release of %s returned %v, want protocol_violation", attack.name, err)
			}
		})
	}
	status, err = SharedRuntimeStatusReport(options)
	if err != nil || len(status.Leases) != 2 {
		t.Fatalf("forged token changed lease state: status=%#v err=%v", status, err)
	}
	if err := ReleaseSharedRuntimeLease(options, leaseRecord.LeaseToken); err != nil {
		t.Fatalf("release by issued token: %v", err)
	}
	select {
	case monitorErr := <-monitor:
		if monitorErr != nil {
			t.Fatalf("token release owner notification: %v", monitorErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("token release was not delivered to the process that owns the lease")
	}
	lease.Close()
	released = true
	waitForSharedTest(t, 5*time.Second, func() bool {
		current, statusErr := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: sharedTestProfileName})
		return statusErr == nil && current.Broker.State == "serving" && current.Runtime != nil && current.Runtime.PID == peer.info.RuntimePID && len(current.Leases) == 1
	}, "releasing one lease disturbed the peer lease or engine")

	// Abrupt owner death is the production crash boundary: the broker must
	// reclaim that process-lifetime lease and drain the engine once no owner
	// remains.
	if err := peer.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = peer.command.Wait()
	waitForSharedTest(t, 8*time.Second, func() bool {
		current, statusErr := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: sharedTestProfileName})
		return statusErr == nil && current.Runtime == nil && len(current.Leases) == 0 && current.Broker.State == "absent"
	}, "broker did not clean up after the last lease owner crashed")
}

func flipLastTokenCharacter(token string) string {
	if token == "" {
		return token
	}
	last := len(token) - 1
	replacement := byte('a')
	if token[last] == replacement {
		replacement = 'b'
	}
	return token[:last] + string(replacement)
}

// Production call site: SharedRuntimeStatusReport -> sharedRuntimeRecordStatus
// -> readSharedLeaseMirrors. A stale broker makes status use the on-disk mirror;
// the client-process bearer token must remain private on that fallback path.
func TestSharedRuntimeStatusReportRedactsClientProcessLeaseTokenFromStaleMirror(t *testing.T) {
	project, home, cache, resolved := newSharedIntegrationProfile(t)
	now := time.Now().UTC()
	token := strings.Repeat("a", 64)
	lease := SharedLeaseRecord{
		LeaseID: "0123456789abcdef0123456789abcdef", LeaseToken: token, LeaseLifetime: "client_process",
		EngineIdentity: resolved.EngineIdentity, ClientKey: "test-client", ClientPID: os.Getpid(),
		RuntimeKey: resolved.RuntimeKey, ProfileDigest: resolved.ProfileDigest,
		Endpoint: resolved.Profile.BaseURL, GrantedAt: now, LastHeartbeatAt: now,
	}
	leasePath := filepath.Join(resolved.Paths.LeasesDir, lease.LeaseID+".json")
	if err := writeSharedJSONAtomic(leasePath, lease); err != nil {
		t.Fatal(err)
	}
	if mirror, err := os.ReadFile(leasePath); err != nil || !bytes.Contains(mirror, []byte(token)) {
		t.Fatalf("fixture did not persist the bearer token in its lease mirror: err=%v", err)
	}
	record := SharedBrokerRecord{
		Stage: "serving", State: "serving", ProtocolVersion: SharedRuntimeProtocolVersion,
		Broker: SharedBrokerIdentity{PID: 0}, RuntimeKeyClaimed: resolved.RuntimeKey,
		RuntimeKey: resolved.RuntimeKey, ProfileDigest: resolved.ProfileDigest,
		EngineIdentity: resolved.EngineIdentity, Endpoint: resolved.Profile.BaseURL,
	}
	if err := writeSharedJSONAtomic(resolved.Paths.BrokerState, record); err != nil {
		t.Fatal(err)
	}

	status, err := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{
		ProjectDir: project, HomeDir: home, CacheRoot: cache, Profile: sharedTestProfileName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.Broker.State != "unverified-stale" || len(status.Leases) != 1 {
		t.Fatalf("status did not report the stale broker and its mirror: broker=%#v leases=%#v", status.Broker, status.Leases)
	}
	if status.Leases[0].LeaseToken != "" {
		t.Fatalf("status exposed the client-process bearer token on the stale-mirror path: %#v", status.Leases[0])
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(token)) {
		t.Fatal("serialized stale status contains the client-process bearer token")
	}
}
