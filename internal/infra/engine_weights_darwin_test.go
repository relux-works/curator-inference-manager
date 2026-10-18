//go:build darwin

package infra

import (
	"strings"
	"testing"
	"time"
)

// Wire evidence accepts a valid observation but rejects malformed, missing-time,
// future-time and changed-instance observations before status publishes them.
func TestEngineWeightsWireObservationRefusals(t *testing.T) {
	valid := EngineWeightsObservation{ID: "gguf-sha256:" + strings.Repeat("a", 64), ObservedAt: time.Now().Add(-time.Second)}
	if err := validateWireWeights(&valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []EngineWeightsObservation{
		{ID: strings.ToUpper(valid.ID), ObservedAt: valid.ObservedAt},
		{ID: valid.ID},
		{ID: valid.ID, ObservedAt: time.Now().Add(time.Hour)},
	} {
		requireWeightsCode(t, validateWireWeights(&bad), "protocol_violation")
	}
	attested := SharedRuntimeProcessRecord{Stage: "running", Weights: &valid}
	if err := validateWireRuntimeRecord(attested, attested); err != nil {
		t.Fatal(err)
	}
	changed := attested
	different := valid
	different.ID = "gguf-sha256:" + strings.Repeat("b", 64)
	changed.Weights = &different
	requireWeightsCode(t, validateWireRuntimeRecord(changed, attested), "protocol_violation")
	changed.Weights = nil
	requireWeightsCode(t, validateWireRuntimeRecord(changed, attested), "protocol_violation")
}
