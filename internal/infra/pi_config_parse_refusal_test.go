//go:build !windows

package infra

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func piRefusalField(err error) string {
	var configurationError *ProjectConfigurationError
	if errors.As(err, &configurationError) {
		return configurationError.Field
	}
	var fieldErr *piFieldError
	if errors.As(err, &fieldErr) {
		return fieldErr.field
	}
	return ""
}

func TestParsePiProfileRejectsEveryMalformedField(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "model-harness.toml")
	if err := os.WriteFile(configurationPath, []byte("[profiles.qwen]\nmode = \"local\"\nargv = [\"--max-kv-size\", \"4096\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown profile key", func(p map[string]any) { p["future"] = true }},
		{"provider missing", func(p map[string]any) { delete(p, "provider") }},
		{"provider invalid", func(p map[string]any) { p["provider"] = "bad/provider" }},
		{"publisher missing", func(p map[string]any) { delete(p, "publisher") }},
		{"family missing", func(p map[string]any) { delete(p, "family") }},
		{"model missing", func(p map[string]any) { delete(p, "model") }},
		{"base url missing", func(p map[string]any) { delete(p, "base_url") }},
		{"base url invalid", func(p map[string]any) { p["base_url"] = "https://example.test/v1" }},
		{"api mismatch", func(p map[string]any) { p["api"] = "chat-completions" }},
		{"reasoning wrong type", func(p map[string]any) { p["reasoning"] = "false" }},
		{"input wrong type", func(p map[string]any) { p["input"] = "text" }},
		{"input mismatch", func(p map[string]any) { p["input"] = []any{"image"} }},
		{"context window invalid", func(p map[string]any) { p["context_window"] = int64(0) }},
		{"max tokens invalid", func(p map[string]any) { p["max_tokens"] = "many" }},
		{"max tokens exceeds context", func(p map[string]any) { p["max_tokens"] = int64(8193) }},
		{"cache budget invalid", func(p map[string]any) { p["cache_budget_bytes"] = int64(-1) }},
		{"thinking missing", func(p map[string]any) { delete(p, "thinking") }},
		{"thinking undocumented", func(p map[string]any) { p["thinking"] = "turbo" }},
		{"reasoning inconsistent with thinking", func(p map[string]any) { p["reasoning"] = false; p["thinking"] = "high" }},
		{"capabilities wrong type", func(p map[string]any) { p["requested_capabilities"] = "text" }},
		{"capabilities mismatch", func(p map[string]any) { p["requested_capabilities"] = []any{"image"} }},
		{"compaction wrong type", func(p map[string]any) { p["compaction"] = "enabled" }},
		{"compaction invalid", func(p map[string]any) { p["compaction"] = map[string]any{"enabled": true} }},
		{"retention missing", func(p map[string]any) { delete(p, "lifecycle_log_retention") }},
		{"retention wrong type", func(p map[string]any) { p["lifecycle_log_retention"] = "required" }},
		{"retention invalid", func(p map[string]any) { p["lifecycle_log_retention"] = map[string]any{} }},
		{"compat missing", func(p map[string]any) { delete(p, "compat") }},
		{"compat wrong type", func(p map[string]any) { p["compat"] = "table" }},
		{"compat invalid", func(p map[string]any) { p["compat"] = map[string]any{"supports_developer_role": "yes"} }},
		{"runtime missing", func(p map[string]any) { delete(p, "runtime") }},
		{"runtime wrong type", func(p map[string]any) { p["runtime"] = "table" }},
		{"runtime invalid", func(p map[string]any) { p["runtime"] = map[string]any{} }},
		{"runtime endpoint mismatch", func(p map[string]any) {
			piTestMap(t, p, "runtime")["argv"] = []any{"serve", "--model", "Model", "--host", "127.0.0.2", "--port", "18011"}
		}},
		{"legacy model harness kv bound too small", func(p map[string]any) {
			r := piTestMap(t, p, "runtime")
			r["executable"] = "/tmp/model-harness"
			r["argv"] = []any{"run", "qwen", "--config", configurationPath, "--model", "Model", "--host", "127.0.0.1", "--port", "18011"}
		}},
		{"cache budget missing argv binding", func(p map[string]any) { p["cache_budget_bytes"] = int64(4096) }},
		{"dflash requested capabilities mismatch", func(p map[string]any) {
			profile := piProfileTable(t, true)
			profile["requested_capabilities"] = []any{"text", "tools"}
			for key, value := range profile {
				p[key] = value
			}
		}},
		{"dflash target model mismatch", func(p map[string]any) {
			profile := piProfileTable(t, true)
			p["model"] = "Other"
			for key, value := range profile {
				if key != "model" {
					p[key] = value
				}
			}
		}},
	}
	wantFields := map[string]string{
		"unknown profile key":                     "agents.pi.profiles.profile.future",
		"provider missing":                        "agents.pi.profiles.profile.provider",
		"provider invalid":                        "agents.pi.profiles.profile.provider",
		"publisher missing":                       "agents.pi.profiles.profile.publisher",
		"family missing":                          "agents.pi.profiles.profile.family",
		"model missing":                           "agents.pi.profiles.profile.model",
		"base url missing":                        "agents.pi.profiles.profile.base_url",
		"base url invalid":                        "agents.pi.profiles.profile.base_url",
		"api mismatch":                            "agents.pi.profiles.profile.api",
		"reasoning wrong type":                    "agents.pi.profiles.profile.reasoning",
		"input wrong type":                        "agents.pi.profiles.profile.input",
		"input mismatch":                          "agents.pi.profiles.profile.input",
		"context window invalid":                  "agents.pi.profiles.profile.context_window",
		"max tokens invalid":                      "agents.pi.profiles.profile.max_tokens",
		"max tokens exceeds context":              "agents.pi.profiles.profile.max_tokens",
		"cache budget invalid":                    "agents.pi.profiles.profile.cache_budget_bytes",
		"thinking missing":                        "agents.pi.profiles.profile.thinking",
		"thinking undocumented":                   "agents.pi.profiles.profile.thinking",
		"reasoning inconsistent with thinking":    "agents.pi.profiles.profile.reasoning",
		"capabilities wrong type":                 "agents.pi.profiles.profile.requested_capabilities",
		"capabilities mismatch":                   "agents.pi.profiles.profile.requested_capabilities",
		"compaction wrong type":                   "agents.pi.profiles.profile.compaction",
		"compaction invalid":                      "agents.pi.profiles.profile.compaction",
		"retention missing":                       "agents.pi.profiles.profile.lifecycle_log_retention",
		"retention wrong type":                    "agents.pi.profiles.profile.lifecycle_log_retention",
		"retention invalid":                       "agents.pi.profiles.profile.lifecycle_log_retention.max_count",
		"compat missing":                          "agents.pi.profiles.profile.compat",
		"compat wrong type":                       "agents.pi.profiles.profile.compat",
		"compat invalid":                          "agents.pi.profiles.profile.compat.supports_developer_role",
		"runtime missing":                         "agents.pi.profiles.profile.runtime",
		"runtime wrong type":                      "agents.pi.profiles.profile.runtime",
		"runtime invalid":                         "agents.pi.profiles.profile.runtime.executable",
		"runtime endpoint mismatch":               "agents.pi.profiles.profile.runtime.argv",
		"legacy model harness kv bound too small": "agents.pi.profiles.profile.runtime.argv",
		"cache budget missing argv binding":       "agents.pi.profiles.profile.runtime.argv",
		"dflash requested capabilities mismatch":  "agents.pi.profiles.profile.requested_capabilities",
		"dflash target model mismatch":            "agents.pi.profiles.profile.runtime.dflash.target_model",
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := piProfileTable(t, false)
			testCase.mutate(profile)
			_, err := parsePiProfile(profile, "fixture.toml", "profile")
			var configurationError *ProjectConfigurationError
			if !errors.As(err, &configurationError) {
				t.Fatalf("malformed profile was not refused as ProjectConfigurationError: %v", err)
			}
			if got, want := piRefusalField(err), wantFields[testCase.name]; got != want {
				t.Fatalf("malformed profile refused at field %q, want %q: %v", got, want, err)
			}
			if testCase.name == "provider missing" || testCase.name == "base url missing" {
				if configurationError.Err.Error() != "required field is absent" {
					t.Fatalf("missing required field reported downstream instead of at requiredString: %v", configurationError.Err)
				}
			}
		})
	}
}

func TestParsePiConfigRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"Pi table wrong type", func(agents map[string]any) { agents["pi"] = "table" }},
		{"unknown Pi field", func(agents map[string]any) { piTestMap(t, agents, "pi")["future"] = true }},
		{"primary session wrong type", func(agents map[string]any) { piTestMap(t, agents, "pi")["primary_session"] = "table" }},
		{"unknown primary field", func(agents map[string]any) {
			piTestMap(t, piTestMap(t, agents, "pi"), "primary_session")["future"] = true
		}},
		{"primary profile wrong type", func(agents map[string]any) {
			piTestMap(t, piTestMap(t, agents, "pi"), "primary_session")["profile"] = 7
		}},
		{"primary profile empty", func(agents map[string]any) {
			piTestMap(t, piTestMap(t, agents, "pi"), "primary_session")["profile"] = ""
		}},
		{"primary compatibility wrong type", func(agents map[string]any) {
			piTestMap(t, piTestMap(t, agents, "pi"), "primary_session")["pi_compatibility"] = 7
		}},
		{"primary yolo wrong type", func(agents map[string]any) {
			piTestMap(t, piTestMap(t, agents, "pi"), "primary_session")["yolo_mode"] = "true"
		}},
		{"primary session empty", func(agents map[string]any) { piTestMap(t, agents, "pi")["primary_session"] = map[string]any{} }},
		{"standalone session wrong type", func(agents map[string]any) { piTestMap(t, agents, "pi")["standalone_session"] = "table" }},
		{"unknown standalone field", func(agents map[string]any) {
			piTestMap(t, agents, "pi")["standalone_session"] = map[string]any{"yolo_mode": true, "future": true}
		}},
		{"standalone yolo wrong type", func(agents map[string]any) {
			piTestMap(t, agents, "pi")["standalone_session"] = map[string]any{"yolo_mode": "true", "tool_allowlist": []any{"read"}}
		}},
		{"standalone allowlist wrong type", func(agents map[string]any) {
			piTestMap(t, agents, "pi")["standalone_session"] = map[string]any{"yolo_mode": true, "tool_allowlist": "read"}
		}},
		{"standalone session empty", func(agents map[string]any) { piTestMap(t, agents, "pi")["standalone_session"] = map[string]any{} }},
		{"profiles table wrong type", func(agents map[string]any) { piTestMap(t, agents, "pi")["profiles"] = "table" }},
		{"empty profile name", func(agents map[string]any) {
			piTestMap(t, agents, "pi")["profiles"] = map[string]any{"": map[string]any{}}
		}},
		{"profile table wrong type", func(agents map[string]any) { piTestMap(t, agents, "pi")["profiles"] = map[string]any{"bad": "profile"} }},
		{"profile malformed", func(agents map[string]any) {
			delete(piTestMap(t, piTestMap(t, agents, "pi"), "profiles")["profile"].(map[string]any), "publisher")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agents := piConfigAgents(t)
			testCase.mutate(agents)
			_, _, _, err := parsePiConfig(agents, "fixture.toml")
			var configurationError *ProjectConfigurationError
			if !errors.As(err, &configurationError) {
				t.Fatalf("malformed Pi configuration was not refused as ProjectConfigurationError: %v", err)
			}
			if testCase.name == "empty profile name" && configurationError.Err.Error() != "profile name must not be empty" {
				t.Fatalf("empty profile name was rejected downstream instead of at its validator: %v", configurationError.Err)
			}
			if testCase.name == "profile table wrong type" && !strings.Contains(configurationError.Err.Error(), "expected table, got string") {
				t.Fatalf("profile table type was rejected downstream instead of at its type check: %v", configurationError.Err)
			}
		})
	}
}

func TestParsePiCompactionRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown key", func(table map[string]any) { table["future"] = true }},
		{"enabled wrong type", func(table map[string]any) { table["enabled"] = "yes" }},
		{"threshold absent", func(table map[string]any) { delete(table, "compact_at_tokens") }},
		{"two thresholds", func(table map[string]any) { table["reserve_tokens"] = 2048 }},
		{"compact threshold invalid", func(table map[string]any) { table["compact_at_tokens"] = int64(0) }},
		{"compact threshold reaches context", func(table map[string]any) { table["compact_at_tokens"] = int64(8192) }},
		{"reserve threshold invalid", func(table map[string]any) { delete(table, "compact_at_tokens"); table["reserve_tokens"] = int64(0) }},
		{"reserve reaches context", func(table map[string]any) { delete(table, "compact_at_tokens"); table["reserve_tokens"] = int64(8192) }},
		{"keep recent invalid", func(table map[string]any) { table["keep_recent_tokens"] = int64(0) }},
		{"reserve below max tokens", func(table map[string]any) { table["compact_at_tokens"] = int64(7600) }},
		{"keep recent reaches threshold", func(table map[string]any) {
			table["compact_at_tokens"] = int64(6000)
			table["keep_recent_tokens"] = int64(6000)
		}},
	}
	wantFields := map[string]string{
		"unknown key":                       "agents.pi.compaction.future",
		"enabled wrong type":                "agents.pi.compaction.enabled",
		"threshold absent":                  "agents.pi.compaction",
		"two thresholds":                    "agents.pi.compaction",
		"compact threshold invalid":         "agents.pi.compaction.compact_at_tokens",
		"compact threshold reaches context": "agents.pi.compaction.compact_at_tokens",
		"reserve threshold invalid":         "agents.pi.compaction.reserve_tokens",
		"reserve reaches context":           "agents.pi.compaction.reserve_tokens",
		"keep recent invalid":               "agents.pi.compaction.keep_recent_tokens",
		"reserve below max tokens":          "agents.pi.compaction.compact_at_tokens",
		"keep recent reaches threshold":     "agents.pi.compaction.keep_recent_tokens",
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := map[string]any{"enabled": true, "compact_at_tokens": int64(6000), "keep_recent_tokens": int64(200)}
			testCase.mutate(table)
			_, err := parsePiCompaction(table, "agents.pi.compaction", 8192, 1024)
			if err == nil {
				t.Fatal("malformed compaction was accepted")
			}
			if got, want := piRefusalField(err), wantFields[testCase.name]; got != want {
				t.Fatalf("compaction refused at field %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestParsePiLifecycleLogRetentionRejectsEveryMalformedField(t *testing.T) {
	maxInt := int64(^uint(0) >> 1)
	cases := []struct {
		name   string
		field  string
		mutate func(map[string]any)
	}{
		{"unknown field", "agents.pi.profiles.profile.lifecycle_log_retention.future", func(table map[string]any) { table["future"] = true }},
		{"required positive member", "agents.pi.profiles.profile.lifecycle_log_retention.max_count", func(table map[string]any) { table["max_count"] = int64(0) }},
		{"timeout duration bound", "agents.pi.profiles.profile.lifecycle_log_retention.create_timeout_seconds", func(table map[string]any) { table["create_timeout_seconds"] = maxTimeDurationSeconds + 1 }},
		{"envelope arithmetic overflow", "agents.pi.profiles.profile.lifecycle_log_retention.max_envelope_bytes", func(table map[string]any) {
			table["max_count"] = int64(1)
			table["max_bytes"] = maxInt
			table["max_envelope_bytes"] = maxInt
		}},
		{"envelope lower bound", "agents.pi.profiles.profile.lifecycle_log_retention.max_envelope_bytes", func(table map[string]any) { table["max_envelope_bytes"] = int64(1) }},
		{"managed slot arithmetic overflow", "agents.pi.profiles.profile.lifecycle_log_retention.max_mutations_per_operation", func(table map[string]any) { table["max_mutations_per_operation"] = maxInt }},
		{"scan entry arithmetic overflow", "agents.pi.profiles.profile.lifecycle_log_retention.max_scan_entries", func(table map[string]any) {
			table["max_mutations_per_operation"] = maxInt / 2
			table["max_scan_entries"] = maxInt
		}},
		{"scan entries lower bound", "agents.pi.profiles.profile.lifecycle_log_retention.max_scan_entries", func(table map[string]any) { table["max_scan_entries"] = int64(1) }},
		{"control document arithmetic overflow", "agents.pi.profiles.profile.lifecycle_log_retention.max_scan_control_bytes", func(table map[string]any) {
			table["max_mutations_per_operation"] = maxInt/4096 + 1
			table["max_scan_entries"] = maxInt
			table["max_scan_control_bytes"] = maxInt
		}},
		{"control bytes lower bound", "agents.pi.profiles.profile.lifecycle_log_retention.max_scan_control_bytes", func(table map[string]any) { table["max_scan_control_bytes"] = int64(1) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := validPiLifecycleLogRetentionTable()
			testCase.mutate(table)
			if _, err := parsePiLifecycleLogRetention(table, "agents.pi.profiles.profile.lifecycle_log_retention"); err == nil {
				t.Fatal("malformed lifecycle retention policy was accepted")
			} else if got := piRefusalField(err); got != testCase.field {
				t.Fatalf("retention refused at field %q, want %q: %v", got, testCase.field, err)
			}
		})
	}
}

func validPiLifecycleLogRetentionTable() map[string]any {
	return map[string]any{
		"max_count": int64(8), "max_bytes": int64(1048576), "max_envelope_bytes": int64(2097152),
		"max_age_seconds": int64(4838400), "create_timeout_seconds": int64(5), "append_timeout_seconds": int64(5),
		"close_timeout_seconds": int64(5), "status_timeout_seconds": int64(5), "maintenance_timeout_seconds": int64(5),
		"max_scan_entries": int64(512), "max_scan_control_bytes": int64(262144), "max_mutations_per_operation": int64(8),
	}
}

func TestParsePiRuntimeSharingRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		mutate func(map[string]any, *PiRuntime)
	}{
		{"unknown field", "agents.pi.runtime.sharing.future", func(table map[string]any, _ *PiRuntime) { table["future"] = true }},
		{"mode missing", "agents.pi.runtime.sharing.mode", func(table map[string]any, _ *PiRuntime) { delete(table, "mode") }},
		{"mode wrong type", "agents.pi.runtime.sharing.mode", func(table map[string]any, _ *PiRuntime) { table["mode"] = true }},
		{"mode unsupported", "agents.pi.runtime.sharing.mode", func(table map[string]any, _ *PiRuntime) { table["mode"] = "pooled" }},
		{"linger missing", "agents.pi.runtime.sharing.linger_seconds", func(table map[string]any, _ *PiRuntime) { delete(table, "linger_seconds") }},
		{"linger negative", "agents.pi.runtime.sharing.linger_seconds", func(table map[string]any, _ *PiRuntime) { table["linger_seconds"] = int64(-1) }},
		{"linger exceeds remaining duration", "agents.pi.runtime.sharing.linger_seconds", func(table map[string]any, _ *PiRuntime) { table["linger_seconds"] = maxTimeDurationSeconds - 2 }},
		{"no room for shutdown and handoff", "agents.pi.runtime.sharing.linger_seconds", func(_ map[string]any, runtime *PiRuntime) {
			runtime.ShutdownTimeoutSeconds = int(maxTimeDurationSeconds)
		}},
		{"max leases invalid", "agents.pi.runtime.sharing.max_leases", func(table map[string]any, _ *PiRuntime) { table["max_leases"] = int64(0) }},
		{"max segment bytes invalid", "agents.pi.runtime.sharing.max_segment_bytes", func(table map[string]any, _ *PiRuntime) { table["max_segment_bytes"] = int64(0) }},
		{"max segments invalid", "agents.pi.runtime.sharing.max_segments", func(table map[string]any, _ *PiRuntime) { table["max_segments"] = int64(0) }},
		{"heartbeat invalid", "agents.pi.runtime.sharing.heartbeat_interval_seconds", func(table map[string]any, _ *PiRuntime) { table["heartbeat_interval_seconds"] = int64(0) }},
		{"heartbeat reaches stale threshold", "agents.pi.runtime.sharing.heartbeat_interval_seconds", func(table map[string]any, _ *PiRuntime) { table["heartbeat_interval_seconds"] = int64(10) }},
		{"lease stale invalid", "agents.pi.runtime.sharing.lease_stale_seconds", func(table map[string]any, _ *PiRuntime) { table["lease_stale_seconds"] = int64(0) }},
		{"broker start timeout too short", "agents.pi.runtime.sharing.broker_start_timeout_seconds", func(table map[string]any, _ *PiRuntime) { table["broker_start_timeout_seconds"] = int64(36) }},
		{"restart limit invalid", "agents.pi.runtime.sharing.restart_limit", func(table map[string]any, _ *PiRuntime) { table["restart_limit"] = int64(0) }},
		{"restart initial backoff invalid", "agents.pi.runtime.sharing.restart_initial_backoff_seconds", func(table map[string]any, _ *PiRuntime) { table["restart_initial_backoff_seconds"] = int64(0) }},
		{"restart max backoff invalid", "agents.pi.runtime.sharing.restart_max_backoff_seconds", func(table map[string]any, _ *PiRuntime) { table["restart_max_backoff_seconds"] = int64(0) }},
		{"restart max below initial", "agents.pi.runtime.sharing.restart_max_backoff_seconds", func(table map[string]any, _ *PiRuntime) {
			table["restart_initial_backoff_seconds"] = int64(5)
			table["restart_max_backoff_seconds"] = int64(4)
		}},
		{"restart max reaches broker start timeout", "agents.pi.runtime.sharing.restart_max_backoff_seconds", func(table map[string]any, _ *PiRuntime) { table["restart_max_backoff_seconds"] = int64(40) }},
		{"stable run invalid", "agents.pi.runtime.sharing.stable_run_seconds", func(table map[string]any, _ *PiRuntime) { table["stable_run_seconds"] = int64(0) }},
		{"quarantine invalid", "agents.pi.runtime.sharing.quarantine_seconds", func(table map[string]any, _ *PiRuntime) { table["quarantine_seconds"] = int64(0) }},
		{"resource mode missing", "agents.pi.runtime.sharing.resource_pressure_mode", func(table map[string]any, _ *PiRuntime) { delete(table, "resource_pressure_mode") }},
		{"resource mode unsupported", "agents.pi.runtime.sharing.resource_pressure_mode", func(table map[string]any, _ *PiRuntime) { table["resource_pressure_mode"] = "always" }},
		{"provider resource missing", "agents.pi.runtime.sharing.resource_pressure", func(table map[string]any, _ *PiRuntime) { table["resource_pressure_mode"] = "provider" }},
		{"disabled resource present", "agents.pi.runtime.sharing.resource_pressure", func(table map[string]any, _ *PiRuntime) { table["resource_pressure"] = map[string]any{} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := validPiRuntimeSharingTable()
			runtime := PiRuntime{Argv: []string{"serve"}, StartupTimeoutSeconds: 5, ShutdownTimeoutSeconds: 2}
			testCase.mutate(table, &runtime)
			if _, err := parsePiRuntimeSharing(table, "agents.pi.runtime.sharing", runtime); err == nil {
				t.Fatal("malformed shared runtime policy was accepted")
			} else if got := piRefusalField(err); got != testCase.field {
				t.Fatalf("sharing refused at field %q, want %q: %v", got, testCase.field, err)
			}
		})
	}
}

func TestParsePiRuntimeResourcePressureRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		mutate func(map[string]any)
	}{
		{"unknown field", "agents.pi.runtime.sharing.resource_pressure.future", func(table map[string]any) { table["future"] = true }},
		{"observation path missing", "agents.pi.runtime.sharing.resource_pressure.observation_path", func(table map[string]any) { delete(table, "observation_path") }},
		{"observation path relative", "agents.pi.runtime.sharing.resource_pressure.observation_path", func(table map[string]any) { table["observation_path"] = "metrics" }},
		{"observation path query", "agents.pi.runtime.sharing.resource_pressure.observation_path", func(table map[string]any) { table["observation_path"] = "/metrics?key=x" }},
		{"observation path unclean", "agents.pi.runtime.sharing.resource_pressure.observation_path", func(table map[string]any) { table["observation_path"] = "/models/../metrics" }},
		{"observation timeout missing", "agents.pi.runtime.sharing.resource_pressure.observation_timeout_milliseconds", func(table map[string]any) { delete(table, "observation_timeout_milliseconds") }},
		{"observation timeout too large", "agents.pi.runtime.sharing.resource_pressure.observation_timeout_milliseconds", func(table map[string]any) {
			table["observation_timeout_milliseconds"] = int64(sharedRuntimeResourceObservationMaxTimeoutMilliseconds + 1)
		}},
		{"pressure threshold missing", "agents.pi.runtime.sharing.resource_pressure.pressure_threshold_bytes", func(table map[string]any) { delete(table, "pressure_threshold_bytes") }},
		{"recovery threshold invalid", "agents.pi.runtime.sharing.resource_pressure.recovery_threshold_bytes", func(table map[string]any) { table["recovery_threshold_bytes"] = int64(0) }},
		{"recovery threshold reaches pressure", "agents.pi.runtime.sharing.resource_pressure.recovery_threshold_bytes", func(table map[string]any) { table["recovery_threshold_bytes"] = int64(1000) }},
		{"eviction grace negative", "agents.pi.runtime.sharing.resource_pressure.eviction_grace_seconds", func(table map[string]any) { table["eviction_grace_seconds"] = int64(-1) }},
		{"pressure action unsupported", "agents.pi.runtime.sharing.resource_pressure.pressure_action", func(table map[string]any) { table["pressure_action"] = "kill" }},
		{"unknown action unsupported", "agents.pi.runtime.sharing.resource_pressure.unknown_action", func(table map[string]any) { table["unknown_action"] = "assume-clear" }},
		{"busy action unsupported", "agents.pi.runtime.sharing.resource_pressure.busy_action", func(table map[string]any) { table["busy_action"] = "evict" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := validPiRuntimeResourcePressureTable()
			testCase.mutate(table)
			if _, err := parsePiRuntimeResourcePressure(table, "agents.pi.runtime.sharing.resource_pressure"); err == nil {
				t.Fatal("malformed resource pressure policy was accepted")
			} else if got := piRefusalField(err); got != testCase.field {
				t.Fatalf("resource policy refused at field %q, want %q: %v", got, testCase.field, err)
			}
		})
	}
}

func validPiRuntimeResourcePressureTable() map[string]any {
	return map[string]any{
		"observation_path": "/metrics", "observation_timeout_milliseconds": int64(250),
		"pressure_threshold_bytes": int64(1000), "recovery_threshold_bytes": int64(100),
		"eviction_grace_seconds": int64(0), "pressure_action": "refuse-new-drain-idle",
		"unknown_action": "refuse-new", "busy_action": "observe",
	}
}

func TestParsePiDFlashRejectsDuplicatedRuntimeArgvSequence(t *testing.T) {
	table := map[string]any{
		"target_model": "Model", "draft_model": "Draft",
		"target_argv": []any{"--model", "Model"}, "draft_argv": []any{"--draft", "Draft"},
	}
	runtimeArgv := []string{"serve", "--model", "Model", "--draft", "Draft", "--draft", "Draft"}
	if _, err := parsePiDFlash(table, "agents.pi.runtime.dflash", runtimeArgv); err == nil {
		t.Fatal("duplicated draft argv sequence was accepted")
	} else if got, want := piRefusalField(err), "agents.pi.runtime.dflash"; got != want {
		t.Fatalf("duplicate argv sequence refused at %q, want %q: %v", got, want, err)
	}
}

func validPiRuntimeSharingTable() map[string]any {
	return map[string]any{
		"mode": "shared", "linger_seconds": int64(0), "max_leases": int64(2),
		"max_segment_bytes": int64(65536), "max_segments": int64(2),
		"heartbeat_interval_seconds": int64(2), "lease_stale_seconds": int64(10),
		"broker_start_timeout_seconds": int64(40), "restart_limit": int64(2),
		"restart_initial_backoff_seconds": int64(1), "restart_max_backoff_seconds": int64(2),
		"stable_run_seconds": int64(5), "quarantine_seconds": int64(30),
		"resource_pressure_mode": "disabled",
	}
}

func TestParsePiCompatRejectsEveryMalformedField(t *testing.T) {
	fields := []string{"supports_developer_role", "supports_reasoning_effort", "supports_usage_in_streaming", "supports_finish_reason", "max_tokens_field", "thinking_format"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			value := any("bad")
			if field == "max_tokens_field" || field == "thinking_format" {
				value = ""
			}
			_, err := parsePiCompat(map[string]any{field: value}, "agents.pi.compat")
			if err == nil {
				t.Fatalf("invalid %s was accepted", field)
			}
			if got, want := piRefusalField(err), "agents.pi.compat."+field; got != want {
				t.Fatalf("invalid %s refused at field %q, want %q: %v", field, got, want, err)
			}
		})
	}
	_, err := parsePiCompat(map[string]any{"future": true}, "agents.pi.compat")
	if err == nil {
		t.Fatal("unknown compatibility field was accepted")
	}
	if got, want := piRefusalField(err), "agents.pi.compat.future"; got != want {
		t.Fatalf("unknown compatibility field refused at %q, want %q: %v", got, want, err)
	}
}

func TestParsePiRuntimeRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown key", func(table map[string]any) { table["future"] = true }},
		{"executable missing", func(table map[string]any) { delete(table, "executable") }},
		{"executable wrong type", func(table map[string]any) { table["executable"] = 7 }},
		{"executable relative", func(table map[string]any) { table["executable"] = "bin/server" }},
		{"executable nul", func(table map[string]any) { table["executable"] = "/bin/server\x00bad" }},
		{"argv missing", func(table map[string]any) { delete(table, "argv") }},
		{"argv empty", func(table map[string]any) { table["argv"] = []any{} }},
		{"argv wrong type", func(table map[string]any) { table["argv"] = "serve" }},
		{"argv invalid token", func(table map[string]any) { table["argv"] = []any{""} }},
		{"argv NUL token", func(table map[string]any) { table["argv"] = []any{"serve\x00bad"} }},
		{"readiness missing", func(table map[string]any) { delete(table, "readiness_path") }},
		{"readiness wrong type", func(table map[string]any) { table["readiness_path"] = 7 }},
		{"readiness mismatch", func(table map[string]any) { table["readiness_path"] = "/health" }},
		{"startup timeout invalid", func(table map[string]any) { table["startup_timeout_seconds"] = int64(0) }},
		{"shutdown timeout invalid", func(table map[string]any) { table["shutdown_timeout_seconds"] = int64(-1) }},
		{"dflash wrong type", func(table map[string]any) { table["dflash"] = "table" }},
		{"sharing wrong type", func(table map[string]any) { table["sharing"] = "table" }},
	}
	wantFields := map[string]string{
		"unknown key":              "agents.pi.runtime.future",
		"executable missing":       "agents.pi.runtime.executable",
		"executable wrong type":    "agents.pi.runtime.executable",
		"executable relative":      "agents.pi.runtime.executable",
		"executable nul":           "agents.pi.runtime.executable",
		"argv missing":             "agents.pi.runtime.argv",
		"argv empty":               "agents.pi.runtime.argv",
		"argv wrong type":          "agents.pi.runtime.argv",
		"argv invalid token":       "agents.pi.runtime.argv",
		"argv NUL token":           "agents.pi.runtime.argv",
		"readiness missing":        "agents.pi.runtime.readiness_path",
		"readiness wrong type":     "agents.pi.runtime.readiness_path",
		"readiness mismatch":       "agents.pi.runtime.readiness_path",
		"startup timeout invalid":  "agents.pi.runtime.startup_timeout_seconds",
		"shutdown timeout invalid": "agents.pi.runtime.shutdown_timeout_seconds",
		"dflash wrong type":        "agents.pi.runtime.dflash",
		"sharing wrong type":       "agents.pi.runtime.sharing",
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := map[string]any{"executable": "/bin/echo", "argv": []any{"serve"}, "readiness_path": "/models", "startup_timeout_seconds": int64(5), "shutdown_timeout_seconds": int64(2)}
			testCase.mutate(table)
			_, err := parsePiRuntime(table, "agents.pi.runtime")
			if err == nil {
				t.Fatal("malformed runtime was accepted")
			}
			if got, want := piRefusalField(err), wantFields[testCase.name]; got != want {
				t.Fatalf("runtime refused at field %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestParsePiDFlashRejectsEveryMalformedField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown key", func(table map[string]any) { table["future"] = true }},
		{"target model missing", func(table map[string]any) { delete(table, "target_model") }},
		{"draft model missing", func(table map[string]any) { delete(table, "draft_model") }},
		{"target argv empty", func(table map[string]any) { table["target_argv"] = []any{} }},
		{"draft argv empty", func(table map[string]any) { table["draft_argv"] = []any{} }},
		{"target terminal model mismatch", func(table map[string]any) { table["target_argv"] = []any{"--model", "Other"} }},
		{"draft terminal model mismatch", func(table map[string]any) { table["draft_argv"] = []any{"--draft", "Other"} }},
		{"argv subsequence absent", func(table map[string]any) { table["target_argv"] = []any{"--absent", "Model"} }},
		{"argv subsequence overlaps", func(table map[string]any) {
			table["draft_model"] = "Model"
			table["draft_argv"] = []any{"--model", "Model"}
		}},
	}
	wantFields := map[string]string{
		"unknown key":                    "agents.pi.runtime.dflash.future",
		"target model missing":           "agents.pi.runtime.dflash.target_model",
		"draft model missing":            "agents.pi.runtime.dflash.draft_model",
		"target argv empty":              "agents.pi.runtime.dflash.target_argv",
		"draft argv empty":               "agents.pi.runtime.dflash.draft_argv",
		"target terminal model mismatch": "agents.pi.runtime.dflash.target_argv",
		"draft terminal model mismatch":  "agents.pi.runtime.dflash.draft_argv",
		"argv subsequence absent":        "agents.pi.runtime.dflash",
		"argv subsequence overlaps":      "agents.pi.runtime.dflash",
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			table := map[string]any{"target_model": "Model", "draft_model": "Draft", "target_argv": []any{"--model", "Model"}, "draft_argv": []any{"--draft", "Draft"}}
			testCase.mutate(table)
			_, err := parsePiDFlash(table, "agents.pi.runtime.dflash", []string{"serve", "--model", "Model", "--draft", "Draft"})
			if err == nil {
				t.Fatal("malformed dflash settings were accepted")
			}
			if got, want := piRefusalField(err), wantFields[testCase.name]; got != want {
				t.Fatalf("dflash refused at field %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestPiLaunchErrorFactoryPreservesTypedCode(t *testing.T) {
	err := piError("legacy-profile-invalid", errors.New("fixture"))
	var launchError *PiLaunchError
	if !errors.As(err, &launchError) || launchError.Code != "legacy-profile-invalid" {
		t.Fatalf("piError lost its typed code: %T %v", err, err)
	}
}

func TestSharedRuntimeErrorFactoryPreservesTypedCode(t *testing.T) {
	err := sharedRuntimeError("fixture-refusal", errors.New("fixture"))
	if code := sharedRuntimeErrorCode(err); code != "fixture-refusal" {
		t.Fatalf("sharedRuntimeError lost its typed code: got %q", code)
	}
}

func piProfileTable(t *testing.T, dflash bool) map[string]any {
	t.Helper()
	var document map[string]any
	if err := toml.Unmarshal([]byte(validPiProfileTOML("profile", "/bin/echo", 18011, dflash)), &document); err != nil {
		t.Fatal(err)
	}
	return piTestMap(t, piTestMap(t, piTestMap(t, document, "agents"), "pi"), "profiles")["profile"].(map[string]any)
}

func piConfigAgents(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := toml.Unmarshal([]byte(validPiProfileTOML("profile", "/bin/echo", 18011, false)), &document); err != nil {
		t.Fatal(err)
	}
	return piTestMap(t, document, "agents")
}

func piTestMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map[string]any", key, parent[key])
	}
	return value
}
