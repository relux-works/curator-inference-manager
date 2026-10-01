//go:build darwin

package infra

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// Producer agreement: the wire sharing validator must agree with the
// producer-owned bounds on every boundary value, and must accept everything
// the producers emit. The catalog family sweep feeds producer-derived sharing
// straight into the reader; the TOML and marker batteries compare producer
// and reader verdicts on identical boundary values.

func TestWireSharingAcceptsEveryCatalogFamily(t *testing.T) {
	stressStarts := []int{1, 600, 3600}
	delays := []int{0, 1, 999, 1000, 60000}
	markerSets := [][]string{{"fatal"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p"}}
	accepted := 0
	for _, engine := range []string{"mlx-lm", "mlx-swift", "llama-cpp"} {
		for _, supervision := range []*EngineCatalogSupervisionPolicy{
			nil,
			{FatalOutputSubstrings: []string{"fatal"}, RestartOnFailure: false, MaxRestarts: 1, RestartWindowSeconds: 1, RestartDelayMilliseconds: 0},
		} {
			for _, startup := range stressStarts {
				entry := EngineCatalogEntry{Engine: engine, Mode: "local", Supervision: supervision}
				runtime := PiRuntime{StartupTimeoutSeconds: startup, ShutdownTimeoutSeconds: engineCatalogShutdownDefault}
				sharing := engineCatalogSharing(entry, runtime)
				if err := validateWireSharing(sharing, runtime); err != nil {
					t.Errorf("engine %s supervision %v startup %d refused: %v", engine, supervision != nil, startup, err)
					continue
				}
				accepted++
			}
		}
		for _, delay := range delays {
			for _, markers := range markerSets {
				for _, maxRestarts := range []int{1, 100} {
					for _, window := range []int{1, 86400} {
						policy := &EngineCatalogSupervisionPolicy{
							FatalOutputSubstrings: markers, RestartOnFailure: true,
							MaxRestarts: maxRestarts, RestartWindowSeconds: window, RestartDelayMilliseconds: delay,
						}
						if err := validateEngineCatalogSupervision(policy); err != nil {
							t.Fatalf("sweep policy invalid: %v", err)
						}
						entry := EngineCatalogEntry{Engine: engine, Mode: "local", Supervision: policy}
						runtime := PiRuntime{StartupTimeoutSeconds: engineCatalogStartupDefault, ShutdownTimeoutSeconds: engineCatalogShutdownDefault}
						sharing := engineCatalogSharing(entry, runtime)
						if err := validateWireSharing(sharing, runtime); err != nil {
							t.Errorf("engine %s delay %d markers %d restarts %d window %d refused: %v",
								engine, delay, len(markers), maxRestarts, window, err)
							continue
						}
						accepted++
					}
				}
			}
		}
	}
	t.Logf("catalog family sweep: %d producer-derived sharings accepted", accepted)
}

// tomlSharingBattery parses a TOML sharing table and the matching wire struct
// at the same boundary value and requires identical verdicts.
func TestWireSharingAgreesWithTOMLProducer(t *testing.T) {
	var document map[string]any
	if err := toml.Unmarshal([]byte(sharedPiProfileTOML("profile", "/bin/echo", 18011)), &document); err != nil {
		t.Fatal(err)
	}
	profile := document["agents"].(map[string]any)["pi"].(map[string]any)["profiles"].(map[string]any)["profile"].(map[string]any)
	runtimeTable := profile["runtime"].(map[string]any)
	runtime := PiRuntime{StartupTimeoutSeconds: 5, ShutdownTimeoutSeconds: 2}
	validTable := runtimeTable["sharing"].(map[string]any)
	validSharing, err := parsePiRuntimeSharing(validTable, "agents.pi.runtime.sharing", runtime)
	if err != nil {
		t.Fatalf("valid TOML sharing refused: %v", err)
	}
	if err := validateWireSharing(validSharing, runtime); err != nil {
		t.Fatalf("valid TOML sharing refused by wire validator: %v", err)
	}
	maxLinger := piSharingMaximumLingerSeconds(runtime.ShutdownTimeoutSeconds)
	cloneTable := func() map[string]any {
		out := make(map[string]any, len(validTable))
		for key, value := range validTable {
			out[key] = value
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		field  string
		value  int64
		accept bool
	}{
		{"linger_at_max", "linger_seconds", maxLinger, true},
		{"linger_above_max", "linger_seconds", maxLinger + 1, false},
		{"linger_zero", "linger_seconds", 0, true},
		// Heartbeat at the duration maximum still refuses on both sides:
		// the heartbeat/stale ordering rules it out (stale caps at half
		// the maximum), which is also what entails a heartbeat upper gate.
		{"heartbeat_at_max", "heartbeat_interval_seconds", maxTimeDurationSeconds, false},
		{"heartbeat_above_max", "heartbeat_interval_seconds", maxTimeDurationSeconds + 1, false},
		{"stale_at_max", "lease_stale_seconds", maxSharedRuntimeLeaseStaleSeconds, true},
		{"stale_above_max", "lease_stale_seconds", maxSharedRuntimeLeaseStaleSeconds + 1, false},
		{"broker_timeout_at_max", "broker_start_timeout_seconds", maxTimeDurationSeconds, true},
		{"broker_timeout_above_max", "broker_start_timeout_seconds", maxTimeDurationSeconds + 1, false},
		{"broker_timeout_at_minimum", "broker_start_timeout_seconds", 37, true},
		{"broker_timeout_below_minimum", "broker_start_timeout_seconds", 36, false},
		{"restart_max_below_timeout", "restart_max_backoff_seconds", 39, true},
		{"restart_max_at_timeout", "restart_max_backoff_seconds", 40, false},
		{"restart_max_above_max", "restart_max_backoff_seconds", maxTimeDurationSeconds + 1, false},
		{"restart_initial_above_max", "restart_initial_backoff_seconds", maxTimeDurationSeconds + 1, false},
		{"stable_at_max", "stable_run_seconds", maxTimeDurationSeconds, true},
		{"stable_above_max", "stable_run_seconds", maxTimeDurationSeconds + 1, false},
		{"quarantine_at_max", "quarantine_seconds", maxTimeDurationSeconds, true},
		{"quarantine_above_max", "quarantine_seconds", maxTimeDurationSeconds + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := cloneTable()
			table[tc.field] = tc.value
			_, parseErr := parsePiRuntimeSharing(table, "agents.pi.runtime.sharing", runtime)
			wire := validSharing
			switch tc.field {
			case "linger_seconds":
				wire.LingerSeconds = int(tc.value)
			case "heartbeat_interval_seconds":
				wire.HeartbeatIntervalSeconds = int(tc.value)
			case "lease_stale_seconds":
				wire.LeaseStaleSeconds = int(tc.value)
			case "broker_start_timeout_seconds":
				wire.BrokerStartTimeoutSeconds = int(tc.value)
			case "restart_max_backoff_seconds":
				wire.RestartMaxBackoffSeconds = int(tc.value)
			case "restart_initial_backoff_seconds":
				wire.RestartInitialBackoffSeconds = int(tc.value)
			case "stable_run_seconds":
				wire.StableRunSeconds = int(tc.value)
			case "quarantine_seconds":
				wire.QuarantineSeconds = int(tc.value)
			}
			wireErr := validateWireSharing(wire, runtime)
			if (parseErr == nil) != tc.accept {
				t.Errorf("producer verdict=%v want accept=%v", parseErr == nil, tc.accept)
			}
			if (wireErr == nil) != tc.accept {
				t.Errorf("wire verdict=%v want accept=%v (%v)", wireErr == nil, tc.accept, wireErr)
			}
		})
	}
}

func TestWirePressureAgreesWithTOMLProducer(t *testing.T) {
	providerTable := func() map[string]any {
		return map[string]any{
			"observation_path": "/observations", "observation_timeout_milliseconds": int64(1000),
			"pressure_threshold_bytes": int64(2000), "recovery_threshold_bytes": int64(1000),
			"eviction_grace_seconds": int64(5),
			"pressure_action":        "refuse-new-drain-idle", "unknown_action": "refuse-new", "busy_action": "observe",
		}
	}
	validPolicy, err := parsePiRuntimeResourcePressure(providerTable(), "agents.pi.runtime.sharing.resource_pressure")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWireResourcePressure(validPolicy); err != nil {
		t.Fatalf("valid TOML pressure refused by wire validator: %v", err)
	}
	t.Run("grace", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			grace  int64
			accept bool
		}{
			{"at_max", maxTimeDurationSeconds, true},
			{"above_max", maxTimeDurationSeconds + 1, false},
			{"zero", 0, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				table := providerTable()
				table["eviction_grace_seconds"] = tc.grace
				_, parseErr := parsePiRuntimeResourcePressure(table, "agents.pi.runtime.sharing.resource_pressure")
				wire := validPolicy
				wire.EvictionGraceSeconds = int(tc.grace)
				wireErr := validateWireResourcePressure(wire)
				if (parseErr == nil) != tc.accept {
					t.Errorf("producer verdict=%v want accept=%v", parseErr == nil, tc.accept)
				}
				if (wireErr == nil) != tc.accept {
					t.Errorf("wire verdict=%v want accept=%v (%v)", wireErr == nil, tc.accept, wireErr)
				}
			})
		}
	})
	t.Run("path", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			path   string
			accept bool
		}{
			{"clean_absolute", "/observations/live", true},
			{"root", "/", true},
			{"relative", "relative", false},
			{"query", "/observations?x=1", false},
			{"fragment", "/observations#x", false},
			{"unclean", "/a/../b", false},
			{"empty", "", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				table := providerTable()
				table["observation_path"] = tc.path
				_, parseErr := parsePiRuntimeResourcePressure(table, "agents.pi.runtime.sharing.resource_pressure")
				wire := validPolicy
				wire.ObservationPath = tc.path
				wireErr := validateWireResourcePressure(wire)
				if (parseErr == nil) != tc.accept {
					t.Errorf("producer verdict=%v want accept=%v", parseErr == nil, tc.accept)
				}
				if (wireErr == nil) != tc.accept {
					t.Errorf("wire verdict=%v want accept=%v (%v)", wireErr == nil, tc.accept, wireErr)
				}
			})
		}
	})
}

func TestWireMarkersAgreeWithCatalogProducer(t *testing.T) {
	runtime := PiRuntime{StartupTimeoutSeconds: 1, ShutdownTimeoutSeconds: 30}
	repeat := func(n int, marker string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = marker
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		markers       []string
		producerValid bool
		wireValid     bool
	}{
		{"single", []string{"fatal"}, true, true},
		{"sixteen", repeat(16, "x"), true, true},
		{"seventeen", repeat(17, "x"), false, false},
		{"empty_marker", []string{""}, false, false},
		{"max_bytes", []string{strings.Repeat("x", 512)}, true, true},
		{"oversize", []string{strings.Repeat("x", 513)}, false, false},
		{"nul", []string{"a\x00b"}, false, false},
		// Union member: a TOML profile omits markers, which the catalog
		// policy rule (1..16 when supervised) never sees. The producer
		// refuses an empty supervised list; the wire accepts omission.
		{"absent_union", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &EngineCatalogSupervisionPolicy{
				FatalOutputSubstrings: tc.markers, RestartOnFailure: true,
				MaxRestarts: 1, RestartWindowSeconds: 1, RestartDelayMilliseconds: 0,
			}
			producerErr := validateEngineCatalogSupervision(policy)
			if (producerErr == nil) != tc.producerValid {
				t.Errorf("producer verdict=%v want valid=%v", producerErr == nil, tc.producerValid)
			}
			sharing := PiRuntimeSharing{
				Mode: "shared", LingerSeconds: 0, MaxLeases: 8,
				MaxSegmentBytes: 1 << 20, MaxSegments: 7,
				HeartbeatIntervalSeconds: 15, LeaseStaleSeconds: 60,
				BrokerStartTimeoutSeconds: 61, RestartLimit: 1,
				RestartInitialBackoffSeconds: 0, RestartMaxBackoffSeconds: 0,
				StableRunSeconds: 60, QuarantineSeconds: 30,
				ResourcePressureMode:  "disabled",
				FatalOutputSubstrings: tc.markers,
			}
			wireErr := validateWireSharing(sharing, runtime)
			if (wireErr == nil) != tc.wireValid {
				t.Errorf("wire verdict=%v want valid=%v (%v)", wireErr == nil, tc.wireValid, wireErr)
			}
		})
	}
}
