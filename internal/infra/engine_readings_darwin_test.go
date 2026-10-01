//go:build darwin

package infra

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
)

func TestStatusReadingsRefusesInvalidLiveObservation(t *testing.T) {
	now := time.Now()
	stale, future := now.Add(-30*time.Second-time.Nanosecond), now.Add(time.Nanosecond)
	for _, tc := range []struct {
		name, code string
		change     func(*SharedRuntimeResourceStatus)
	}{
		{"schema", "readings_malformed", func(r *SharedRuntimeResourceStatus) { r.Schema = "__mutant__" }},
		{"malformed source", "readings_malformed", func(r *SharedRuntimeResourceStatus) { r.Source = "__mutant__" }},
		{"failed read", "readings_read_failed", func(r *SharedRuntimeResourceStatus) { r.Source = "provider-observation-failed" }},
		{"partial timestamp", "readings_partial", func(r *SharedRuntimeResourceStatus) { r.ObservedAt = nil }},
		{"stale timestamp", "readings_stale", func(r *SharedRuntimeResourceStatus) { r.ObservedAt = &stale }},
		{"future timestamp", "readings_stale", func(r *SharedRuntimeResourceStatus) { r.ObservedAt = &future }},
		{"malformed inference", "readings_malformed", func(r *SharedRuntimeResourceStatus) { r.Inference.State = "__mutant__" }},
		{"unknown without reason", "readings_partial", func(r *SharedRuntimeResourceStatus) { r.Inference.State = SharedRuntimeInferenceUnknown }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			served := fixture.answerOnce(t, nil)
			r := validReadingsResourceStatus(now, SharedRuntimeInferenceIdle)
			tc.change(&r)
			oldWrite, oldRead := sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage
			oldNow := sharedRuntimeReadingsNow
			sharedRuntimeReadingsNow = func() time.Time { return now }
			t.Cleanup(func() {
				sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage = oldWrite, oldRead
				sharedRuntimeReadingsNow = oldNow
			})
			sharedRuntimeStatusWriteMessage = func(io.Writer, sharedWireMessage) error { return nil }
			sharedRuntimeStatusReadMessage = func(io.Reader) (sharedWireMessage, error) {
				return sharedWireMessage{Type: "status", State: "serving", Stage: "composed", Broker: &fixture.broker, Runtime: &fixture.record, EffectiveSharing: &fixture.resolved.Sharing, Resources: &r}, nil
			}
			report, err := SharedRuntimeStatusReport(sharedRuntimeOperatorOptionsForFixture(fixture))
			var typed *SharedRuntimeError
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("wanted %s typed refusal, got %v", tc.code, err)
			}
			if len(report.Readings.Facts) != 0 {
				t.Fatal("refusal leaked partial readings")
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStatusReadingsObservesOnlyAttestedFacts(t *testing.T) {
	for _, testCase := range []struct {
		state SharedRuntimeInferenceState
		age   time.Duration
	}{
		{SharedRuntimeInferenceIdle, 0}, {SharedRuntimeInferenceBusy, 0}, {SharedRuntimeInferenceUnknown, 0}, {SharedRuntimeInferenceIdle, 30 * time.Second},
	} {
		state := testCase.state
		t.Run(string(state)+"/"+testCase.age.String(), func(t *testing.T) {
			fixture := newSharedAttestationFixture(t)
			served := fixture.answerOnce(t, nil)
			now := time.Now()
			stamp := now.Add(-testCase.age)
			r := validReadingsResourceStatus(stamp, state)
			oldWrite, oldRead := sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage
			oldNow := sharedRuntimeReadingsNow
			sharedRuntimeReadingsNow = func() time.Time { return now }
			t.Cleanup(func() {
				sharedRuntimeStatusWriteMessage, sharedRuntimeStatusReadMessage = oldWrite, oldRead
				sharedRuntimeReadingsNow = oldNow
			})
			sharedRuntimeStatusWriteMessage = func(io.Writer, sharedWireMessage) error { return nil }
			sharedRuntimeStatusReadMessage = func(io.Reader) (sharedWireMessage, error) {
				return sharedWireMessage{Type: "status", State: "serving", Stage: "composed", Broker: &fixture.broker, Runtime: &fixture.record, EffectiveSharing: &fixture.resolved.Sharing, Resources: &r}, nil
			}
			report, err := SharedRuntimeStatusReport(sharedRuntimeOperatorOptionsForFixture(fixture))
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Readings.Facts) != len(inferenceengine.MeasuredFacts()) {
				t.Fatal("incomplete readings")
			}
			for _, row := range report.Readings.Facts {
				switch row.Fact {
				case inferenceengine.FactHealth:
					if row.Outcome != inferenceengine.OutcomeObservedValue {
						t.Fatal("attested HTTP liveness missing")
					}
				case inferenceengine.FactInferenceBusy:
					if state == SharedRuntimeInferenceUnknown {
						if row.Outcome != inferenceengine.OutcomeNotObserved || row.Reason != r.Inference.Reason {
							t.Fatal(row)
						}
					} else if row.Value != `{"busy":`+map[bool]string{true: "true", false: "false"}[state == SharedRuntimeInferenceBusy]+`}` {
						t.Fatal(row)
					}
				default:
					if row.Outcome != inferenceengine.OutcomeNotObserved || row.Reason == "" {
						t.Fatalf("unobserved legacy/runtime fact synthesized: %#v", row)
					}
				}
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func validReadingsResourceStatus(now time.Time, state SharedRuntimeInferenceState) SharedRuntimeResourceStatus {
	bytes := uint64(100)
	observation := sharedRuntimeProviderResourceObservation{LoadedModelMemory: sharedRuntimeProviderMemoryFact{State: "observed", Bytes: &bytes}, Inference: sharedRuntimeProviderInferenceFact{State: string(state)}}
	if state == SharedRuntimeInferenceUnknown {
		observation.Inference.Reason = "provider cannot observe"
	}
	status, _ := classifySharedRuntimeResources(observation, testSharedRuntimeResourcePolicy(), "serving", false, now)
	return status
}
