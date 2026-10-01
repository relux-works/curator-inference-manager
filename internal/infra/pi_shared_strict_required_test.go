package infra

import (
	"strings"
	"testing"
)

// Walker unit battery for required presence and nullable members. The shared
// walker enforces the declaration on the raw key stream: required members
// must be present, null refuses except for nullable members, and explicit
// zero values always pass through to the semantic checks.
func TestStrictJSONRequiredAndNullable(t *testing.T) {
	spec := &strictJSONSpec{
		object: map[string]*strictJSONSpec{
			"linger_seconds": nil,
			"argv":           nil,
			"nested": &strictJSONSpec{
				object: map[string]*strictJSONSpec{
					"grace": nil,
				},
				required: []string{"grace"},
			},
		},
		required: []string{"linger_seconds", "argv"},
		nullable: []string{"argv"},
	}
	for _, tc := range []struct {
		name       string
		document   string
		rejectNull bool
		wantErr    string
	}{
		{"explicit zeros pass", `{"linger_seconds":0,"argv":[],"nested":{"grace":0}}`, true, ""},
		{"nullable null passes", `{"linger_seconds":0,"argv":null,"nested":{"grace":0}}`, true, ""},
		{"missing required refuses", `{"argv":[],"nested":{"grace":0}}`, true, `missing required JSON member "linger_seconds"`},
		{"null required refuses", `{"linger_seconds":null,"argv":[],"nested":{"grace":0}}`, true, "JSON document contains null"},
		{"missing nested required refuses", `{"linger_seconds":0,"argv":[],"nested":{}}`, true, `missing required JSON member "grace"`},
		{"null nested required refuses", `{"linger_seconds":0,"argv":[],"nested":{"grace":null}}`, true, "JSON document contains null"},
		{"missing nullable refuses", `{"linger_seconds":0,"nested":{"grace":0}}`, true, `missing required JSON member "argv"`},
		{"unknown still refuses", `{"linger_seconds":0,"argv":[],"extra":1,"nested":{"grace":0}}`, true, `unknown JSON member "extra"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStrictJSON([]byte(tc.document), spec, tc.rejectNull)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid document refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}
