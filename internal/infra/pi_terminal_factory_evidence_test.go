//go:build !windows

package infra

import (
	"errors"
	"testing"
)

func TestPiTerminalEvidenceFactoryNeverErasesRefusal(t *testing.T) {
	cause := errors.New("fixture failure")
	err := piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "fixture.site", cause)
	var typed *PiTerminalEvidenceError
	if !errors.As(err, &typed) || typed.State != PiTerminalEvidenceUnclassifiable || typed.Site != "fixture.site" || !errors.Is(err, cause) {
		t.Fatalf("factory erased refusal: %v", err)
	}
}
