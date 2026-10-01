package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// strictJSONSpec is the closed member set of one JSON object, shared by every
// evidence envelope decoded on the readings path. encoding/json matches object
// keys case-insensitively even with DisallowUnknownFields, so a hostile peer
// can smuggle "State" beside "state" and overwrite the canonical member. This
// pre-pass enforces exact-case member sets and duplicate rejection on the raw
// key stream before struct decoding. All failures are plain errors; each
// production boundary wraps them in its own typed refusal.
//
// required names the members that must be present in every decoded object.
// Absent, null and zero are three distinct states: Go struct decoding launders
// absent and null scalars into zero values, so presence and non-null are
// enforced here on the raw key stream, before any semantic range check runs.
// required mirrors the owner struct's non-omitempty JSON members; legitimately
// optional (omitempty) members stay absent-capable. nullable names the members
// whose value may be JSON null. Only process argv arrays are nullable: the
// owner type declares them as non-omitempty slices, so a producer with an
// empty argv emits an explicit null. Every other member refuses null when
// rejectNull is set, including optional members, because the producer omits
// them instead of nulling them.
type strictJSONSpec struct {
	// object maps each exact allowed member name to the spec of its value. A
	// nil child accepts any JSON value. A nil map accepts any member name
	// (duplicates are still rejected); it is used only for values that carry
	// no further key constraints.
	object map[string]*strictJSONSpec
	// elements constrains object elements of an array value. A nil spec
	// accepts any array element shape. Array elements are never nullable:
	// the producer emits Go strings and structs, never null elements.
	elements *strictJSONSpec
	// required lists the members that must be present. An empty list
	// enforces nothing; nodes without required members behave as before.
	required []string
	// nullable lists the members that accept an explicit JSON null value.
	nullable []string
}

// validateStrictJSON enforces exact-case closed member sets and duplicate
// rejection over data. Declared required members must be present. When
// rejectNull is set, any JSON null refuses except for nullable members.
func validateStrictJSON(data []byte, spec *strictJSONSpec, rejectNull bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateStrictJSONValue(decoder, spec, rejectNull); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("content follows JSON document")
	}
	return nil
}

func strictSpecAllowsNull(spec *strictJSONSpec, name string) bool {
	if spec == nil {
		return false
	}
	for _, candidate := range spec.nullable {
		if candidate == name {
			return true
		}
	}
	return false
}

func validateStrictJSONValue(decoder *json.Decoder, spec *strictJSONSpec, rejectNull bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if rejectNull {
			return errors.New("JSON document contains null")
		}
		return nil
	}
	delimiter, aggregate := token.(json.Delim)
	if !aggregate {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if seen[name] {
				return fmt.Errorf("duplicate JSON member %q", name)
			}
			seen[name] = true
			var child *strictJSONSpec
			if spec != nil && spec.object != nil {
				child, ok = spec.object[name]
				if !ok {
					return fmt.Errorf("unknown JSON member %q", name)
				}
			}
			memberRejectNull := rejectNull && !strictSpecAllowsNull(spec, name)
			if err := validateStrictJSONValue(decoder, child, memberRejectNull); err != nil {
				return err
			}
		}
		if spec != nil {
			for _, name := range spec.required {
				if !seen[name] {
					return fmt.Errorf("missing required JSON member %q", name)
				}
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		var elements *strictJSONSpec
		if spec != nil {
			elements = spec.elements
		}
		for decoder.More() {
			// Array elements are never nullable, even when the array
			// member itself is: only the whole member may be null. The
			// incoming member-level permission is intentionally not
			// propagated here; encoding/json would otherwise erase a
			// null element into a zero value before any semantic check.
			if err := validateStrictJSONValue(decoder, elements, true); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

// engineResourceStrictSpec is the closed member set of the versioned resource
// observation: the top object plus nested memory, inference and policy
// objects. Every other value is a scalar.
func engineResourceStrictSpec() *strictJSONSpec {
	memory := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"state": nil, "bytes": nil, "pressure_at_bytes": nil, "recovery_at_bytes": nil, "reason": nil,
	}}
	inference := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"state": nil, "reason": nil,
	}}
	policy := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"mode": nil, "eviction_grace_seconds": nil, "pressure_action": nil, "unknown_action": nil, "busy_action": nil,
	}}
	return &strictJSONSpec{object: map[string]*strictJSONSpec{
		"schema": nil, "state": nil, "source": nil, "observed_at": nil,
		"loaded_model_memory": memory, "inference": inference,
		"admission": nil, "reason": nil, "policy": policy,
	}}
}
