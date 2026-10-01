//go:build darwin

package infra

import (
	"encoding/json"
	"strings"
	"testing"
)

// Generated cardinality test for the R3 class (rework-brief-04): an
// explicitly present fatal_output_substrings list obeys the producer's
// cardinality, generated from the catalog's own constants; legitimate
// omission stays legal. Lengths below are min-1, min, max and max+1, so
// a producer bound change moves the generated boundaries with it.

func TestWireFatalMarkersObeyProducerCardinality(t *testing.T) {
	runtime := PiRuntime{StartupTimeoutSeconds: 1, ShutdownTimeoutSeconds: 30}
	base := func(t *testing.T) PiRuntimeSharing {
		t.Helper()
		data, err := json.Marshal(requiredMatrixSharing(false))
		if err != nil {
			t.Fatal(err)
		}
		var sharing PiRuntimeSharing
		if err := json.Unmarshal(data, &sharing); err != nil {
			t.Fatal(err)
		}
		return sharing
	}
	markers := func(length int) []string {
		list := make([]string, length)
		for i := range list {
			list[i] = "fatal-marker"
		}
		return list
	}
	// Present-empty must decode to a non-nil list: the semantic gate
	// reads presence from nil-ness, and explicit null already refuses in
	// the strict pre-pass. This pins the mechanism the gate depends on.
	t.Run("present_empty_decodes_non_nil", func(t *testing.T) {
		sharing := base(t)
		if sharing.FatalOutputSubstrings != nil {
			t.Fatalf("omitted markers decoded to %q, want nil", sharing.FatalOutputSubstrings)
		}
		data, err := json.Marshal(map[string]any{"fatal_output_substrings": []string{}})
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Markers []string `json:"fatal_output_substrings"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Markers == nil || len(decoded.Markers) != 0 {
			t.Fatalf("present empty list decoded to %#v, want non-nil empty", decoded.Markers)
		}
	})
	for _, tc := range []struct {
		name   string
		length int
		// present selects an explicit list; absent leaves the member nil.
		present bool
		wantErr string
	}{
		{"absent_omitted_valid", 0, false, ""},
		{"present_below_minimum_refuses", engineCatalogFatalMarkersMinCount - 1, true, "fatal marker count"},
		{"present_minimum_valid", engineCatalogFatalMarkersMinCount, true, ""},
		{"present_maximum_valid", engineCatalogFatalMarkersMaxCount, true, ""},
		{"present_above_maximum_refuses", engineCatalogFatalMarkersMaxCount + 1, true, "fatal marker count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sharing := base(t)
			if tc.present {
				sharing.FatalOutputSubstrings = markers(tc.length)
			}
			err := validateWireSharing(sharing, runtime)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("length %d present=%v refused: %v", tc.length, tc.present, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("length %d present=%v error=%v want substring %q", tc.length, tc.present, err, tc.wantErr)
			}
			if sharedRuntimeErrorCode(err) != "protocol_violation" {
				t.Fatalf("length %d present=%v code=%q want protocol_violation", tc.length, tc.present, sharedRuntimeErrorCode(err))
			}
		})
	}
}

// Generated carrier property for the S1 class (rework-brief-05): every
// sharing object a hello or status reply admits — configured and effective —
// enforces the producer's marker cardinality. Each frame runs the production
// admission sequence (readSharedWireMessage + the caller presence check +
// the attested envelope validator) on every hello/status matrix base, with
// the other carrier left valid. Lengths are generated from the catalog's own
// constants, so a producer bound change moves the boundaries with it.
func TestWireSharingCarriersEnforceProducerCardinality(t *testing.T) {
	markers := func(length int) []any {
		list := make([]any, length)
		for i := range list {
			list[i] = "fatal-marker"
		}
		return list
	}
	for _, base := range requiredMatrixBases() {
		if base.kind != "hello_ok" && base.kind != "status" {
			continue
		}
		for _, carrier := range []string{"sharing_configured", "sharing_effective"} {
			for _, tc := range []struct {
				name    string
				length  int
				wantErr string
			}{
				{"present_below_minimum_refuses", engineCatalogFatalMarkersMinCount - 1, "fatal marker count"},
				{"present_minimum_admits", engineCatalogFatalMarkersMinCount, ""},
				{"present_maximum_admits", engineCatalogFatalMarkersMaxCount, ""},
				{"present_above_maximum_refuses", engineCatalogFatalMarkersMaxCount + 1, "fatal marker count"},
			} {
				t.Run(base.name+"/"+carrier+"/"+tc.name, func(t *testing.T) {
					frame := cloneFrame(t, base.frame)
					effective, ok := frame["sharing_effective"].(map[string]any)
					if !ok {
						t.Fatal("matrix base lacks a valid effective sharing object")
					}
					if carrier == "sharing_configured" {
						data, err := json.Marshal(effective)
						if err != nil {
							t.Fatal(err)
						}
						var configured map[string]any
						if err := json.Unmarshal(data, &configured); err != nil {
							t.Fatal(err)
						}
						configured["fatal_output_substrings"] = markers(tc.length)
						frame[carrier] = configured
					} else {
						effective["fatal_output_substrings"] = markers(tc.length)
					}
					err := admitRequiredMatrixFrame(t, base, frame)
					if tc.wantErr == "" {
						if err != nil {
							t.Fatalf("%s length %d refused: %v", carrier, tc.length, err)
						}
						return
					}
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("%s length %d error=%v want substring %q", carrier, tc.length, err, tc.wantErr)
					}
					if sharedRuntimeErrorCode(err) != "protocol_violation" {
						t.Fatalf("%s length %d code=%q want protocol_violation", carrier, tc.length, sharedRuntimeErrorCode(err))
					}
				})
			}
		}
		t.Run(base.name+"/configured_absent_admits", func(t *testing.T) {
			if err := admitRequiredMatrixFrame(t, base, cloneFrame(t, base.frame)); err != nil {
				t.Fatalf("unmutated base refused: %v", err)
			}
		})
	}
}
