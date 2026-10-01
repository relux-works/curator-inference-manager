//go:build darwin

package infra

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Generated null-element property test for the R2 class (loop-response S3
// design-change demand): only the entire nullable argv member may be null;
// a null element inside any wire array must refuse before readings are
// published. Array paths are enumerated by reflecting over the wire owner
// structs, not by listing examples, so a new slice member is covered
// automatically — or fails this test when its element kind has no valid
// builder. Each case runs through the production admission sequence in
// admitRequiredMatrixFrame.

type nullElementPath struct {
	parent  []string // JSON segments to the parent object
	member  string   // slice-typed JSON member
	element reflect.Type
}

// collectSlicePaths walks the owner type graph from the wire message,
// following JSON-tagged struct fields through pointers, structs and
// slices of structs. Every slice-typed member is recorded with its JSON
// path and element type. Maps, interfaces and unexported carriers fail
// closed: the walk must see every array the decoder can reach.
func collectSlicePaths(t *testing.T, typ reflect.Type, path []string, paths *[]nullElementPath) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	timeType := reflect.TypeOf(time.Time{})
	if typ == timeType {
		return
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("slice walk target %v at %q is not a struct", typ, strings.Join(path, "."))
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if field.Anonymous && name == "" {
			collectSlicePaths(t, field.Type, path, paths)
			continue
		}
		if name == "" {
			continue
		}
		member := field.Type
		for member.Kind() == reflect.Pointer {
			member = member.Elem()
		}
		switch member.Kind() {
		case reflect.Slice, reflect.Array:
			element := member.Elem()
			for element.Kind() == reflect.Pointer {
				element = element.Elem()
			}
			*paths = append(*paths, nullElementPath{parent: append([]string(nil), path...), member: name, element: element})
			if element.Kind() == reflect.Struct && element != timeType {
				collectSlicePaths(t, element, append(path, name+"[0]"), paths)
			}
		case reflect.Struct:
			if member == timeType {
				continue
			}
			collectSlicePaths(t, member, append(path, name), paths)
		case reflect.Map, reflect.Interface:
			t.Fatalf("unwalkable %v member %q at %q hides arrays from the null-element property", member.Kind(), name, strings.Join(path, "."))
		}
	}
}

// collectNullableMembers returns the JSON paths of nullable members from
// the single shared-wire declaration, so the property below and the
// decoder can never disagree about which whole members may be null.
func collectNullableMembers(node *strictJSONSpec, path []string, members map[string]bool) {
	for _, member := range node.nullable {
		name := member
		if len(path) > 0 {
			name = strings.Join(path, ".") + "." + member
		}
		members[name] = true
	}
	names := make([]string, 0, len(node.object))
	for name := range node.object {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := node.object[name]
		if child == nil {
			continue
		}
		if child.elements != nil {
			collectNullableMembers(child.elements, append(path, name+"[0]"), members)
			continue
		}
		if child.object != nil {
			collectNullableMembers(child, append(path, name), members)
		}
	}
}

// nullElementValidValue builds one valid element for a discovered slice
// path. A slice of an unknown element kind fails the test: the class
// stays closed by forcing an explicit builder.
func nullElementValidValue(t *testing.T, frame map[string]any, found nullElementPath) any {
	t.Helper()
	switch found.element.Kind() {
	case reflect.String:
		return "element-token"
	case reflect.Struct:
		if found.element == reflect.TypeOf(SharedLeaseStatus{}) {
			endpoint := "http://127.0.0.1:1/v1"
			if runtime, ok := frame["runtime"].(map[string]any); ok {
				if candidate, ok := runtime["endpoint"].(string); ok && candidate != "" {
					endpoint = candidate
				}
			}
			if lease, ok := frame["lease"].(map[string]any); ok {
				if candidate, ok := lease["endpoint"].(string); ok && candidate != "" {
					endpoint = candidate
				}
			}
			engine := map[string]any{"name": "engine-name", "key": "engine-key"}
			if candidate, ok := frame["engine_identity"].(map[string]any); ok && candidate["key"] != nil {
				engine = candidate
			}
			return requiredMatrixLease(endpoint, engine)
		}
	}
	t.Fatalf("no valid element builder for %v at %q", found.element, strings.Join(append(found.parent, found.member), "."))
	return nil
}

// setNullElementShape navigates to the parent object and sets the member
// to shape, creating the member when the parent exists. It reports
// whether the parent path existed in the frame.
func setNullElementShape(frame map[string]any, found nullElementPath, shape any) bool {
	node := frame
	for _, segment := range found.parent {
		child, ok := node[segment]
		if !ok {
			return false
		}
		next, ok := child.(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	node[found.member] = shape
	return true
}

func TestWireArrayElementsNeverNullable(t *testing.T) {
	var paths []nullElementPath
	collectSlicePaths(t, reflect.TypeOf(sharedWireMessage{}), nil, &paths)
	if len(paths) == 0 {
		t.Fatal("no slice paths enumerated")
	}
	// Anchor against a silently empty or drifting enumeration: the known
	// wire arrays must be present. Newly discovered paths join them
	// automatically.
	known := map[string]bool{
		"broker.argv": false, "runtime.pre_exec.argv": false, "runtime.post_exec.argv": false,
		"sharing_effective.fatal_output_substrings": false, "sharing_configured.fatal_output_substrings": false,
		"leases": false,
	}
	seen := map[string]bool{}
	for _, found := range paths {
		name := strings.Join(append(found.parent, found.member), ".")
		seen[name] = true
	}
	for name := range known {
		if !seen[name] {
			t.Errorf("enumerated slice paths %v lack known array %q", seen, name)
		}
	}
	t.Logf("null-element property: %d array paths enumerated", len(paths))
	nullable := map[string]bool{}
	collectNullableMembers(sharedWireStrictSpec(), nil, nullable)
	bases := requiredMatrixBases()
	for _, found := range paths {
		name := strings.Join(append(found.parent, found.member), ".")
		shapes := []struct {
			shapeName string
			build     func(t *testing.T, frame map[string]any) any
			// wantErr selects refusal (true) or admission (false).
			wantErr func() bool
		}{
			{"single-null", func(t *testing.T, _ map[string]any) any { return []any{nil} }, func() bool { return true }},
			{"null-first", func(t *testing.T, frame map[string]any) any {
				return []any{nil, nullElementValidValue(t, frame, found)}
			}, func() bool { return true }},
			{"null-last", func(t *testing.T, frame map[string]any) any {
				return []any{nullElementValidValue(t, frame, found), nil}
			}, func() bool { return true }},
			{"null-middle", func(t *testing.T, frame map[string]any) any {
				return []any{nullElementValidValue(t, frame, found), nil, nullElementValidValue(t, frame, found)}
			}, func() bool { return true }},
			{"whole-null", func(*testing.T, map[string]any) any { return nil }, func() bool { return !nullable[name] }},
			{"valid-single", func(t *testing.T, frame map[string]any) any {
				return []any{nullElementValidValue(t, frame, found)}
			}, func() bool { return false }},
			{"valid-pair", func(t *testing.T, frame map[string]any) any {
				return []any{nullElementValidValue(t, frame, found), nullElementValidValue(t, frame, found)}
			}, func() bool { return false }},
		}
		for _, shape := range shapes {
			covered := false
			for _, base := range bases {
				frame := cloneFrame(t, base.frame)
				if !setNullElementShape(frame, found, shape.build(t, frame)) {
					continue
				}
				covered = true
				err := admitRequiredMatrixFrame(t, base, frame)
				if shape.wantErr() {
					if err == nil {
						t.Errorf("array %s shape %s admitted on base %s", name, shape.shapeName, base.name)
						continue
					}
					var typed *SharedRuntimeError
					if base.kind != "decode-only" && !errors.As(err, &typed) && !strings.Contains(err.Error(), "incomplete") {
						t.Errorf("array %s shape %s on base %s refused without a typed error: %v", name, shape.shapeName, base.name, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("array %s shape %s refused on base %s: %v", name, shape.shapeName, base.name, err)
				}
			}
			if !covered {
				t.Errorf("array %s shape %s ran on no base", name, shape.shapeName)
			}
		}
	}
}
