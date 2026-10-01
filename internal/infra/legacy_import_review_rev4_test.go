//go:build darwin

package infra

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A named persisted state file is not proof that its contents are classifiable.
func TestReviewRev4MalformedColdLedger(t *testing.T) {
	for _, namespace := range []string{"legacy", "manager"} {
		for _, action := range []string{"preview", "apply", "resume", "rollback"} {
			t.Run(namespace+"/"+action, func(t *testing.T) {
				o := legacyImportFixture(t)
				if action == "resume" || action == "rollback" {
					legacyApply(t, o)
				}
				root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
				if namespace == "manager" {
					root = filepath.Join(o.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimePathVersion, "unclassifiable")
				}
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "restart-ledger.json"), []byte("{malformed"), 0600); err != nil {
					t.Fatal(err)
				}
				before := legacyTree(t, o.CacheRoot)
				_, err := ImportLegacyEngineState(o, action)
				if !reflect.DeepEqual(before, legacyTree(t, o.CacheRoot)) {
					t.Fatal("cache changed")
				}
				if err == nil {
					t.Fatal("malformed persisted lifecycle state classified as cold through public import " + action)
				}
			})
		}
	}
}

// Preserve the filename inventory, but replace a known child's regular-file
// shape; this must remain refused before any source bytes can be imported.
func TestReviewRev4LifecycleKnownNameHardlink(t *testing.T) {
	o := legacyImportFixture(t)
	entry := legacyReworkEvidence(t, &o)
	source := filepath.Join(entry, "record.json")
	if err := os.Link(source, filepath.Join(filepath.Dir(o.HomeDir), "second-link")); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"preview", "apply"} {
		if _, err := ImportLegacyEngineState(o, action); err == nil {
			t.Fatal("multiply-linked record admitted")
		}
	}
}

// Case and escaped spelling checks use semantic keys, not raw byte substrings.
func TestReviewRev4EscapedJournalKey(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	p := filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"phase":"committed"`), []byte(`"phase":"committed","\u0050hase":"committed"`), 1)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyEngineState(o, "preview"); err == nil {
		t.Fatal("escaped case-variant key admitted")
	}
}

func TestReviewRev4MalformedLegacyLedgerStatus(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "restart-ledger.json"), []byte("{malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{HomeDir: o.HomeDir, CacheRoot: o.CacheRoot, ProjectDir: o.ProjectDir, Profile: "qwen"})
	if err == nil {
		t.Fatalf("malformed legacy ledger did not refuse status: broker=%s availability=%s", status.Broker.State, status.EngineAvailability)
	}
}
