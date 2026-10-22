package infra

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The optional provenance object accepts exactly the v1 keys and values;
// malformed objects and aliases refuse while the neighboring valid reports pass.
func TestStatusProvenanceGuard(t *testing.T) {
	const baseline = `{"contract_version":1,"engine_identity":{"name":"selected","key":"key"},"runtime_key":"key","profile_digest":"","broker":{"state":"absent","source":"determined"},"sharing":{},"leases":[],"readings":{"contract_version":"observed-process/v3","facts":[]}}`
	const valid = `{"contract_version":"status-provenance/v1","engines_config":"/catalog/engines.toml","engines_config_sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","cache_root":"/cache"}`
	cases := []struct {
		name, raw string
		accepted  bool
	}{
		{"absent", "", true}, {"valid", `"provenance":` + valid, true},
		{"null", `"provenance":null`, false},
		{"alias-only", `"Provenance":` + valid, false},
		{"alias-duplicate", `"provenance":` + valid + `,"Provenance":` + valid, false},
		{"duplicate", `"provenance":` + valid + `,"provenance":` + valid, false},
	}
	for _, mutation := range []struct{ name, from, to string }{
		{"version", `status-provenance/v1`, `status-provenance/v2`},
		{"relative-catalog", `/catalog/engines.toml`, `engines.toml`},
		{"relative-cache", `/cache`, `cache`},
		{"unclean-catalog", `/catalog/engines.toml`, `/catalog/../engines.toml`},
		{"unclean-cache", `/cache`, `/cache/`},
		{"nonhex", `abcdef0123456789`, `gbcdef0123456789`},
		{"uppercase", `abcdef0123456789`, `Abcdef0123456789`},
		{"short", `abcdef0123456789`, `abc`},
		{"extra-key", `"cache_root":"/cache"`, `"cache_root":"/cache","extra":true`},
		{"nested-duplicate", `"cache_root":"/cache"`, `"cache_root":"/cache","cache_root":"/cache"`},
		{"nested-alias", `"cache_root"`, `"Cache_root"`},
		{"null-path", `"cache_root":"/cache"`, `"cache_root":null`},
	} {
		cases = append(cases, struct {
			name, raw string
			accepted  bool
		}{mutation.name, `"provenance":` + strings.Replace(valid, mutation.from, mutation.to, 1), false})
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valid), &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		copyFields := make(map[string]json.RawMessage)
		for k, v := range fields {
			if k != key {
				copyFields[k] = v
			}
		}
		raw, err := json.Marshal(copyFields)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			name, raw string
			accepted  bool
		}{"missing-" + key, `"provenance":` + string(raw), false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := baseline
			if tc.raw != "" {
				raw = `{` + tc.raw + `,` + baseline[1:]
			}
			err := ValidateSharedRuntimeStatusJSON([]byte(raw), "selected")
			if tc.accepted {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var refusal *SharedRuntimeError
			if !errors.As(err, &refusal) || refusal.Code != "status_contract_violation" {
				t.Fatalf("want status_contract_violation, got %v", err)
			}
		})
	}
}
