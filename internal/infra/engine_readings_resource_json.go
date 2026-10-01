package infra

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The resource envelope's required members, in struct order. This is the one
// declaration shared by the production decoder below and the generated
// required-member matrix test. Provider action members are required only when
// the policy mode is provider; observed_at, reason, bytes and thresholds are
// state-conditional and stay out of this list.
var (
	engineResourceRequiredMembers          = []string{"schema", "state", "source", "loaded_model_memory", "inference", "admission", "policy"}
	engineResourceMemoryRequiredMembers    = []string{"state"}
	engineResourceInferenceRequiredMembers = []string{"state"}
	engineResourcePolicyRequiredMembers    = []string{"mode", "eviction_grace_seconds"}
	engineResourceProviderActionMembers    = []string{"pressure_action", "unknown_action", "busy_action"}
)

// UnmarshalJSON preserves required-member presence and the closed resource
// schema at the wire boundary. Go zero values must not turn missing/null
// evidence into a valid zero eviction grace. Semantic validation is performed
// before readings are built; this decoder also applies to consumer status reads.
// Key closure (exact case, no duplicates or unknown members) runs through the
// shared strict decoder before struct decoding, because encoding/json folds
// member names case-insensitively even with DisallowUnknownFields.
func (r *SharedRuntimeResourceStatus) UnmarshalJSON(data []byte) error {
	if err := validateStrictJSON(data, engineResourceStrictSpec(), true); err != nil {
		return err
	}
	type plain SharedRuntimeResourceStatus
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if err := requireEngineResourceMembers(object, engineResourceRequiredMembers...); err != nil {
		return err
	}
	for _, field := range []string{"loaded_model_memory", "inference", "policy"} {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(object[field], &nested); err != nil {
			return err
		}
		required := engineResourceMemoryRequiredMembers
		if field == "inference" {
			required = engineResourceInferenceRequiredMembers
		}
		if field == "policy" {
			required = engineResourcePolicyRequiredMembers
			if decoded.Policy.Mode == "provider" {
				required = append(append([]string(nil), required...), engineResourceProviderActionMembers...)
			}
		}
		if err := requireEngineResourceMembers(nested, required...); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	*r = SharedRuntimeResourceStatus(decoded)
	return nil
}

func requireEngineResourceMembers(object map[string]json.RawMessage, members ...string) error {
	for _, member := range members {
		if _, present := object[member]; !present {
			return fmt.Errorf("resource status lacks %s", member)
		}
	}
	return nil
}
