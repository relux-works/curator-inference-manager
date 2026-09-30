//go:build !windows

package infra

import (
	"bytes"
	"testing"
)

func TestSharedRuntimeFatalOutputMatcherRequiresFullMarkerAcrossWrites(t *testing.T) {
	const marker = "RuntimeError: resource limit exceeded"
	var persisted bytes.Buffer
	writer := newSharedRuntimeOutputWriter(&persisted, []string{marker})
	if _, err := writer.Write([]byte("noise " + marker[:17])); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-writer.state.fatalOutput:
		t.Fatalf("partial marker was treated as fatal: %q", got)
	default:
	}
	if _, err := writer.Write([]byte(marker[17:] + " tail")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-writer.state.fatalOutput:
		if got != marker {
			t.Fatalf("matched marker=%q want=%q", got, marker)
		}
	default:
		t.Fatal("marker split across writes was not detected")
	}
	if got, want := persisted.String(), "noise "+marker+" tail"; got != want {
		t.Fatalf("runtime output forwarding changed bytes: got=%q want=%q", got, want)
	}

	for _, nearMiss := range []string{
		marker[:len(marker)-1] + "!",
		"X" + marker[1:],
	} {
		var sink bytes.Buffer
		nearMissWriter := newSharedRuntimeOutputWriter(&sink, []string{marker})
		if _, err := nearMissWriter.Write([]byte(nearMiss)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-nearMissWriter.state.fatalOutput:
			t.Errorf("near miss %q triggered fatal marker %q", nearMiss, got)
		default:
		}
	}
}
