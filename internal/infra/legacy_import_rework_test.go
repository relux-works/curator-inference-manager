//go:build darwin

package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func legacyReworkEvidence(t *testing.T, o *LegacyImportOptions) string {
	t.Helper()
	entry := writeLegacyPiEvidenceFixture(t, "ok", "")
	o.EvidenceRoot = filepath.Join(filepath.Dir(o.HomeDir), "evidence")
	if err := os.Mkdir(o.EvidenceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(o.EvidenceRoot, "terminal")
	if err := os.Rename(entry, moved); err != nil {
		t.Fatal(err)
	}
	return moved
}

func TestImportLegacyEngineStateAdmissionRevalidatedThroughCommit(t *testing.T) {
	// Production propagation sites mapped by the AST evidence generator.
	_ = "legacyRecoveryAdmission"
	_ = "admission"
	for _, point := range []string{"before-staging", "after-backup", "after-replacement-1", "after-replacement-2"} {
		for _, namespace := range []string{"legacy", "manager", "lifecycle"} {
			t.Run(point+"/"+namespace, func(t *testing.T) {
				o := legacyImportFixture(t)
				entry := legacyReworkEvidence(t, &o)
				curator := filepath.Join(o.HomeDir, ".curator")
				if err := os.Mkdir(curator, 0700); err != nil {
					t.Fatal(err)
				}
				catalog, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
				referent := filepath.Join(o.HomeDir, "original.toml")
				if err := os.WriteFile(referent, []byte(legacyInitialCatalog), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(referent, catalog); err != nil {
					t.Fatal(err)
				}
				before, data, err := legacySnapshot(catalog)
				if err != nil {
					t.Fatal(err)
				}
				original := legacyImportCheckpoint
				t.Cleanup(func() { legacyImportCheckpoint = original })
				var lock *os.File
				t.Cleanup(func() {
					if lock != nil {
						_ = lock.Close()
					}
				})
				fired := false
				var activeRoot string
				var activeTree map[string]string
				originalRename := legacyRename
				t.Cleanup(func() { legacyRename = originalRename })
				legacyRename = func(from, to string) error {
					if fired && point == "after-backup" && to == filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json") {
						t.Fatal("recovery changed journal before active admission check")
					}
					if fired && (to == catalog || to == filepath.Join(curator, "terminal-evidence.json")) {
						state, candidate, err := legacySnapshot(from)
						if err != nil {
							t.Fatal(err)
						}
						if state.Kind == "file" && (bytes.Contains(candidate, []byte("[engines.qwen]")) || to != catalog) {
							t.Fatal("replacement attempted while active admission held")
						}
					}
					return originalRename(from, to)
				}
				legacyImportCheckpoint = func(checkpoint string) error {
					if checkpoint != point || fired {
						return nil
					}
					fired = true
					if namespace == "lifecycle" {
						activeRoot = entry
					} else {
						activeRoot = filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
						if namespace == "manager" {
							activeRoot = filepath.Join(o.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimePathVersion, "new")
						}
						if err := os.MkdirAll(activeRoot, 0700); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					lockName := "broker.lock"
					if namespace == "lifecycle" {
						lockName = "active.lock"
					}
					lock, err = os.OpenFile(filepath.Join(activeRoot, lockName), os.O_CREATE|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
					activeTree = legacyTree(t, activeRoot)
					return nil
				}
				_, err = ImportLegacyEngineState(o, "apply")
				if err == nil {
					t.Fatal("active lock admitted during transaction")
				}
				if !fired {
					t.Fatal("checkpoint not exercised")
				}
				got, gotData, snapshotErr := legacySnapshot(catalog)
				if snapshotErr != nil || !legacyStatesEqual(got, before) || !bytes.Equal(gotData, data) {
					t.Fatalf("partial import not restored: %v %v", got, snapshotErr)
				}
				evidencePath, _ := legacyTargetPath(o.HomeDir, legacyEvidenceTarget)
				if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("created evidence not removed: %v", err)
				}
				if !reflect.DeepEqual(activeTree, legacyTree(t, activeRoot)) {
					t.Fatal("active evidence/process lock touched")
				}
				check, err := os.OpenFile(lock.Name(), os.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = check.Close() }()
				if err := syscall.Flock(int(check.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					t.Fatal("active process lock released")
				}
				for _, action := range []string{"preview", "apply", "resume", "rollback"} {
					if _, err := ImportLegacyEngineState(o, action); err == nil {
						t.Fatalf("%s accepted active state", action)
					}
				}
			})
		}
	}
}

func TestImportLegacyEngineStateJournalExactCanonicalMembers(t *testing.T) {
	for _, level := range []string{"journal", "target", "before", "after"} {
		var sample any = legacyJournalBase()
		if level == "target" {
			sample = legacyJournalBase().Targets[0]
		}
		if level == "before" || level == "after" {
			sample = legacyJournalBase().Targets[0].Before
		}
		raw, _ := json.Marshal(sample)
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		for key := range keys {
			for _, shape := range []string{"case", "unknown"} {
				t.Run(level+"/"+key+"/"+shape, func(t *testing.T) {
					o := legacyImportFixture(t)
					legacyApply(t, o)
					path := filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var document map[string]any
					_ = json.Unmarshal(data, &document)
					object := document
					if level != "journal" {
						object = document["targets"].([]any)[0].(map[string]any)
					}
					if level == "before" || level == "after" {
						object = object[level].(map[string]any)
					}
					variant := strings.ToUpper(key[:1]) + key[1:]
					if shape == "unknown" {
						variant = "__mutant__"
					}
					object[variant] = object[key]
					data, _ = json.Marshal(document)
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					before := legacyTree(t, o.HomeDir)
					for _, action := range []string{"preview", "apply", "resume", "rollback"} {
						_, err := ImportLegacyEngineState(o, action)
						requireLegacySite(t, err, "journal.decode")
						if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
							t.Fatal("journal refusal wrote files")
						}
					}
				})
			}
		}
	}
}

func TestImportLegacyEngineStateCredentialArgvRefusesWithoutDisclosure(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		site string
	}{}
	for _, flag := range []string{"--api-key", "--API_KEY", "--token", "--secret", "--password", "--auth", "--bearer", "--credential", "--hf-token"} {
		cases = append(cases, struct {
			name string
			argv []string
			site string
		}{flag + "/space", []string{flag, "synthetic-sensitive-value"}, "profiles.credential_flag"})
		cases = append(cases, struct {
			name string
			argv []string
			site string
		}{flag + "/equals", []string{flag + "=synthetic-sensitive-value"}, "profiles.credential_flag"})
	}
	for _, value := range []string{"sk-synthetic-rework", "ghp_synthetic", "xoxb-synthetic", "xoxp-synthetic", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ0ZXN0In0.c2ln"} {
		cases = append(cases, struct {
			name string
			argv []string
			site string
		}{value, []string{value}, "profiles.credential_value"})
		cases = append(cases, struct {
			name string
			argv []string
			site string
		}{"equals/" + value, []string{"--data=" + value}, "profiles.credential_value"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyApply(t, o)
			raw, err := os.ReadFile(o.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := toml.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			profile := document["profiles"].(map[string]any)["qwen"].(map[string]any)
			argv := []string{"serve", "--host", "{host}", "--port", "{port}"}
			argv = append(argv, tc.argv...)
			profile["argv"] = argv
			data, err := toml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(o.ConfigPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, o.HomeDir)
			for _, action := range []string{"preview", "apply", "resume", "rollback"} {
				report, err := ImportLegacyEngineState(o, action)
				requireLegacySite(t, err, tc.site)
				rendered, _ := json.Marshal(report)
				for _, secret := range []string{"synthetic-sensitive-value", "sk-synthetic-rework", "ghp_synthetic", "xoxb-synthetic", "xoxp-synthetic", "eyJhbGciOiJub25lIn0"} {
					if strings.Contains(err.Error(), secret) || bytes.Contains(rendered, []byte(secret)) {
						t.Fatal("preview exposed credential value")
					}
				}
				if tc.site == "profiles.credential_flag" && !strings.Contains(err.Error(), "credential flag") {
					t.Fatal("preview omitted safe flag classification")
				}
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
					t.Fatal("credential refusal wrote target/staging")
				}
			}
		})
	}
}

func TestImportLegacyEngineStateInventoriesEveryLifecycleChild(t *testing.T) {
	for _, shape := range []string{"socket", "fifo", "file", "directory", "symlink", "record-symlink", "log-directory", "active-fifo"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			entry := legacyReworkEvidence(t, &o)
			legacyApply(t, o)
			path := filepath.Join(entry, "__mutant__")
			switch shape {
			case "socket":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte("unclassified"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("record.json", path); err != nil {
					t.Fatal(err)
				}
			default:
				name := strings.Split(shape, "-")[0]
				switch name {
				case "active":
					name = "active.lock"
				case "record":
					name = "record.json"
				default:
					name = "log.jsonl"
				}
				path = filepath.Join(entry, name)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if shape == "record-symlink" {
					if err := os.Symlink("log.jsonl", path); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "log-directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "active-fifo" {
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := legacyTree(t, o.HomeDir)
			source := legacyTree(t, o.EvidenceRoot)
			for _, action := range []string{"preview", "apply", "resume", "rollback"} {
				_, err := ImportLegacyEngineState(o, action)
				var refusal *PiTerminalEvidenceError
				if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable {
					t.Fatalf("unsafe child accepted: %v", err)
				}
				if shape == "file" || shape == "socket" || shape == "fifo" || shape == "directory" || shape == "symlink" {
					assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.child_unknown")
				}
			}
			if _, err := ReadLegacyPiLifecycleEvidence(o.EvidenceRoot); err == nil {
				t.Fatal("compatibility reader admitted unknown child")
			}
			if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) || !reflect.DeepEqual(source, legacyTree(t, o.EvidenceRoot)) {
				t.Fatal("unsafe child refusal mutated state")
			}
		})
	}
}

func TestImportLegacyEngineStateLifecycleJSONExactMembers(t *testing.T) {
	for _, object := range []string{"record", "event", "summary"} {
		var sample any = legacyPiEvidenceRecord{}
		if object == "event" {
			sample = legacyPiEvidenceEvent{Fields: map[string]json.RawMessage{}}
		}
		if object == "summary" {
			sample = PiTerminalEvidenceSummary{}
		}
		raw, _ := json.Marshal(sample)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		for key := range fields {
			for _, shape := range []string{"case", "unknown", "duplicate", "trailing"} {
				t.Run(object+"/"+key+"/"+shape, func(t *testing.T) {
					o := legacyImportFixture(t)
					entry := legacyReworkEvidence(t, &o)
					if object == "summary" {
						legacyApply(t, o)
					}
					path := filepath.Join(entry, "record.json")
					if object == "event" {
						path = filepath.Join(entry, "log.jsonl")
					}
					if object == "summary" {
						path, _ = legacyTargetPath(o.HomeDir, legacyEvidenceTarget)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var value map[string]any
					var logStart []byte
					if object == "event" {
						lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
						logStart, data = lines[0], lines[1]
					}
					if object == "summary" {
						var values []map[string]any
						_ = json.Unmarshal(data, &values)
						value = values[0]
					} else {
						_ = json.Unmarshal(bytes.TrimSpace(data), &value)
					}
					variant := strings.ToUpper(key[:1]) + key[1:]
					if shape == "unknown" {
						variant = "__mutant__"
					}
					if shape == "case" || shape == "unknown" {
						value[variant] = value[key]
					}
					changed, _ := json.Marshal(value)
					if shape == "duplicate" {
						member, _ := json.Marshal(value[key])
						changed = append(changed[:len(changed)-1], []byte(",\""+key+"\":"+string(member)+"}")...)
					}
					if shape == "trailing" {
						changed = append(changed, []byte(" true")...)
					}
					if object == "summary" {
						changed = append(append([]byte{'['}, changed...), ']')
					}
					if object == "event" {
						changed = append(append(append(logStart, '\n'), changed...), '\n')
					}
					if err := os.WriteFile(path, changed, 0600); err != nil {
						t.Fatal(err)
					}
					if object == "event" {
						mutatePiEvidenceRecord(t, entry, func(r *legacyPiEvidenceRecord) { r.CommittedBytes = int64(len(changed)) })
					}
					if object == "summary" {
						j, err := legacyReadJournal(o.HomeDir)
						if err != nil {
							t.Fatal(err)
						}
						for i := range j.Targets {
							target := &j.Targets[i]
							if target.TargetID == legacyEvidenceTarget {
								target.After.SHA256 = legacyString(legacyDigest(changed))
								if err := os.WriteFile(filepath.Join(legacyTransactionRoot(o.HomeDir), j.TransactionID+"-"+target.TargetID+"-after"), changed, 0600); err != nil {
									t.Fatal(err)
								}
							}
						}
						if err := legacyWriteJournal(legacyTransactionRoot(o.HomeDir), *j); err != nil {
							t.Fatal(err)
						}
					}
					before := legacyTree(t, o.HomeDir)
					for _, action := range []string{"preview", "apply"} {
						_, err := ImportLegacyEngineState(o, action)
						if object == "summary" {
							requireLegacySite(t, err, "destination.evidence_decode")
						} else {
							site := "entry.record"
							if object == "event" {
								site = "entry.terminal_status"
							}
							assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, site)
						}
						if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
							t.Fatal("closed JSON refusal wrote target")
						}
					}
				})
			}
		}
	}
}

func TestImportLegacyEngineStateLifecycleChildInventoryReadFailure(t *testing.T) {
	o := legacyImportFixture(t)
	entry := legacyReworkEvidence(t, &o)
	previous := statPiEvidenceDirectoryFile
	statPiEvidenceDirectoryFile = func(file *os.File) (os.FileInfo, error) {
		info, err := file.Stat()
		if err := os.Chmod(entry, 0300); err != nil {
			t.Fatal(err)
		}
		return info, err
	}
	t.Cleanup(func() { statPiEvidenceDirectoryFile = previous; _ = os.Chmod(entry, 0700) })
	_, err := ImportLegacyEngineState(o, "preview")
	if err == nil {
		t.Fatal("unreadable child inventory accepted")
	}
	assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.children_read")
}

func TestImportLegacyEngineStateLateSourceChangeRequiresWholeDigest(t *testing.T) {
	for _, point := range []string{"after-backup", "after-replacement-1"} {
		t.Run(point, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyReworkEvidence(t, &o)
			beforeSource, err := os.ReadFile(o.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			previous := legacyImportCheckpoint
			fired := false
			legacyImportCheckpoint = func(checkpoint string) error {
				if checkpoint != point || fired {
					return nil
				}
				fired = true
				j, err := legacyReadJournal(o.HomeDir)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 1024; i++ {
					data := bytes.Replace(beforeSource, []byte("org/qwen"), []byte(fmt.Sprintf("org/changed-%d", i)), 1)
					if err := os.WriteFile(o.ConfigPath, data, 0600); err != nil {
						t.Fatal(err)
					}
					plan, err := legacyPlan(o, j)
					if err != nil {
						t.Fatal(err)
					}
					if plan.report.SourceSHA256[:1] == j.SourceSHA256[:1] && plan.report.SourceSHA256 != j.SourceSHA256 {
						return nil
					}
				}
				t.Fatal("failed to construct equal-prefix/different-digest source")
				return nil
			}
			t.Cleanup(func() { legacyImportCheckpoint = previous })
			_, err = ImportLegacyEngineState(o, "apply")
			requireLegacySite(t, err, "recovery.source")
			if !fired {
				t.Fatal("source change not exercised")
			}
			for _, id := range []string{legacyCatalogTarget, legacyEvidenceTarget} {
				path, _ := legacyTargetPath(o.HomeDir, id)
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("changed source retained partial target %s: %v", id, err)
				}
			}
		})
	}
}

func TestImportLegacyEngineStateClosedJSONEncodingAndTrailingBytes(t *testing.T) {
	// Map the helper's encoding and trailing-token gates to their public callers.
	_ = "journal.utf8"
	_ = "journal.trailing"
	for _, source := range []string{"record", "event"} {
		for _, shape := range []string{"raw-invalid-utf8", "string-invalid-utf8", "trailing-value"} {
			t.Run(source+"/"+shape, func(t *testing.T) {
				o := legacyImportFixture(t)
				entry := legacyReworkEvidence(t, &o)
				path := filepath.Join(entry, "record.json")
				if source == "event" {
					path = filepath.Join(entry, "log.jsonl")
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if shape == "raw-invalid-utf8" {
					data = []byte{0xff}
				}
				if shape == "string-invalid-utf8" {
					data = bytes.Replace(data, []byte("2026"), []byte{'2', '0', '2', 0xff}, 1)
				}
				if shape == "trailing-value" {
					data = append(data, []byte("true")...)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if source == "event" {
					mutatePiEvidenceRecord(t, entry, func(r *legacyPiEvidenceRecord) { r.CommittedBytes = int64(len(data)) })
				}
				before := legacyTree(t, o.HomeDir)
				for _, action := range []string{"preview", "apply"} {
					_, err := ImportLegacyEngineState(o, action)
					if err == nil {
						t.Fatal("malformed closed JSON admitted")
					}
					var refusal *PiTerminalEvidenceError
					if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable {
						t.Fatalf("malformed encoding not unknown: %v", err)
					}
					if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
						t.Fatal("malformed JSON wrote targets")
					}
				}
			})
		}
	}
}

func TestImportLegacyEngineStateLifecyclePartialReadAndMetadataBoundaries(t *testing.T) {
	for _, shape := range []string{"directory-path-stat", "directory-file-stat", "active-stat", "directory-fstat", "active-identity", "log-identity", "record-read", "directory-mode-0701", "entry-id-33", "uppercase-id"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			entry := legacyReworkEvidence(t, &o)
			pathStat, fileStatHook, activeStat, directoryStat, identity, recordRead := statPiEvidenceDirectoryPath, statPiEvidenceDirectoryFile, fstatPiEvidenceActiveLock, fstatPiEvidenceDirectory, statPiEvidenceFile, readPiEvidenceRecord
			t.Cleanup(func() {
				statPiEvidenceDirectoryPath = pathStat
				statPiEvidenceDirectoryFile = fileStatHook
				fstatPiEvidenceActiveLock = activeStat
				fstatPiEvidenceDirectory = directoryStat
				statPiEvidenceFile = identity
				readPiEvidenceRecord = recordRead
			})
			switch shape {
			case "directory-path-stat":
				statPiEvidenceDirectoryPath = func(path string, stat *syscall.Stat_t) error {
					if err := syscall.Stat(path, stat); err != nil {
						return err
					}
					return os.ErrPermission
				}
			case "directory-file-stat":
				statPiEvidenceDirectoryFile = func(file *os.File) (os.FileInfo, error) {
					info, err := file.Stat()
					if err != nil {
						return info, err
					}
					return info, os.ErrPermission
				}
			case "active-stat":
				fstatPiEvidenceActiveLock = func(fd int, stat *syscall.Stat_t) error {
					if err := syscall.Fstat(fd, stat); err != nil {
						return err
					}
					return os.ErrPermission
				}
			case "directory-fstat":
				fstatPiEvidenceDirectory = func(fd int, stat *syscall.Stat_t) error {
					if err := syscall.Fstat(fd, stat); err != nil {
						return err
					}
					return os.ErrPermission
				}
			case "active-identity", "log-identity":
				statPiEvidenceFile = func(file *os.File) (piEvidenceFileIdentity, error) {
					value, err := fileStat(file)
					if err != nil {
						return value, err
					}
					target := "active.lock"
					if shape == "log-identity" {
						target = "log.jsonl"
					}
					if filepath.Base(file.Name()) == target {
						return value, os.ErrPermission
					}
					return value, nil
				}
			case "record-read":
				readPiEvidenceRecord = func(reader io.Reader) ([]byte, error) {
					data, err := io.ReadAll(reader)
					if err != nil {
						return data, err
					}
					return data, os.ErrPermission
				}
			case "directory-mode-0701":
				if err := os.Chmod(entry, 0701); err != nil {
					t.Fatal(err)
				}
			case "entry-id-33":
				mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) { record.EntryID = strings.Repeat("a", 33) })
			case "uppercase-id":
				mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) { record.EntryID = strings.Repeat("A", 32) })
			}
			before := legacyTree(t, o.HomeDir)
			site := "entry.record"
			switch shape {
			case "directory-path-stat", "directory-file-stat", "directory-mode-0701":
				site = "entry.directory"
			case "active-stat":
				site = "entry.active_lock"
			case "directory-fstat", "active-identity", "log-identity":
				site = "entry.identity"
			}
			for _, action := range []string{"preview", "apply"} {
				_, err := ImportLegacyEngineState(o, action)
				if err == nil {
					t.Fatal("partial/failed metadata read or invalid boundary admitted")
				}
				var refusal *PiTerminalEvidenceError
				if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable {
					t.Fatalf("read failure not unknown: %v", err)
				}
				assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, site)
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
					t.Fatal("metadata refusal wrote targets")
				}
			}
		})
	}
}

func TestImportLegacyEngineStateSourceSymlinkNarrowingBoundary(t *testing.T) {
	o := legacyImportFixture(t)
	if err := os.Remove(o.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("not-read", o.ConfigPath); err != nil {
		t.Fatal(err)
	}
	before := legacyTree(t, o.HomeDir)
	for _, action := range []string{"preview", "apply"} {
		_, err := ImportLegacyEngineState(o, action)
		if err == nil {
			t.Fatal("source symlink admitted")
		}
		requireLegacySite(t, err, "source.symlink")
		if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
			t.Fatal("source symlink refusal wrote")
		}
	}
}

func TestImportLegacyEngineStateLifecycleCommittedEnvelopeBoundary(t *testing.T) {
	for _, member := range []string{"bytes", "records"} {
		t.Run(member, func(t *testing.T) {
			o := legacyImportFixture(t)
			entry := legacyReworkEvidence(t, &o)
			mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) {
				if member == "bytes" {
					record.CommittedBytes--
				} else {
					record.CommittedRecords--
				}
			})
			site := "entry.log"
			if member == "records" {
				site = "entry.terminal_status"
			}
			before := legacyTree(t, o.HomeDir)
			for _, action := range []string{"preview", "apply"} {
				_, err := ImportLegacyEngineState(o, action)
				if err == nil {
					t.Fatal("off-by-one committed envelope admitted")
				}
				assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, site)
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
					t.Fatal("envelope refusal wrote target")
				}
			}
		})
	}
}

func TestImportLegacyEngineStateLifecycleSchemaSingleton(t *testing.T) {
	for _, action := range []string{"preview", "apply", "resume", "rollback"} {
		t.Run(action, func(t *testing.T) {
			o := legacyImportFixture(t)
			entry := legacyReworkEvidence(t, &o)
			if action == "resume" || action == "rollback" {
				legacyApply(t, o)
			}
			mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) { record.SchemaVersion = 2 })
			beforeHome, beforeCache, beforeEvidence := legacyTree(t, o.HomeDir), legacyTree(t, o.CacheRoot), legacyTree(t, o.EvidenceRoot)
			_, err := ImportLegacyEngineState(o, action)
			if err == nil {
				t.Fatal("unsupported lifecycle schema admitted through import " + action)
			}
			assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.record")
			if !reflect.DeepEqual(beforeHome, legacyTree(t, o.HomeDir)) || !reflect.DeepEqual(beforeCache, legacyTree(t, o.CacheRoot)) || !reflect.DeepEqual(beforeEvidence, legacyTree(t, o.EvidenceRoot)) {
				t.Fatal("schema refusal changed source or manager state")
			}
		})
	}
}
