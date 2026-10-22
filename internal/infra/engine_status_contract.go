package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const StatusProvenanceContractVersion = "status-provenance/v1"

// StatusProvenance identifies the catalog bytes and cache used by one status call.
type StatusProvenance struct {
	ContractVersion     string `json:"contract_version"`
	EnginesConfig       string `json:"engines_config"`
	EnginesConfigSHA256 string `json:"engines_config_sha256"`
	CacheRoot           string `json:"cache_root"`
}

// Mirrors skill-agents-management pkg/localruntime/decode.go:51-66.
// Keep pairs joint: independent state/source vocabularies admit impossible pairs.
func validStatusBrokerPair(state, source string) bool {
	switch source {
	case "determined":
		return state == "absent"
	case "kernel-candidate-reporting-only":
		return state == "starting-unverified"
	case "record-derived-unverified":
		return state == "starting" || state == "serving" || state == "lingering" || state == "draining" || state == "unverified-stale"
	case "attested":
		return state == "starting" || state == "serving" || state == "lingering" || state == "draining"
	}
	return false
}

// ValidateSharedRuntimeStatusJSON checks localruntime status and engineobservation
// transcription before publication, preserving the consumers' zero-value
// tolerances and additive status members.
func ValidateSharedRuntimeStatusJSON(data []byte, expectedEngine string) error {
	if err := validateStatusDocument(data, expectedEngine); err != nil {
		return sharedRuntimeError("status_contract_violation", err)
	}
	return nil
}

func statusObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("required status object is missing or null")
	}
	return fields, nil
}

func statusMember(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("required status member %s is missing or null", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("status member %s: %w", key, err)
	}
	return nil
}

// Consumer pkg/localruntime/decode.go:177-183, 332-338, 346-355: require
// presence, but let json.Unmarshal retain the consumer's null/zero semantics.
func statusConsumerMember(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok {
		return fmt.Errorf("required status member %s is missing", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("status member %s: %w", key, err)
	}
	return nil
}

// Consumer pkg/engineobservation/adapter.go:248,270 and strict.go:39-95:
// reject every outer duplicate and aliases of readings or provenance.
// Decode additive values as raw JSON; their nested keys are not checked here.
func statusOuterObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("status is not a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("status object key is not a string")
		}
		// Consumer pkg/engineobservation/strict.go:60-63: all outer keys.
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate JSON member %q", name)
		}
		// Readings consumer aliases and the producer-owned provenance alias
		// refuse even when the canonical member is absent.
		if name != "readings" && strings.EqualFold(name, "readings") || name != "provenance" && strings.EqualFold(name, "provenance") {
			return nil, fmt.Errorf("case-variant JSON member %q shadows readings or provenance", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	// Consumer pkg/engineobservation/strict.go:83-94: required readings and EOF.
	if _, ok := fields["readings"]; !ok {
		return nil, errors.New("missing required status readings")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("content follows status document")
		}
		return nil, err
	}
	return fields, nil
}

func validateStatusDocument(data []byte, expectedEngine string) error {
	fields, err := statusOuterObject(data)
	if err != nil {
		return err
	}
	if raw, present := fields["provenance"]; present {
		if err := validateStatusProvenance(raw); err != nil {
			return err
		}
	}
	// Consumer decode.go:125-159: integer v1, requested identity, runtime key,
	// and a present profile_digest (which may be empty).
	var version int
	if err := statusMember(fields, "contract_version", &version); err != nil {
		return err
	}
	if version != 1 {
		return errors.New("unsupported status contract_version")
	}
	// Consumer pkg/localruntime/decode.go:140-148: use struct decoding,
	// including case-insensitive field matching, then check name and key.
	var identity struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if err := statusConsumerMember(fields, "engine_identity", &identity); err != nil {
		return err
	}
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.Key) == "" || identity.Name != expectedEngine {
		return errors.New("status engine identity mismatch or empty")
	}
	var runtimeKey, digest string
	if err := statusMember(fields, "runtime_key", &runtimeKey); err != nil {
		return err
	}
	if strings.TrimSpace(runtimeKey) == "" {
		return errors.New("empty status runtime_key")
	}
	// Consumer pkg/localruntime/decode.go:157,346-355: null decodes to an empty string.
	if err := statusConsumerMember(fields, "profile_digest", &digest); err != nil {
		return err
	}
	// Consumer pkg/localruntime/decode.go:165-174: struct string decoding
	// followed by the frozen joint state/source set, with additive members ignored.
	var broker struct {
		State  string `json:"state"`
		Source string `json:"source"`
	}
	if err := statusConsumerMember(fields, "broker", &broker); err != nil {
		return err
	}
	if !validStatusBrokerPair(broker.State, broker.Source) {
		return errors.New("unsupported status broker state/source pair")
	}
	// Consumer pkg/localruntime/decode.go:177-187: sharing must be present;
	// null/omitted nested fields retain zero, while invalid types/negative max refuse.
	var sharing struct {
		Configured struct {
			MaxLeases int `json:"max_leases"`
		} `json:"configured"`
	}
	if err := statusConsumerMember(fields, "sharing", &sharing); err != nil {
		return err
	}
	if sharing.Configured.MaxLeases < 0 {
		return errors.New("negative status max_leases")
	}
	// Consumer pkg/localruntime/decode.go:290-306: absent/null runtime is valid;
	// pid uses Go int decoding (omitted/null/negative accepted, wrong types refuse).
	if raw, ok := fields["runtime"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var runtime struct {
			PID       int `json:"pid"`
			StartTime *struct {
				Seconds      *int64 `json:"seconds"`
				Microseconds *int32 `json:"microseconds"`
			} `json:"start_time"`
		}
		if err := json.Unmarshal(raw, &runtime); err != nil {
			return err
		}
		// Consumer pkg/localruntime/decode.go:299-324: start_time and its
		// components are non-null, seconds >= 0, microseconds in [0,999999].
		start := runtime.StartTime
		if start == nil || start.Seconds == nil || start.Microseconds == nil {
			return errors.New("missing status runtime start_time member")
		}
		if *start.Seconds < 0 || *start.Microseconds < 0 || *start.Microseconds > 999999 {
			return errors.New("invalid status runtime start_time")
		}
	}
	// Consumer pkg/localruntime/decode.go:331-340: require leases presence,
	// accept null or any array elements, and reject non-array non-null values.
	var leases []json.RawMessage
	if err := statusConsumerMember(fields, "leases", &leases); err != nil {
		return err
	}
	// Consumer decode.go:254-283, 377-453: three generations only; scalars
	// cannot be null, timestamps can be null but otherwise must be RFC3339.
	groups := [][]string{{"restart_count", "quarantined_until", "last_readiness_match", "manual_quarantine"}, {"restart_not_before", "half_open"}}
	counts := [2]int{}
	for i, group := range groups {
		for _, key := range group {
			if _, ok := fields[key]; ok {
				counts[i]++
			}
		}
	}
	if !(counts[0] == 0 && counts[1] == 0 || counts[0] == 4 && (counts[1] == 0 || counts[1] == 2)) {
		return errors.New("partial status restart cohort")
	}
	if _, ok := fields["restart_count"]; ok {
		var count int
		if err := statusMember(fields, "restart_count", &count); err != nil {
			return err
		}
		if count < 0 {
			return errors.New("negative status restart_count")
		}
	}
	for _, key := range []string{"manual_quarantine", "half_open"} {
		if _, ok := fields[key]; ok {
			var value bool
			if err := statusMember(fields, key, &value); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"restart_not_before", "quarantined_until", "last_readiness_match"} {
		if raw, ok := fields[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var value string
			if err := statusMember(fields, key, &value); err != nil {
				return err
			}
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				return err
			}
		}
	}
	return validateStatusReadings(fields["readings"])
}

// Consumer pkg/engineobservation/adapter.go:245-310, 344-389: validate
// transcription shape only. NotObserved is decodable and must not make the
// status guard run the downstream conformance/admission validator.
func validateStatusReadings(raw json.RawMessage) error {
	rowSpec := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"fact": nil, "outcome": nil, "value": nil, "cause": nil, "reason": nil, "source": nil,
	}, required: []string{"fact", "outcome", "source"}}
	spec := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"contract_version": nil, "facts": {elements: rowSpec},
	}, required: []string{"contract_version", "facts"}}
	if err := validateStrictJSON(raw, spec, true); err != nil {
		return err
	}
	fields, err := statusObject(raw)
	if err != nil {
		return err
	}
	var version string
	if err := statusMember(fields, "contract_version", &version); err != nil {
		return err
	}
	if version != "observed-process/v3" {
		return errors.New("unsupported status readings contract_version")
	}
	var rows []json.RawMessage
	if err := statusMember(fields, "facts", &rows); err != nil {
		return err
	}
	for _, raw := range rows {
		row, err := statusObject(raw)
		if err != nil {
			return err
		}
		for _, key := range []string{"fact", "outcome", "source", "value", "cause", "reason"} {
			if _, present := row[key]; present {
				var value string
				if err := statusMember(row, key, &value); err != nil {
					return err
				}
			}
		}
		var outcome string
		if err := statusMember(row, "outcome", &outcome); err != nil {
			return err
		}
		_, value := row["value"]
		_, cause := row["cause"]
		_, reason := row["reason"]
		switch outcome {
		case "observed-value":
			if !value || cause || reason {
				return errors.New("invalid observed-value status reading")
			}
		case "observed-absent":
			if value || cause || reason {
				return errors.New("invalid observed-absent status reading")
			}
		case "not-observed":
			var causeText, reasonText string
			if err := statusMember(row, "cause", &causeText); err != nil {
				return err
			}
			if err := statusMember(row, "reason", &reasonText); err != nil {
				return err
			}
			if value || reasonText == "" || causeText != "malformed" && causeText != "read-failure" && causeText != "unsupported" {
				return errors.New("invalid not-observed status reading")
			}
		default:
			return errors.New("unknown status reading outcome")
		}
	}
	return nil
}

func validateStatusProvenance(raw json.RawMessage) error {
	spec := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"contract_version": nil, "engines_config": nil, "engines_config_sha256": nil, "cache_root": nil,
	}, required: []string{"contract_version", "engines_config", "engines_config_sha256", "cache_root"}}
	if err := validateStrictJSON(raw, spec, true); err != nil {
		return err
	}
	fields, err := statusObject(raw)
	if err != nil {
		return err
	}
	var provenance StatusProvenance
	for key, target := range map[string]*string{
		"contract_version": &provenance.ContractVersion, "engines_config": &provenance.EnginesConfig,
		"engines_config_sha256": &provenance.EnginesConfigSHA256, "cache_root": &provenance.CacheRoot,
	} {
		if err := statusMember(fields, key, target); err != nil {
			return err
		}
	}
	if provenance.ContractVersion != StatusProvenanceContractVersion {
		return errors.New("unsupported status provenance contract_version")
	}
	for _, path := range []string{provenance.EnginesConfig, provenance.CacheRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("status provenance path must be absolute and clean")
		}
	}
	digest, err := hex.DecodeString(provenance.EnginesConfigSHA256)
	if err != nil || len(digest) != sha256.Size || strings.ToLower(provenance.EnginesConfigSHA256) != provenance.EnginesConfigSHA256 {
		return errors.New("status provenance digest must be lowercase SHA-256 hex")
	}
	return nil
}
