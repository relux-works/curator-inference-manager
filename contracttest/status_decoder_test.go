// Local faithful copy of skill-agents-management@4510df7 pkg/localruntime/decode.go.
// Only the returned domain Status construction is removed; decoding rules are
// retained, including consumer zero-value tolerances. Producer tests additionally
// enforce explicit max_leases/pid presence and non-null leases.
package contracttest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrDecodeFailure = errors.New("localruntime: status response failed to decode")

var (
	restartStatusPreExtensionFields = []string{
		"restart_count",
		"quarantined_until",
		"last_readiness_match",
		"manual_quarantine",
	}
	restartStatusCurrentOnlyFields = []string{
		"restart_not_before",
		"half_open",
	}
)

const curatorEnginesStatusContractVersion = 1

type brokerPair struct {
	state  string
	source BrokerObservationSource
}

// Consumer pkg/localruntime/decode.go:51-66 (origin/main 4510df7).
var frozenBrokerPairs = map[brokerPair]bool{
	{"absent", SourceDetermined}: true,

	{"starting-unverified", SourceCandidateOnly}: true,

	{"starting", SourceRecordUnverified}:         true,
	{"serving", SourceRecordUnverified}:          true,
	{"lingering", SourceRecordUnverified}:        true,
	{"draining", SourceRecordUnverified}:         true,
	{"unverified-stale", SourceRecordUnverified}: true,

	{"starting", SourceAttested}:  true,
	{"serving", SourceAttested}:   true,
	{"lingering", SourceAttested}: true,
	{"draining", SourceAttested}:  true,
}

func ValidBrokerPair(state string, source BrokerObservationSource) bool {
	return frozenBrokerPairs[brokerPair{state: state, source: source}]
}

type wireBroker struct {
	State  string `json:"state"`
	Source string `json:"source"`
}

type wireEngineIdentity struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type wireSharingConfigured struct {
	MaxLeases int `json:"max_leases"`
}

type wireSharing struct {
	Configured wireSharingConfigured `json:"configured"`
}

type wireRuntime struct {
	PID       int                   `json:"pid"`
	StartTime *wireProcessStartTime `json:"start_time"`
}

type wireProcessStartTime struct {
	Seconds      *int64 `json:"seconds"`
	Microseconds *int32 `json:"microseconds"`
}

// Consumer pkg/localruntime/decode.go:121-199: required envelope and identity.
func decodeConsumerStatus(raw []byte, expectedEngine string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("%w: response is not a JSON object: %v", ErrDecodeFailure, err)
	}
	versionRaw, ok := fields["contract_version"]
	if !ok {
		return fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "contract_version")
	}
	var version int
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return fmt.Errorf("%w: field %q is not an integer: %v", ErrDecodeFailure, "contract_version", err)
	}
	if version != curatorEnginesStatusContractVersion {
		return fmt.Errorf("%w: unsupported curator-engines status contract_version %d (want %d)", ErrDecodeFailure, version, curatorEnginesStatusContractVersion)
	}
	engineRaw, ok := fields["engine_identity"]
	if !ok {
		return fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "engine_identity")
	}
	var engine wireEngineIdentity
	if err := json.Unmarshal(engineRaw, &engine); err != nil {
		return fmt.Errorf("%w: field %q is not an engine identity object: %v", ErrDecodeFailure, "engine_identity", err)
	}
	if strings.TrimSpace(engine.Name) == "" || strings.TrimSpace(engine.Key) == "" {
		return fmt.Errorf("%w: engine_identity requires non-empty name and key", ErrDecodeFailure)
	}
	if engine.Name != expectedEngine {
		return fmt.Errorf("%w: requested engine %q but curator-engines reported %q", ErrDecodeFailure, expectedEngine, engine.Name)
	}

	runtimeKey, err := requiredNonEmptyString(fields, "runtime_key")
	if err != nil {
		return err
	}
	_ = runtimeKey // structurally required and validated; this adapter does not otherwise use it

	if _, err := requiredString(fields, "profile_digest"); err != nil {
		return err
	}

	brokerRaw, ok := fields["broker"]
	if !ok {
		return fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "broker")
	}
	var broker wireBroker
	if err := json.Unmarshal(brokerRaw, &broker); err != nil {
		return fmt.Errorf("%w: field %q is not an object with string state/source: %v", ErrDecodeFailure, "broker", err)
	}
	if strings.TrimSpace(broker.State) == "" || strings.TrimSpace(broker.Source) == "" {
		return fmt.Errorf("%w: %q requires a non-empty state and source", ErrDecodeFailure, "broker")
	}
	if !ValidBrokerPair(broker.State, BrokerObservationSource(broker.Source)) {
		return fmt.Errorf("%w: (%s, %s) is not one of the frozen, jointly-reachable broker (state, source) pairs",
			ErrDecodeFailure, broker.State, broker.Source)
	}

	sharingRaw, ok := fields["sharing"]
	if !ok {
		return fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "sharing")
	}
	var sharing wireSharing
	if err := json.Unmarshal(sharingRaw, &sharing); err != nil {
		return fmt.Errorf("%w: field %q is not an object with sharing.configured.max_leases: %v", ErrDecodeFailure, "sharing", err)
	}
	if sharing.Configured.MaxLeases < 0 {
		return fmt.Errorf("%w: sharing.configured.max_leases is negative", ErrDecodeFailure)
	}

	_, _, err = decodeOptionalRuntime(fields)
	if err != nil {
		return err
	}

	_, err = decodeLeases(fields)
	if err != nil {
		return err
	}
	if err := validateRestartStatusCohort(fields); err != nil {
		return err
	}

	// Consumer decode.go:201-240: validate each present restart scalar.
	if _, _, err := optionalNonNegativeInt(fields, "restart_count"); err != nil {
		return err
	}
	for _, key := range []string{"restart_not_before", "quarantined_until", "last_readiness_match"} {
		if _, _, err := optionalNullableTimestamp(fields, key); err != nil {
			return err
		}
	}
	for _, key := range []string{"manual_quarantine", "half_open"} {
		if _, _, err := optionalBool(fields, key); err != nil {
			return err
		}
	}

	return nil
}

// Consumer pkg/localruntime/decode.go:254-283: all-or-nothing generations.
func validateRestartStatusCohort(fields map[string]json.RawMessage) error {
	present := func(keys []string) int {
		count := 0
		for _, key := range keys {
			if _, ok := fields[key]; ok {
				count++
			}
		}
		return count
	}

	preCount := present(restartStatusPreExtensionFields)
	currentOnlyCount := present(restartStatusCurrentOnlyFields)
	switch {
	case preCount == 0 && currentOnlyCount == 0:
		return nil
	case preCount == len(restartStatusPreExtensionFields) && currentOnlyCount == 0:
		return nil
	case preCount == len(restartStatusPreExtensionFields) && currentOnlyCount == len(restartStatusCurrentOnlyFields):
		return nil
	default:
		return fmt.Errorf("%w: restart status fields form a partial cohort (pre-extension %d/%d, current-only %d/%d)",
			ErrDecodeFailure, preCount, len(restartStatusPreExtensionFields), currentOnlyCount, len(restartStatusCurrentOnlyFields))
	}
}

// Consumer pkg/localruntime/decode.go:285-326: nullable runtime and start-time bounds.
func decodeOptionalRuntime(fields map[string]json.RawMessage) (pid int, startedAt time.Time, err error) {
	raw, ok := fields["runtime"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return 0, time.Time{}, nil
	}
	var wr wireRuntime
	if err := json.Unmarshal(raw, &wr); err != nil {
		return 0, time.Time{}, fmt.Errorf("%w: field %q is present but not an object with pid/start_time: %v", ErrDecodeFailure, "runtime", err)
	}
	if wr.StartTime == nil {
		return 0, time.Time{}, fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "runtime.start_time")
	}
	parsed, err := decodeProcessStartTime(*wr.StartTime)
	if err != nil {
		return 0, time.Time{}, err
	}
	return wr.PID, parsed, nil
}

func decodeProcessStartTime(start wireProcessStartTime) (time.Time, error) {
	if start.Seconds == nil {
		return time.Time{}, fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "runtime.start_time.seconds")
	}
	if start.Microseconds == nil {
		return time.Time{}, fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "runtime.start_time.microseconds")
	}
	if *start.Seconds < 0 {
		return time.Time{}, fmt.Errorf("%w: field %q is negative", ErrDecodeFailure, "runtime.start_time.seconds")
	}
	if *start.Microseconds < 0 {
		return time.Time{}, fmt.Errorf("%w: field %q is negative", ErrDecodeFailure, "runtime.start_time.microseconds")
	}
	if *start.Microseconds > 999999 {
		return time.Time{}, fmt.Errorf("%w: field %q exceeds 999999", ErrDecodeFailure, "runtime.start_time.microseconds")
	}
	return time.Unix(*start.Seconds, int64(*start.Microseconds)*1000), nil
}

// Consumer pkg/localruntime/decode.go:328-341: required leases array.
func decodeLeases(fields map[string]json.RawMessage) (int, error) {
	raw, ok := fields["leases"]
	if !ok {
		return 0, fmt.Errorf("%w: missing field %q", ErrDecodeFailure, "leases")
	}
	var leases []json.RawMessage
	if err := json.Unmarshal(raw, &leases); err != nil {
		return 0, fmt.Errorf("%w: field %q is not an array: %v", ErrDecodeFailure, "leases", err)
	}
	return len(leases), nil
}

func requiredString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("%w: missing field %q", ErrDecodeFailure, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: field %q is not a string: %v", ErrDecodeFailure, key, err)
	}
	return value, nil
}

func requiredNonEmptyString(fields map[string]json.RawMessage, key string) (string, error) {
	value, err := requiredString(fields, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: field %q is empty", ErrDecodeFailure, key)
	}
	return value, nil
}

func optionalNonNegativeInt(fields map[string]json.RawMessage, key string) (value int, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return 0, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, true, fmt.Errorf("%w: field %q is null, not an integer", ErrDecodeFailure, key)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("%w: field %q is not an integer: %v", ErrDecodeFailure, key, err)
	}
	if value < 0 {
		return 0, true, fmt.Errorf("%w: field %q is negative", ErrDecodeFailure, key)
	}
	return value, true, nil
}

func optionalNullableTimestamp(fields map[string]json.RawMessage, key string) (value *time.Time, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, true, fmt.Errorf("%w: field %q is not null or an RFC3339 string: %v", ErrDecodeFailure, key, err)
	}
	parsed, err := time.Parse(time.RFC3339, encoded)
	if err != nil {
		return nil, true, fmt.Errorf("%w: field %q is not an RFC3339 timestamp: %v", ErrDecodeFailure, key, err)
	}
	return &parsed, true, nil
}

func optionalBool(fields map[string]json.RawMessage, key string) (value bool, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return false, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return false, true, fmt.Errorf("%w: field %q is null, not a boolean", ErrDecodeFailure, key)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, fmt.Errorf("%w: field %q is not a boolean: %v", ErrDecodeFailure, key, err)
	}
	return value, true, nil
}

type BrokerObservationSource string

const (
	SourceDetermined       BrokerObservationSource = "determined"
	SourceCandidateOnly    BrokerObservationSource = "kernel-candidate-reporting-only"
	SourceRecordUnverified BrokerObservationSource = "record-derived-unverified"
	SourceAttested         BrokerObservationSource = "attested"
)
