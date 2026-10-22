package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The real publication boundary writes a complete valid report, or refuses
// malformed provenance with status_contract_violation and zero stdout bytes.
func TestStatusProvenancePublication(t *testing.T) {
	const baseline = `{"contract_version":1,"engine_identity":{"name":"selected","key":"key"},"runtime_key":"key","profile_digest":"","broker":{"state":"absent","source":"determined"},"sharing":{},"leases":[],"readings":{"contract_version":"observed-process/v3","facts":[]}}`
	const provenance = `{"contract_version":"status-provenance/v1","engines_config":"/catalog/engines.toml","engines_config_sha256":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","cache_root":"/cache"}`
	valid := `{"provenance":` + provenance + `,` + baseline[1:]
	cases := []struct {
		name, raw string
		accepted  bool
	}{
		{"absent", baseline, true}, {"valid", valid, true},
		{"wrong-version", strings.Replace(valid, "status-provenance/v1", "status-provenance/v2", 1), false},
		{"relative-catalog", strings.Replace(valid, "/catalog/engines.toml", "engines.toml", 1), false},
		{"relative-cache", strings.Replace(valid, `"/cache"`, `"cache"`, 1), false},
		{"nonhex", strings.Replace(valid, "abcdef0123456789", "gbcdef0123456789", 1), false},
		{"uppercase", strings.Replace(valid, "abcdef0123456789", "Abcdef0123456789", 1), false},
		{"short", strings.Replace(valid, "abcdef0123456789", "abc", 1), false},
		{"missing-key", strings.Replace(valid, `,"cache_root":"/cache"`, "", 1), false},
		{"extra-key", strings.Replace(valid, `"cache_root":"/cache"`, `"cache_root":"/cache","extra":true`, 1), false},
		{"alias-duplicate", `{"Provenance":` + provenance + `,` + valid[1:], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := writeStatusJSONDocument(&out, []byte(tc.raw), "selected", &stderr)
			if tc.accepted {
				if code != 0 || stderr.Len() != 0 || out.String() != tc.raw+"\n" {
					t.Fatalf("valid report: code=%d stdout=%q stderr=%q", code, &out, &stderr)
				}
				return
			}
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &refusal); err != nil || code != 1 || out.Len() != 0 || refusal.Code != "status_contract_violation" {
				t.Fatalf("code=%d stdout=%q stderr=%q err=%v", code, &out, &stderr, err)
			}
		})
	}
}
