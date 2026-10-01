//go:build darwin

package infra

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Production boundary: readSharedWireMessage must refuse lexical bypasses of
// the closed wire schema (exact-case unknown members and duplicate keys) with
// the typed protocol_violation refusal.
func TestSharedWireMessageKeysRefuseLexicalBypass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   string
		wantErr string
	}{
		{"valid minimal", "{\"type\":\"status\",\"state\":\"serving\",\"stage\":\"composed\"}\n", ""},
		{"duplicate top member", "{\"type\":\"status\",\"state\":\"serving\",\"state\":\"draining\"}\n", "protocol_violation"},
		{"case alias top member", "{\"type\":\"status\",\"state\":\"serving\",\"State\":\"draining\"}\n", "protocol_violation"},
		{"case alias message type", "{\"type\":\"status\",\"Type\":\"lease\"}\n", "protocol_violation"},
		{"case alias nested broker", "{\"type\":\"status\",\"broker\":{\"pid\":1,\"PID\":2}}\n", "protocol_violation"},
		{"case alias nested runtime", "{\"type\":\"status\",\"runtime\":{\"pid\":1,\"Stage\":\"x\"}}\n", "protocol_violation"},
		{"unknown nested member", "{\"type\":\"status\",\"broker\":{\"pid\":1,\"extra\":true}}\n", "protocol_violation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, err := readSharedWireMessage(strings.NewReader(tc.frame))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid frame refused: %v", err)
				}
				if message.Type != "status" || message.State != "serving" || message.Stage != "composed" {
					t.Fatalf("valid frame decoded to %#v", message)
				}
				return
			}
			var typed *SharedRuntimeError
			if !errors.As(err, &typed) || typed.Code != tc.wantErr {
				t.Fatalf("error=%v want typed %s", err, tc.wantErr)
			}
		})
	}
}

func TestSharedWireStrictSpecCoversStructTags(t *testing.T) {
	requireStrictSpecCoversStruct(t, sharedWireStrictSpec(), reflect.TypeOf(sharedWireMessage{}))
}
