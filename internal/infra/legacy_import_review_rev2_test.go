//go:build darwin

package infra

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReviewRev2ActiveAfterBackup(t *testing.T) {
	o := legacyImportFixture(t)
	prev := legacyImportCheckpoint
	t.Cleanup(func() { legacyImportCheckpoint = prev })
	legacyImportCheckpoint = func(point string) error {
		if point == "after-backup" {
			root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "broker-state.json"), []byte(`{"pid":123}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	report, err := ImportLegacyEngineState(o, "apply")
	if err == nil {
		t.Fatalf("active/unclassifiable old state appeared after backup but import installed targets: %+v", report)
	}
}

func TestReviewRev2JournalUnknownCaseFields(t *testing.T) {
	for _, shape := range []string{"top", "target", "state"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyApply(t, o)
			p := filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "top":
				data = bytes.Replace(data, []byte(`"phase":"committed"`), []byte(`"phase":"committed","Phase":"committed"`), 1)
			case "target":
				data = bytes.Replace(data, []byte(`"targetId":"engine-catalog"`), []byte(`"targetId":"engine-catalog","TargetId":"engine-catalog"`), 1)
			case "state":
				data = bytes.Replace(data, []byte(`"mode":0`), []byte(`"mode":0,"Mode":0`), 1)
			}
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ImportLegacyEngineState(o, "preview"); err == nil {
				t.Fatal("unknown JSON member accepted through production import preview")
			}
		})
	}
}

func TestReviewRev2CredentialArgv(t *testing.T) {
	o := legacyImportFixture(t)
	data, err := os.ReadFile(o.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("argv=['serve',"), []byte("argv=['serve','--api-key','synthetic-review-credential',"), 1)
	if err := os.WriteFile(o.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = ImportLegacyEngineState(o, "apply")
	if err != nil {
		return
	}
	target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
	out, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("synthetic-review-credential")) {
		t.Fatal("credential in legacy argv copied into manager catalog and transaction staging")
	}
}

func TestReviewRev2NestedLifecycleSocket(t *testing.T) {
	o := legacyImportFixture(t)
	entry := writeLegacyPiEvidenceFixture(t, "ok", "")
	root := filepath.Join(filepath.Dir(o.HomeDir), "evidence")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "terminal")
	if err := os.Rename(entry, moved); err != nil {
		t.Fatal(err)
	}
	o.EvidenceRoot = root
	listener, err := net.Listen("unix", filepath.Join(moved, "active.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	summaries, readErr := ReadLegacyPiLifecycleEvidence(root)
	_, importErr := ImportLegacyEngineState(o, "apply")
	if readErr == nil || importErr == nil {
		t.Fatalf("active socket inside terminal directory accepted: summaries=%d reader_error=%v import_error=%v", len(summaries), readErr, importErr)
	}
}

func TestReviewRev2HeldLockAfterBackup(t *testing.T) {
	o := legacyImportFixture(t)
	previous := legacyImportCheckpoint
	t.Cleanup(func() { legacyImportCheckpoint = previous })
	var lock *os.File
	t.Cleanup(func() {
		if lock != nil {
			_ = lock.Close()
		}
	})
	legacyImportCheckpoint = func(point string) error {
		if point == "after-backup" {
			dir := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			var err error
			lock, err = os.OpenFile(filepath.Join(dir, "broker.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if _, err := ImportLegacyEngineState(o, "apply"); err == nil {
		t.Fatal("import applied while an actual legacy broker flock was held after backup")
	}
}
