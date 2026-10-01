//go:build !windows

package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Structured runtime evidence cannot be attested cold by its filename. Only
// layout-declared logs are opaque; other unknown children refuse in the scanner.
func legacyValidateColdState(path string) error {
	state, data, err := legacySnapshot(path)
	if err != nil {
		return err
	}
	if state.Kind != "file" {
		return legacyImportRefuse("state.structured_shape", errors.New("regular state file required"))
	}
	if filepath.Base(path) == sharedRuntimeBrokerStateName {
		var record SharedBrokerRecord
		if err := legacyDecodeRuntimeState(data, &record); err != nil {
			return err
		}
		// Broker records have no terminal state in the owner protocol. Even a
		// stale PID is not permission to claim, kill, or relabel its identity.
		return legacyImportRefuse("state.unclassifiable", errors.New("active record, socket, or unknown state"))
	}
	var ledger SharedRuntimeRestartLedger
	if err := legacyDecodeRuntimeState(data, &ledger); err != nil {
		return err
	}
	if ledger.Schema != sharedRuntimeRestartLedgerSchema || !piStateKeyPattern.MatchString(ledger.RuntimeKey) || !legacyDigestPattern.MatchString(ledger.ProfileDigest) {
		return legacyImportRefuse("state.ledger_identity", errors.New("invalid restart ledger identity"))
	}
	// Rendezvous uses a 32-byte prefix; the full runtime directory uses 64.
	name := filepath.Base(filepath.Dir(path))
	if name != ledger.RuntimeKey && name != fmt.Sprintf("%.32s", ledger.RuntimeKey) {
		return legacyImportRefuse("state.ledger_path", errors.New("restart ledger directory identity differs"))
	}
	if ledger.RestartCount < 0 || len(ledger.FailureHistory) > sharedRuntimeFailureHistoryLimit {
		return legacyImportRefuse("state.ledger_counters", errors.New("invalid restart ledger counters"))
	}
	now := time.Now()
	for _, stamp := range []*time.Time{ledger.RestartNotBefore, ledger.QuarantinedUntil, ledger.LastReadinessMatch} {
		if stamp != nil && stamp.IsZero() {
			return legacyImportRefuse("state.ledger_time", errors.New("invalid restart ledger timestamp"))
		}
	}
	previous := time.Time{}
	for _, event := range ledger.FailureHistory {
		if event.OccurredAt.IsZero() || event.OccurredAt.Before(previous) || event.RestartCount < 1 || event.BackoffSeconds < 0 || event.Quarantined != (event.QuarantinedUntil != nil) {
			return legacyImportRefuse("state.ledger_history", errors.New("invalid restart failure history"))
		}
		if event.QuarantinedUntil != nil && event.QuarantinedUntil.Before(event.OccurredAt) {
			return legacyImportRefuse("state.ledger_history_time", errors.New("invalid restart quarantine chronology"))
		}
		if event.QuarantinedUntil != nil && event.QuarantinedUntil.After(now) {
			return legacyImportRefuse("state.ledger_active", errors.New("restart history quarantine window is open"))
		}
		if event.OccurredAt.After(now) {
			return legacyImportRefuse("state.ledger_active", errors.New("restart history is not terminal"))
		}
		previous = event.OccurredAt
	}
	if ledger.RestartCount > 0 && len(ledger.FailureHistory) == 0 && !ledger.ManualQuarantine {
		return legacyImportRefuse("state.ledger_active", errors.New("restart count has no terminal evidence"))
	}
	if len(ledger.FailureHistory) != 0 && !ledger.FailureHistory[len(ledger.FailureHistory)-1].Quarantined && !ledger.ManualQuarantine {
		return legacyImportRefuse("state.ledger_active", errors.New("restart window has no terminal quarantine evidence"))
	}
	// Pending retries (even overdue), half-open attempts and readiness without
	// terminal evidence cannot prove inactivity. Historical expired quarantine
	// and manual quarantine alone do not claim a running process.
	if ledger.RestartNotBefore != nil || ledger.HalfOpen || ledger.LastReadinessMatch != nil || (ledger.QuarantinedUntil != nil && ledger.QuarantinedUntil.After(now)) {
		return legacyImportRefuse("state.ledger_active", errors.New("restart ledger is active or unclassifiable"))
	}
	return nil
}

func legacyDecodeRuntimeState(data []byte, destination any) error {
	if err := legacyDecodeClosedJSON(data, destination); err != nil {
		return legacyImportRefuse("state.structured_json", err)
	}
	return legacyRequireRuntimeStateMembers(data, reflect.TypeOf(destination).Elem())
}

// Derive required members from the owner types, including nested state. Null
// is allowed only where the owner writer can emit it (pointers and slices).
func legacyRequireRuntimeStateMembers(data []byte, kind reflect.Type) error {
	for kind.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		kind = kind.Elem()
	}
	if reflect.PointerTo(kind).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	switch kind.Kind() {
	case reflect.Struct:
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return legacyImportRefuse("state.structured_members", err)
		}
		for i := 0; i < kind.NumField(); i++ {
			field := kind.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			value, present := raw[tag[0]]
			if !present && len(tag) == 1 {
				return legacyImportRefuse("state.structured_required", errors.New("missing state member"))
			}
			if present {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && field.Type.Kind() != reflect.Pointer && field.Type.Kind() != reflect.Slice {
					return legacyImportRefuse("state.structured_null", errors.New("null state member"))
				}
				if err := legacyRequireRuntimeStateMembers(value, field.Type); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return legacyImportRefuse("state.structured_members", err)
		}
		for _, value := range values {
			if err := legacyRequireRuntimeStateMembers(value, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
