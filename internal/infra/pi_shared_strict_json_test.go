package infra

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStrictJSONCore(t *testing.T) {
	spec := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"state": nil,
		"nested": &strictJSONSpec{object: map[string]*strictJSONSpec{
			"state": nil,
		}},
		"rows": &strictJSONSpec{elements: &strictJSONSpec{object: map[string]*strictJSONSpec{
			"state": nil,
		}}},
	}}
	for _, tc := range []struct {
		name       string
		document   string
		rejectNull bool
		wantErr    string
	}{
		{"valid", `{"state":"a","nested":{"state":"b"},"rows":[{"state":"c"}]}`, false, ""},
		{"unknown top member", `{"state":"a","State":"b"}`, false, `unknown JSON member "State"`},
		{"unknown nested member", `{"nested":{"state":"a","STATE":"b"}}`, false, `unknown JSON member "STATE"`},
		{"unknown array member", `{"rows":[{"state":"a","extra":1}]}`, false, `unknown JSON member "extra"`},
		{"duplicate top member", `{"state":"a","state":"b"}`, false, `duplicate JSON member "state"`},
		{"duplicate nested member", `{"nested":{"state":"a","state":"b"}}`, false, `duplicate JSON member "state"`},
		{"null rejected", `{"state":null}`, true, "JSON document contains null"},
		{"null allowed", `{"state":null}`, false, ""},
		{"trailing content", `{"state":"a"} {}`, false, "content follows JSON document"},
		{"scalar member shapes pass", `{"state":0,"nested":{"state":false},"rows":[]}`, false, ""},
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

func TestEngineResourceStrictSpecCoversStructTags(t *testing.T) {
	requireStrictSpecCoversStruct(t, engineResourceStrictSpec(), reflect.TypeOf(SharedRuntimeResourceStatus{}))
}

// requireStrictSpecCoversStruct fails when any JSON member of typ or its nested
// structs is missing from the closed spec. New struct fields must extend the
// spec in the same change; the zero-value fallback never silently admits them.
func requireStrictSpecCoversStruct(t *testing.T, spec *strictJSONSpec, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("spec target %v is not a struct", typ)
	}
	if spec == nil || spec.object == nil {
		t.Fatalf("strict spec lacks an object member set for %v", typ)
	}
	timeType := reflect.TypeOf(time.Time{})
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if field.Anonymous && name == "" {
			requireStrictSpecCoversStruct(t, spec, field.Type)
			continue
		}
		if name == "" || field.PkgPath != "" {
			continue
		}
		child, ok := spec.object[name]
		if !ok {
			t.Fatalf("strict spec lacks member %q of %v", name, typ)
		}
		member := field.Type
		for member.Kind() == reflect.Pointer {
			member = member.Elem()
		}
		switch member.Kind() {
		case reflect.Struct:
			if member == timeType {
				continue
			}
			requireStrictSpecCoversStruct(t, child, member)
		case reflect.Slice, reflect.Array:
			element := member.Elem()
			for element.Kind() == reflect.Pointer {
				element = element.Elem()
			}
			if element.Kind() == reflect.Struct && element != timeType {
				if child == nil || child.elements == nil {
					t.Fatalf("strict spec lacks an element member set for %q of %v", name, typ)
				}
				requireStrictSpecCoversStruct(t, child.elements, element)
			}
		}
	}
}
