package infra

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The guard accepts all eleven joint pairs and each complete restart generation;
// violation plants must produce status_contract_violation rather than pass.
func TestStatusContractGuard(t *testing.T) {
	baseline := `{"contract_version":1,"engine_identity":{"name":"profile","key":"key"},"runtime_key":"key","profile_digest":"","broker":{"state":"absent","source":"determined"},"sharing":{"configured":{"max_leases":0}},"leases":[],"readings":{"contract_version":"observed-process/v3","facts":[]}}`
	decode := func() map[string]any {
		var f map[string]any
		if err := json.Unmarshal([]byte(baseline), &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	check := func(fields map[string]any, accepted bool) {
		t.Helper()
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateSharedRuntimeStatusJSON(raw, "profile")
		var typed *SharedRuntimeError
		if accepted && err != nil || !accepted && (!errors.As(err, &typed) || typed.Code != "status_contract_violation") {
			t.Fatalf("accepted=%t error=%v document=%s", accepted, err, raw)
		}
	}
	for source, states := range map[string][]string{
		"determined": {"absent"}, "kernel-candidate-reporting-only": {"starting-unverified"},
		"record-derived-unverified": {"starting", "serving", "lingering", "draining", "unverified-stale"},
		"attested":                  {"starting", "serving", "lingering", "draining"},
	} {
		for _, state := range states {
			f := decode()
			f["broker"] = map[string]string{"state": state, "source": source}
			check(f, true)
		}
	}
	f := decode()
	check(f, true)
	for key, value := range map[string]any{"restart_count": 0, "quarantined_until": nil, "last_readiness_match": nil, "manual_quarantine": false} {
		f[key] = value
	}
	check(f, true)
	f["restart_not_before"], f["half_open"] = "2026-01-01T00:00:00Z", false
	check(f, true)
	delete(f, "half_open")
	check(f, false)
	for _, key := range []string{"contract_version", "engine_identity", "runtime_key", "profile_digest", "broker", "sharing", "leases", "readings"} {
		f := decode()
		delete(f, key)
		check(f, false)
		if key != "sharing" && key != "profile_digest" && key != "leases" {
			f = decode()
			f[key] = nil
			check(f, false)
		}
	}
	for _, readings := range []string{
		`{"contract_version":"observed-process/v2","facts":[]}`,
		`{"contract_version":"observed-process/v3","facts":null}`,
		`{"contract_version":"observed-process/v3","facts":[{"fact":"health","outcome":"observed-value","source":"unavailable"}]}`,
		`{"contract_version":"observed-process/v3","facts":[{"fact":"health","outcome":"not-observed","source":"unavailable","cause":"unknown","reason":"readings_stale"}]}`,
		`{"contract_version":"observed-process/v3","facts":[],"error":"readings_stale"}`,
	} {
		f := decode()
		f["readings"] = json.RawMessage(readings)
		check(f, false)
	}
	f = decode()
	f["readings"] = json.RawMessage(`{"contract_version":"observed-process/v3","facts":[{"fact":"health","outcome":"not-observed","source":"unavailable","cause":"read-failure","reason":"readings_stale"}]}`)
	check(f, true)

	for _, tc := range []struct {
		key   string
		value any
	}{
		{"contract_version", 2}, {"contract_version", 1.5}, {"engine_identity", map[string]string{"name": "other", "key": "key"}},
		{"runtime_key", " "}, {"profile_digest", false},
		{"broker", map[string]string{"state": "refused", "source": "record-derived-unverified"}},
		{"broker", map[string]string{"state": "serving", "source": "determined"}},
		{"sharing", map[string]any{"configured": map[string]any{"max_leases": -1}}},
		{"leases", map[string]any{}}, {"restart_count", 0}, {"half_open", false},
		{"runtime", map[string]any{"pid": 1}},
		{"runtime", map[string]any{"pid": 1, "start_time": map[string]int{"seconds": 0, "microseconds": 1000000}}},
	} {
		f := decode()
		f[tc.key] = tc.value
		check(f, false)
	}
	f = decode()
	f["runtime"] = nil
	check(f, true)
	f["runtime"] = map[string]any{"pid": 0, "start_time": map[string]int{"seconds": 0, "microseconds": 999999}}
	check(f, true)
}

// Paired raw-wire cases pin consumer zero-value tolerances and rejection rules;
// raw JSON preserves duplicate keys that a map-based fixture would erase.
func TestStatusContractGuardConsumerParity(t *testing.T) {
	const baseline = `{"contract_version":1,"engine_identity":{"name":"profile","key":"key"},"runtime_key":"key","profile_digest":"","broker":{"state":"absent","source":"determined"},"sharing":{"configured":{"max_leases":0}},"leases":[],"readings":{"contract_version":"observed-process/v3","facts":[]}}`
	for _, tc := range []struct {
		name, from, to string
		accepted       bool
	}{
		{"baseline", "", "", true},
		// localruntime/decode.go:177-187: struct zero values, but int types/range still apply.
		{"max-omitted", `"max_leases":0`, ``, true},
		{"max-null", `"max_leases":0`, `"max_leases":null`, true},
		{"max-negative", `"max_leases":0`, `"max_leases":-1`, false},
		{"max-string", `"max_leases":0`, `"max_leases":"0"`, false},
		{"configured-omitted", `"configured":{"max_leases":0}`, ``, true},
		{"configured-null", `"configured":{"max_leases":0}`, `"configured":null`, true},
		{"configured-array", `"configured":{"max_leases":0}`, `"configured":[]`, false},
		{"sharing-null", `"sharing":{"configured":{"max_leases":0}}`, `"sharing":null`, true},
		{"sharing-missing", `"sharing":{"configured":{"max_leases":0}},`, ``, false},
		{"sharing-case", `"configured":{"max_leases":0}`, `"CONFIGURED":{"MAX_LEASES":0}`, true},
		// localruntime/decode.go:290-325: pid is optional, nullable and unrestricted in sign;
		// start_time and both bounded components remain required.
		{"pid-omitted", `"leases":[]`, `"runtime":{"start_time":{"seconds":0,"microseconds":0}},"leases":[]`, true},
		{"pid-null", `"leases":[]`, `"runtime":{"pid":null,"start_time":{"seconds":0,"microseconds":0}},"leases":[]`, true},
		{"pid-negative", `"leases":[]`, `"runtime":{"pid":-1,"start_time":{"seconds":0,"microseconds":0}},"leases":[]`, true},
		{"pid-string", `"leases":[]`, `"runtime":{"pid":"1","start_time":{"seconds":0,"microseconds":0}},"leases":[]`, false},
		{"start-missing", `"leases":[]`, `"runtime":{"pid":null},"leases":[]`, false},
		{"start-null", `"leases":[]`, `"runtime":{"pid":null,"start_time":null},"leases":[]`, false},
		{"seconds-null", `"leases":[]`, `"runtime":{"start_time":{"seconds":null,"microseconds":0}},"leases":[]`, false},
		{"seconds-missing", `"leases":[]`, `"runtime":{"start_time":{"microseconds":0}},"leases":[]`, false},
		{"seconds-negative", `"leases":[]`, `"runtime":{"start_time":{"seconds":-1,"microseconds":0}},"leases":[]`, false},
		{"micros-missing", `"leases":[]`, `"runtime":{"start_time":{"seconds":0}},"leases":[]`, false},
		{"micros-null", `"leases":[]`, `"runtime":{"start_time":{"seconds":0,"microseconds":null}},"leases":[]`, false},
		{"micros-negative", `"leases":[]`, `"runtime":{"start_time":{"seconds":0,"microseconds":-1}},"leases":[]`, false},
		{"micros-upper-bound", `"leases":[]`, `"runtime":{"start_time":{"seconds":0,"microseconds":999999}},"leases":[]`, true},
		{"micros-out-of-range", `"leases":[]`, `"runtime":{"start_time":{"seconds":0,"microseconds":1000000}},"leases":[]`, false},
		{"runtime-case", `"leases":[]`, `"runtime":{"PID":null,"START_TIME":{"SECONDS":0,"MICROSECONDS":0}},"leases":[]`, true},
		// localruntime/decode.go:331-340: required presence, nullable slice, opaque elements.
		{"leases-null", `"leases":[]`, `"leases":null`, true},
		{"leases-missing", `"leases":[],`, ``, false},
		{"leases-object", `"leases":[]`, `"leases":{}`, false},
		{"leases-opaque", `"leases":[]`, `"leases":[null,1,{}]`, true},
		// localruntime/decode.go:140-168,346-355: struct case matching and nullable digest.
		{"digest-null", `"profile_digest":""`, `"profile_digest":null`, true},
		{"digest-number", `"profile_digest":""`, `"profile_digest":0`, false},
		{"identity-case", `"name":"profile","key":"key"`, `"NAME":"profile","KEY":"key"`, true},
		{"broker-case", `"state":"absent","source":"determined"`, `"STATE":"absent","SOURCE":"determined"`, true},
		{"pair-outside-set", `"state":"absent"`, `"state":"serving"`, false},
		// engineobservation/adapter.go:270 and strict.go:60-77: all outer duplicates
		// refuse, only readings aliases refuse, additive nested values stay opaque.
		{"duplicate-readings", `"readings":`, `"readings":{},"readings":`, false},
		{"duplicate-readings-reversed", `"readings":`, `"readings":{"contract_version":"observed-process/v3","facts":[]},"readings":`, false},
		{"readings-alias", `"readings":`, `"READINGS":{},"readings":`, false},
		{"readings-mixed-alias", `"readings":`, `"Readings":{},"readings":`, false},
		{"readings-alias-only", `"readings":`, `"READINGS":`, false},
		{"duplicate-additive", `"readings":`, `"extra":1,"extra":2,"readings":`, false},
		{"additive-case", `"readings":`, `"EXTRA":null,"extra":{},"BROKER":{},"readings":`, true},
		{"additive-nested-duplicates", `"readings":`, `"extra":{"x":null,"x":[]},"readings":`, true},
		{"nested-duplicate-valid", `"max_leases":0`, `"max_leases":-1,"max_leases":0`, true},
		{"nested-duplicate-invalid-type", `"max_leases":0`, `"max_leases":"bad","max_leases":0`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := baseline
			if tc.from != "" {
				if strings.Count(raw, tc.from) != 1 {
					t.Fatal("fixture replacement must match exactly once")
				}
				raw = strings.Replace(raw, tc.from, tc.to, 1)
			}
			err := ValidateSharedRuntimeStatusJSON([]byte(raw), "profile")
			if tc.accepted {
				if err != nil {
					t.Fatalf("consumer-valid document refused: %v", err)
				}
				return
			}
			var typed *SharedRuntimeError
			if !errors.As(err, &typed) || typed.Code != "status_contract_violation" {
				t.Fatalf("want status_contract_violation, got %v", err)
			}
		})
	}
}
