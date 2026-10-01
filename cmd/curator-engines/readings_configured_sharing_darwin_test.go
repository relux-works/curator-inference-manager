//go:build darwin

package main

import (
	"testing"
)

// S1 delta reproducer (TASK-261002-3axy8j F1): every sharing object a hello
// or status reply admits — configured and effective — obeys the producer's
// fatal-marker cardinality (1..16 when present; omission stays legal). The
// valid effective object is left unchanged; a copy with only the marker list
// replaced rides the configured slot. Lengths pin the catalog bounds
// (engineCatalogFatalMarkersMin/MaxCount); the infra generated carrier test
// moves with the producer. Drives run(status --json) over the kernel-attested
// fixture; hostile cases refuse with an empty readings set.
func TestPanelDeltaConfiguredMarkers(t *testing.T) {
	env := outerEnvelopeFixture(t)
	code, stdout, stderr := env.execute(env.resources, nil, nil)
	requireOuterValid(t, code, stdout, stderr)
	markers := func(length int) []string {
		list := make([]string, length)
		for i := range list {
			list[i] = "fatal-marker"
		}
		return list
	}
	for _, phase := range []string{"hello", "status"} {
		for _, tc := range []struct {
			name      string
			length    int
			wantValid bool
		}{
			{"below_minimum_refuses", 0, false},
			{"minimum_admits", 1, true},
			{"maximum_admits", 16, true},
			{"above_maximum_refuses", 17, false},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				mutate := func(reply map[string]any) {
					configured := outerReplyMap(t, reply["sharing_effective"])
					configured["fatal_output_substrings"] = markers(tc.length)
					reply["sharing_configured"] = configured
				}
				var code int
				var stdout, stderr string
				if phase == "hello" {
					code, stdout, stderr = env.execute(env.resources, mutate, nil)
				} else {
					code, stdout, stderr = env.execute(env.resources, nil, mutate)
				}
				if tc.wantValid {
					requireOuterValid(t, code, stdout, stderr)
					return
				}
				requireOuterRefusal(t, code, stdout, stderr, "protocol_violation")
			})
		}
	}
}
