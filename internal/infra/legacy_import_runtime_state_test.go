//go:build darwin

package infra

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func legacyColdLedgerFixture() SharedRuntimeRestartLedger {
	return newSharedRuntimeRestartLedger(strings.Repeat("a", 64), strings.Repeat("b", 64))
}

func legacyLedgerRoot(o LegacyImportOptions, namespace string) string {
	key := legacyColdLedgerFixture().RuntimeKey
	switch namespace {
	case "legacy":
		return filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", key)
	case "legacy-rv":
		return filepath.Join(o.CacheRoot, "agents-infra", "pi-rv", key[:32])
	case "manager-rv":
		return filepath.Join(o.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimeRendezvousName, key[:32])
	default:
		return filepath.Join(o.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimePathVersion, key)
	}
}

func TestImportLegacyEngineStateColdRuntimeLedgerValidation(t *testing.T) {
	// Explicit site names keep the AST-derived production witness selection narrow.
	_ = []string{"state.structured_shape", "state.ledger_identity", "state.ledger_path", "state.ledger_counters", "state.ledger_time", "state.ledger_history", "state.ledger_history_time", "state.ledger_active", "state.structured_json", "state.structured_members", "state.structured_required", "state.structured_null"}
	base, err := json.Marshal(legacyColdLedgerFixture())
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string][]byte{
		"malformed": []byte("{malformed"), "null": []byte("null"),
		"wrong-root-type": []byte("[]"), "trailing": append(append([]byte(nil), base...), '1'),
		"duplicate": bytesReplaceLedger(base, `"restart_count":0`, `"restart_count":0,"restart_count":0`),
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(base, &raw); err != nil {
		t.Fatal(err)
	}
	for name := range raw {
		for _, change := range []string{"missing", "case", "null", "type"} {
			if change == "null" && (name == "restart_not_before" || name == "quarantined_until" || name == "last_readiness_match" || name == "failure_history") {
				continue
			}
			copy := map[string]json.RawMessage{}
			for key, value := range raw {
				copy[key] = value
			}
			switch change {
			case "missing":
				delete(copy, name)
			case "case":
				copy[strings.ToUpper(name[:1])+name[1:]] = copy[name]
				delete(copy, name)
			case "null":
				copy[name] = json.RawMessage("null")
			case "type":
				copy[name] = json.RawMessage(`{}`)
			}
			encoded, err := json.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			vectors[change+"/"+name] = encoded
		}
	}
	// Identity and value guards, independently of the JSON shape guards.
	for _, item := range []struct {
		name   string
		mutate func(*SharedRuntimeRestartLedger)
	}{
		{"schema", func(l *SharedRuntimeRestartLedger) { l.Schema = "__mutant__" }},
		{"key", func(l *SharedRuntimeRestartLedger) { l.RuntimeKey = "../outside" }},
		{"profile", func(l *SharedRuntimeRestartLedger) { l.ProfileDigest = "../outside" }},
		{"path-key", func(l *SharedRuntimeRestartLedger) { l.RuntimeKey = strings.Repeat("c", 64) }},
		{"negative-count", func(l *SharedRuntimeRestartLedger) { l.RestartCount = -1 }},
		{"unclassified-count", func(l *SharedRuntimeRestartLedger) { l.RestartCount = 1 }},
		{"open-history-quarantine", func(l *SharedRuntimeRestartLedger) {
			stamp := time.Unix(4102444800, 0)
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(1, 0), RestartCount: 1, Quarantined: true, QuarantinedUntil: &stamp}}
		}},
		{"history-limit", func(l *SharedRuntimeRestartLedger) {
			for range sharedRuntimeFailureHistoryLimit + 1 {
				l.FailureHistory = append(l.FailureHistory, SharedRuntimeFailureEvent{OccurredAt: time.Unix(1, 0), RestartCount: 1})
			}
		}},
		{"zero-restart-time", func(l *SharedRuntimeRestartLedger) { l.RestartNotBefore = new(time.Time) }},
		{"zero-quarantine-time", func(l *SharedRuntimeRestartLedger) { l.QuarantinedUntil = new(time.Time) }},
		{"zero-readiness-time", func(l *SharedRuntimeRestartLedger) { l.LastReadinessMatch = new(time.Time) }},
		{"zero-event-time", func(l *SharedRuntimeRestartLedger) { l.FailureHistory = []SharedRuntimeFailureEvent{{RestartCount: 1}} }},
		{"event-order", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(2, 0), RestartCount: 1}, {OccurredAt: time.Unix(1, 0), RestartCount: 1}}
		}},
		{"event-count", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(1, 0)}}
		}},
		{"event-backoff", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(1, 0), RestartCount: 1, BackoffSeconds: -1}}
		}},
		{"event-quarantine", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(1, 0), RestartCount: 1, Quarantined: true}}
		}},
		{"event-quarantine-time", func(l *SharedRuntimeRestartLedger) {
			stamp := time.Unix(1, 0)
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(2, 0), RestartCount: 1, Quarantined: true, QuarantinedUntil: &stamp}}
		}},
		{"future-event", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(4102444800, 0), RestartCount: 1}}
		}},
		{"pending-restart", func(l *SharedRuntimeRestartLedger) { stamp := time.Unix(4102444800, 0); l.RestartNotBefore = &stamp }},
		{"overdue-restart", func(l *SharedRuntimeRestartLedger) { stamp := time.Unix(1, 0); l.RestartNotBefore = &stamp }},
		{"half-open", func(l *SharedRuntimeRestartLedger) { l.HalfOpen = true }},
		{"open-window", func(l *SharedRuntimeRestartLedger) {
			l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Now().Add(-time.Second), RestartCount: 1}}
		}},
		{"readiness", func(l *SharedRuntimeRestartLedger) { stamp := time.Unix(1, 0); l.LastReadinessMatch = &stamp }},
		{"future-readiness", func(l *SharedRuntimeRestartLedger) { stamp := time.Unix(4102444800, 0); l.LastReadinessMatch = &stamp }},
		{"open-quarantine", func(l *SharedRuntimeRestartLedger) { stamp := time.Unix(4102444800, 0); l.QuarantinedUntil = &stamp }},
	} {
		l := legacyColdLedgerFixture()
		item.mutate(&l)
		if len(l.FailureHistory) > 0 {
			switch item.name {
			case "history-limit":
				for i := range l.FailureHistory {
					stamp := time.Unix(2, 0)
					l.FailureHistory[i].Quarantined = true
					l.FailureHistory[i].QuarantinedUntil = &stamp
				}
			case "open-window":
				// No terminal marker: recent history cannot attest a cold runtime.
			case "future-event":
				stamp := time.Unix(4102444801, 0)
				l.FailureHistory[0].Quarantined = true
				l.FailureHistory[0].QuarantinedUntil = &stamp
			default:
				stamp := time.Unix(4, 0)
				l.FailureHistory = append(l.FailureHistory, SharedRuntimeFailureEvent{OccurredAt: time.Unix(3, 0), RestartCount: 1, Quarantined: true, QuarantinedUntil: &stamp})
			}
		}
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		vectors[item.name] = data
	}
	// Nested known state fields are closed and required too.
	for _, object := range []string{
		`{"occurred_at":"1970-01-01T00:00:01Z","restart_count":1,"quarantined":false,"Unknown":true}`,
		`{"Occurred_at":"1970-01-01T00:00:01Z","restart_count":1,"quarantined":false}`,
		`{"occurred_at":"1970-01-01T00:00:01Z","quarantined":false}`,
		`{"occurred_at":"1970-01-01T00:00:01Z","restart_count":1,"quarantined":false,"quarantined_until":"yesterday"}`,
	} {
		vectors[object] = bytesReplaceLedger(base, `"failure_history":null`, `"failure_history":[`+object+`,{"occurred_at":"1970-01-01T00:00:03Z","restart_count":1,"quarantined":true,"quarantined_until":"1970-01-01T00:00:04Z"}]`)
	}
	for _, namespace := range []string{"legacy", "legacy-rv", "manager", "manager-rv"} {
		o := legacyImportFixture(t)
		legacyApply(t, o)
		root := legacyLedgerRoot(o, namespace)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		for name, data := range vectors {
			t.Run(name+"/"+namespace, func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(root, sharedRuntimeRestartLedgerName), data, 0600); err != nil {
					t.Fatal(err)
				}
				before := legacyTree(t, filepath.Dir(o.HomeDir))
				for _, action := range []string{"preview", "apply", "resume", "rollback"} {
					_, err := ImportLegacyEngineState(o, action)
					if err == nil {
						t.Fatalf("%s admitted unclassifiable ledger", action)
					}
				}
				if !reflect.DeepEqual(before, legacyTree(t, filepath.Dir(o.HomeDir))) {
					t.Fatal("refusal changed persisted state")
				}
			})
		}
	}
}

func bytesReplaceLedger(data []byte, before, after string) []byte {
	return []byte(strings.Replace(string(data), before, after, 1))
}

func TestImportLegacyEngineStateColdRuntimeLedgerAcceptsTerminalHistory(t *testing.T) {
	for _, namespace := range []string{"legacy", "legacy-rv", "manager", "manager-rv"} {
		for _, kind := range []string{"fresh", "expired", "manual", "history-limit"} {
			t.Run(namespace+"/"+kind, func(t *testing.T) {
				o := legacyImportFixture(t)
				root := legacyLedgerRoot(o, namespace)
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				l := legacyColdLedgerFixture()
				if kind != "fresh" {
					stamp := time.Unix(2, 0)
					l.RestartCount = 1
					l.QuarantinedUntil = &stamp
					l.FailureHistory = []SharedRuntimeFailureEvent{{OccurredAt: time.Unix(1, 0), RestartCount: 1, Quarantined: true, QuarantinedUntil: &stamp}}
				}
				l.ManualQuarantine = kind == "manual"
				if kind == "history-limit" {
					event := l.FailureHistory[0]
					for len(l.FailureHistory) < sharedRuntimeFailureHistoryLimit {
						l.FailureHistory = append(l.FailureHistory, event)
					}
				}
				data, err := json.Marshal(l)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, sharedRuntimeRestartLedgerName), data, 0600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{sharedRuntimeBrokerLogName, sharedRuntimeLogName} {
					if err := os.WriteFile(filepath.Join(root, name), []byte("{opaque"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				before := legacyTree(t, root)
				for _, action := range []string{"preview", "apply", "apply", "resume", "rollback"} {
					if _, err := ImportLegacyEngineState(o, action); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(before, legacyTree(t, root)) {
					t.Fatal("cold state was rewritten")
				}
			})
		}
	}
}

func TestImportLegacyEngineStateColdRuntimeLedgerOwnerEntrypoints(t *testing.T) {
	for _, name := range []string{sharedRuntimeRestartLedgerName, sharedRuntimeBrokerStateName, "unknown-state.json"} {
		t.Run(name, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyApply(t, o)
			root := legacyLedgerRoot(o, "legacy")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("{malformed"), 0600); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, filepath.Dir(o.HomeDir))
			owner := SharedRuntimeOperatorOptions{HomeDir: o.HomeDir, CacheRoot: o.CacheRoot, ProjectDir: o.ProjectDir, Profile: "qwen"}
			if _, err := AcquireSharedRuntimeLease(context.Background(), owner, "", os.Environ()); err == nil {
				t.Fatal("lease/ensure admitted malformed state")
			}
			if _, err := StopSharedRuntime(owner, false, time.Second); err == nil {
				t.Fatal("stop/recovery admitted malformed state")
			}
			if _, err := SharedRuntimeStatusReport(owner); err == nil {
				t.Fatal("status reported malformed state as absent")
			}
			if !reflect.DeepEqual(before, legacyTree(t, filepath.Dir(o.HomeDir))) {
				t.Fatal("owner entrypoint touched state")
			}
		})
	}
}

func TestImportLegacyEngineStateColdRuntimeStructuredShapes(t *testing.T) {
	_ = "state.structured_shape"
	_ = "state.unclassifiable"
	_ = "state.ledger_path"
	for _, shape := range []string{"symlink", "broker", "directory-identity"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			root := legacyLedgerRoot(o, "legacy")
			if shape == "directory-identity" {
				root = filepath.Join(filepath.Dir(root), "mutant")
			}
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, sharedRuntimeRestartLedgerName)
			data, err := json.Marshal(legacyColdLedgerFixture())
			if err != nil {
				t.Fatal(err)
			}
			if shape == "broker" {
				path = filepath.Join(root, sharedRuntimeBrokerStateName)
				data, err = json.Marshal(SharedBrokerRecord{State: "serving", ProtocolVersion: SharedRuntimeProtocolVersion})
				if err != nil {
					t.Fatal(err)
				}
			}
			if shape == "symlink" {
				if err := os.Symlink(string(data), path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, filepath.Dir(o.HomeDir))
			for _, action := range []string{"preview", "apply"} {
				if _, err := ImportLegacyEngineState(o, action); err == nil {
					t.Fatal("structured state admitted", shape)
				}
			}
			if !reflect.DeepEqual(before, legacyTree(t, filepath.Dir(o.HomeDir))) {
				t.Fatal("structured state changed")
			}
		})
	}
}

func TestImportLegacyEngineStateColdRuntimeLedgerAppearsBeforeReplacement(t *testing.T) {
	for _, point := range []string{"before-staging", "after-backup", "after-replacement-1"} {
		t.Run(point, func(t *testing.T) {
			o := legacyImportFixture(t)
			original := legacyImportCheckpoint
			t.Cleanup(func() { legacyImportCheckpoint = original })
			var late map[string]string
			legacyImportCheckpoint = func(checkpoint string) error {
				if checkpoint != point {
					return nil
				}
				root := legacyLedgerRoot(o, "manager")
				if err := os.MkdirAll(root, 0700); err != nil {
					return err
				}
				ledger := legacyColdLedgerFixture()
				stamp := time.Unix(4102444800, 0)
				ledger.RestartNotBefore = &stamp
				data, err := json.Marshal(ledger)
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(root, sharedRuntimeRestartLedgerName), data, 0600); err != nil {
					return err
				}
				late = legacyTree(t, o.CacheRoot)
				return nil
			}
			if _, err := ImportLegacyEngineState(o, "apply"); err == nil {
				t.Fatal("late restart window admitted")
			}
			if late == nil {
				t.Fatal("checkpoint not reached")
			}
			if !reflect.DeepEqual(late, legacyTree(t, o.CacheRoot)) {
				t.Fatal("late state changed")
			}
			target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("late admission did not restore absent target", err)
			}
		})
	}
}

func TestRefuseActiveLegacyEngineStateUnknownSiblingProductionAdmission(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	root := legacyLedgerRoot(o, "legacy")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unknown-state.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	requireLegacySite(t, RefuseActiveLegacyEngineState(o.CacheRoot), "state.unclassifiable")
	if _, err := resolveSharedProfile(o.ProjectDir, o.HomeDir, o.CacheRoot, "qwen"); err == nil {
		t.Fatal("resolver admitted unknown sibling")
	}
	if _, err := ImportLegacyEngineState(o, "preview"); err == nil {
		t.Fatal("unknown sibling admitted through import")
	}
}
