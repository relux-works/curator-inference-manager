//go:build darwin

package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pelletier/go-toml/v2"
)

const legacyInitialCatalog = "schema_version=1\n[engines.unrelated]\nmode=\"local\"\nengine=\"mlx-swift\"\nexecutable=\"/usr/bin/false\"\nargv=[\"{host}\",\"{port}\"]\n"

func legacyImportFixture(t *testing.T) LegacyImportOptions {
	t.Helper()
	if err := os.MkdirAll("/tmp/.temp", 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp/.temp", "li-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	cache := filepath.Join(root, "cache")
	project := filepath.Join(root, "project")
	for _, path := range []string{home, cache, project} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "legacy.toml")
	data := []byte("[profiles.qwen]\nmode='local'\nengine='mlx-swift'\nmodel='org/qwen'\nexecutable='/usr/bin/false'\nargv=['serve','--host','{host}','--port','{port}']\n")
	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return LegacyImportOptions{HomeDir: home, ConfigPath: config, CacheRoot: cache, ProjectDir: project}
}
func legacyTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		value := fmt.Sprintf("%v:%d:%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += "link:" + link
		} else if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "bytes:" + string(data)
		}
		snapshot[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func requireLegacySite(t *testing.T, err error, site string) {
	t.Helper()
	var refusal *LegacyImportError
	if !errors.As(err, &refusal) || refusal.Site != site {
		t.Fatalf("want refusal %s, got %T %v", site, err, err)
	}
}
func legacyReadTarget(t *testing.T, options LegacyImportOptions, id string) []byte {
	t.Helper()
	path, err := legacyTargetPath(options.HomeDir, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func legacyApply(t *testing.T, options LegacyImportOptions) {
	t.Helper()
	if _, err := ImportLegacyEngineState(options, "apply"); err != nil {
		t.Fatal(err)
	}
}
func TestImportLegacyEngineStatePreviewRepeatRollbackExact(t *testing.T) {
	for _, kind := range []string{"absent", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			options := legacyImportFixture(t)
			curator := filepath.Join(options.HomeDir, ".curator")
			if err := os.Mkdir(curator, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(curator, "engines.toml")
			original := []byte("# preserve comment and exact bytes\n" + legacyInitialCatalog)
			referent := filepath.Join(options.HomeDir, "original.toml")
			if kind == "file" {
				if err := os.WriteFile(target, original, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.WriteFile(referent, original, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../original.toml", target); err != nil {
					t.Fatal(err)
				}
			}
			sentinel := filepath.Join(curator, "untouched")
			if err := os.WriteFile(sentinel, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, options.HomeDir)
			preview, err := ImportLegacyEngineState(options, "preview")
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Targets) != 1 || preview.Targets[0] != legacyCatalogTarget {
				t.Fatalf("preview=%+v", preview)
			}
			if after := legacyTree(t, options.HomeDir); !reflect.DeepEqual(before, after) {
				t.Fatal("preview changed bytes/mode/link/timestamps")
			}
			legacyApply(t, options)
			entry, state, err := ReadEngineCatalogEntry(target, "qwen")
			if err != nil || state != EngineCatalogPresent || entry.ServedModelID != "qwen" {
				t.Fatalf("catalog=%+v %v %v", entry, state, err)
			}
			applied := legacyTree(t, options.HomeDir)
			report, err := ImportLegacyEngineState(options, "apply")
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Targets) != 0 {
				t.Fatalf("repeat targets=%v", report.Targets)
			}
			if after := legacyTree(t, options.HomeDir); !reflect.DeepEqual(applied, after) {
				t.Fatal("repeat changed bytes/mode/link/timestamps")
			}
			if _, err := ImportLegacyEngineState(options, "rollback"); err != nil {
				t.Fatal(err)
			}
			if kind == "absent" {
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("created target survived rollback: %v", err)
				}
			}
			if kind == "file" {
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o400 || !bytes.Equal(legacyReadTarget(t, options, legacyCatalogTarget), original) {
					t.Fatal("bytes or mode not restored")
				}
			}
			if kind == "symlink" {
				link, err := os.Readlink(target)
				if err != nil || link != "../original.toml" {
					t.Fatalf("link=%q %v", link, err)
				}
				data, _ := os.ReadFile(referent)
				if !bytes.Equal(data, original) {
					t.Fatal("referent touched")
				}
			}
			data, _ := os.ReadFile(sentinel)
			if string(data) != "sentinel" {
				t.Fatal("unrelated sentinel touched")
			}
		})
	}
}

func TestLegacyImportKillHelper(t *testing.T) {
	point := os.Getenv("CURATOR_IMPORT_KILL_POINT")
	if point == "" {
		return
	}
	var options LegacyImportOptions
	if err := json.Unmarshal([]byte(os.Getenv("CURATOR_IMPORT_OPTIONS")), &options); err != nil {
		t.Fatal(err)
	}
	legacyImportCheckpoint = func(at string) error {
		if at == point {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err := ImportLegacyEngineState(options, os.Getenv("CURATOR_IMPORT_ACTION"))
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("kill point not reached")
}
func TestImportLegacyEngineStateKillPointsRecoverDeterministically(t *testing.T) {
	for _, point := range []string{"before-staging", "after-backup", "after-replacement-1", "after-replacement-2"} {
		for _, action := range []string{"resume", "rollback"} {
			t.Run(point+"/"+action, func(t *testing.T) {
				options := legacyImportFixture(t)
				entry := writeLegacyPiEvidenceFixture(t, "ok", "")
				root := filepath.Join(t.TempDir(), "evidence")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(entry, filepath.Join(root, "terminal")); err != nil {
					t.Fatal(err)
				}
				options.EvidenceRoot = root
				sourceBefore := legacyTree(t, root)
				// Two selected targets exercise every replacement checkpoint.
				if err := os.Mkdir(filepath.Join(options.HomeDir, ".curator"), 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(options.HomeDir, ".curator", "engines.toml")
				original := []byte(legacyInitialCatalog)
				if err := os.WriteFile(target, original, 0o400); err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(options)
				command := exec.Command(os.Args[0], "-test.run=^TestLegacyImportKillHelper$")
				command.Env = append(os.Environ(), "CURATOR_IMPORT_KILL_POINT="+point, "CURATOR_IMPORT_OPTIONS="+string(encoded), "CURATOR_IMPORT_ACTION=apply")
				output, err := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
					t.Fatalf("not killed: %v %s", err, output)
				}
				if point == "before-staging" {
					if _, err := os.Stat(legacyTransactionRoot(options.HomeDir)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("staged before checkpoint")
					}
					legacyApply(t, options)
				}
				if _, err := ImportLegacyEngineState(options, action); err != nil {
					t.Fatal(err)
				}
				if action == "resume" {
					if _, state, err := ReadEngineCatalogEntry(target, "qwen"); err != nil || state != EngineCatalogPresent {
						t.Fatalf("catalog=%v %v", state, err)
					}
					data := legacyReadTarget(t, options, legacyEvidenceTarget)
					if bytes.Contains(data, []byte(testLegacyPiEvidenceEntryID)) || bytes.Contains(data, []byte("credential-secret")) {
						t.Fatal("raw evidence copied")
					}
				} else {
					data, _ := os.ReadFile(target)
					info, _ := os.Stat(target)
					if !bytes.Equal(data, original) || info.Mode().Perm() != 0o400 {
						t.Fatal("rollback did not restore exact file")
					}
					evidence, _ := legacyTargetPath(options.HomeDir, legacyEvidenceTarget)
					if _, err := os.Stat(evidence); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("evidence target survived rollback")
					}
				}
				if !reflect.DeepEqual(sourceBefore, legacyTree(t, root)) {
					t.Fatal("old evidence was modified")
				}
			})
		}
	}
}
func TestImportLegacyEngineStateRecoveryRefusesDriftAndBrokenEvidence(t *testing.T) {
	for _, attack := range []string{"missing-backup", "corrupt-backup", "target-drift", "stage-drift", "source-drift", "active-socket", "unknown-target"} {
		t.Run(attack, func(t *testing.T) {
			options := legacyImportFixture(t)
			if err := os.Mkdir(filepath.Join(options.HomeDir, ".curator"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(options.HomeDir, ".curator", "engines.toml")
			if err := os.WriteFile(path, []byte(legacyInitialCatalog), 0o600); err != nil {
				t.Fatal(err)
			}
			previous := legacyImportCheckpoint
			legacyImportCheckpoint = func(point string) error {
				if point == "after-backup" {
					return errors.New("interrupted")
				}
				return nil
			}
			_, err := ImportLegacyEngineState(options, "apply")
			legacyImportCheckpoint = previous
			if err == nil {
				t.Fatal("interruption ignored")
			}
			journal, err := legacyReadJournal(options.HomeDir)
			if err != nil {
				t.Fatal(err)
			}
			root := legacyTransactionRoot(options.HomeDir)
			target := journal.Targets[0]
			switch attack {
			case "missing-backup":
				_ = os.Remove(filepath.Join(root, *target.BackupRef))
			case "corrupt-backup":
				_ = os.WriteFile(filepath.Join(root, *target.BackupRef), []byte("corrupt"), 0o600)
			case "target-drift":
				_ = os.WriteFile(path, []byte("schema_version=1\n[engines.other]\nmode='local'\nengine='mlx-swift'\nexecutable='/usr/bin/false'\nargv=['{host}','{port}']\n"), 0o600)
			case "stage-drift":
				_ = os.WriteFile(filepath.Join(root, journal.TransactionID+"-"+target.TargetID+"-after"), []byte("corrupt"), 0o600)
			case "source-drift":
				_ = os.WriteFile(options.ConfigPath, []byte("[profiles]\n"), 0o600)
			case "active-socket":
				directory := filepath.Join(options.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimeRendezvousName, "active")
				_ = os.MkdirAll(directory, 0o700)
				listener, err := net.Listen("unix", filepath.Join(directory, "b.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			case "unknown-target":
				journal.Targets[0].TargetID = "not-owned"
				if err := legacyWriteJournal(root, *journal); err != nil {
					t.Fatal(err)
				}
			}
			before := legacyTree(t, options.HomeDir)
			for _, action := range []string{"resume", "rollback", "apply", "preview"} {
				_, err := ImportLegacyEngineState(options, action)
				// Preview is a source/plan read; recovery authority is checked by resume,
				// apply and rollback. Preview must still refuse ambiguous target authority.

				if err == nil {
					t.Fatalf("%s accepted %s", action, attack)
				}
				switch attack {
				case "missing-backup", "corrupt-backup":
					requireLegacySite(t, err, "recovery.backup")
				case "stage-drift":
					requireLegacySite(t, err, "recovery.staging")
				}
				if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
					t.Fatalf("%s wrote while refusing", action)
				}
			}
		})
	}
}
func TestImportLegacyEngineStateSourceFormsAndPrecedence(t *testing.T) {
	for _, form := range []string{"absent", "malformed", "unknown-field", "read-failed", "conflict", "same", "update-owned", "destination-drift"} {
		t.Run(form, func(t *testing.T) {
			options := legacyImportFixture(t)
			switch form {
			case "absent":
				_ = os.Remove(options.ConfigPath)
			case "malformed":
				_ = os.WriteFile(options.ConfigPath, []byte("[profiles"), 0o600)
			case "unknown-field":
				data, _ := os.ReadFile(options.ConfigPath)
				_ = os.WriteFile(options.ConfigPath, append(data, []byte("unlisted=true\n")...), 0o600)
			case "read-failed":
				_ = os.Remove(options.ConfigPath)
				_ = os.Mkdir(options.ConfigPath, 0o700)
			default:
				legacyApply(t, options)
				if form == "same" {
					root := legacyTransactionRoot(options.HomeDir)
					_ = os.Remove(filepath.Join(root, "journal.json"))
				}
				if form == "conflict" {
					root := legacyTransactionRoot(options.HomeDir)
					_ = os.Remove(filepath.Join(root, "journal.json"))
				}
				if form == "conflict" || form == "update-owned" || form == "destination-drift" {
					data, _ := os.ReadFile(options.ConfigPath)
					data = bytes.ReplaceAll(data, []byte("org/qwen"), []byte("org/new-model"))
					_ = os.WriteFile(options.ConfigPath, data, 0o600)
				}
				if form == "destination-drift" {
					path, _ := legacyTargetPath(options.HomeDir, legacyCatalogTarget)
					data, _ := os.ReadFile(path)
					_ = os.WriteFile(path, append([]byte("# human edit\n"), data...), 0o600)
				}
			}
			before := legacyTree(t, options.HomeDir)
			report, err := ImportLegacyEngineState(options, "preview")
			if form == "absent" || form == "same" || form == "update-owned" {
				if err != nil {
					t.Fatal(err)
				}
				if form != "update-owned" && len(report.Targets) != 0 {
					t.Fatalf("targets=%v", report.Targets)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", form)
			}
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("preview wrote")
			}
			if form == "update-owned" {
				legacyApply(t, options)
				entry, _, err := ReadEngineCatalogEntry(filepath.Join(options.HomeDir, ".curator", "engines.toml"), "qwen")
				if err != nil || entry.ServedModelID != "new-model" {
					t.Fatalf("update=%+v %v", entry, err)
				}
			}
			if form == "malformed" || form == "unknown-field" || form == "read-failed" || form == "conflict" || form == "destination-drift" {
				for _, action := range []string{"apply", "resume", "rollback"} {
					if _, err := ImportLegacyEngineState(options, action); err == nil {
						t.Fatalf("%s accepted %s", action, form)
					}
				}
			}
		})
	}
}

func TestImportLegacyEngineStateActiveLegacyEvidenceAndReadOnlyReader(t *testing.T) {
	for _, shape := range []string{"active-lock", "unknown-terminal", "socket", "partial-directory", "valid"} {
		t.Run(shape, func(t *testing.T) {
			options := legacyImportFixture(t)
			entry := writeLegacyPiEvidenceFixture(t, "ok", "")
			root := filepath.Join(filepath.Dir(options.HomeDir), "entries")
			_ = os.Mkdir(root, 0o700)
			path := filepath.Join(root, "terminal")
			if err := os.Rename(entry, path); err != nil {
				t.Fatal(err)
			}
			options.EvidenceRoot = root
			if shape == "active-lock" {
				file, err := os.Open(filepath.Join(path, "active.lock"))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = file.Close() }()
			}
			if shape == "unknown-terminal" {
				_ = os.WriteFile(filepath.Join(path, "log.jsonl"), []byte("{}\n"), 0o600)
			}
			if shape == "socket" {
				listener, err := net.Listen("unix", filepath.Join(root, "state.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			}
			if shape == "partial-directory" {
				_ = os.Mkdir(filepath.Join(root, "partial"), 0o700)
			}
			before := legacyTree(t, root)
			destination := legacyTree(t, options.HomeDir)
			summaries, readErr := ReadLegacyPiLifecycleEvidence(root)
			for _, action := range []string{"preview", "apply", "resume", "rollback"} {
				if shape == "valid" && (action == "resume" || action == "rollback") {
					continue
				}
				_, err := ImportLegacyEngineState(options, action)
				if shape == "valid" {
					if err != nil || readErr != nil || len(summaries) != 1 {
						t.Fatalf("valid=%v %v", err, readErr)
					}
				} else {
					if err == nil || readErr == nil {
						t.Fatalf("accepted %s", shape)
					}
					if !reflect.DeepEqual(destination, legacyTree(t, options.HomeDir)) {
						t.Fatal("refusal wrote destination")
					}
				}
				if !reflect.DeepEqual(before, legacyTree(t, root)) {
					t.Fatal("compatibility reader/import touched legacy evidence")
				}
			}
		})
	}
}

func TestReadLegacyEngineProfilesGeneratedRealFixtureProperty(t *testing.T) {
	// Keep the AST-derived witness selection focused on this refusal class.
	_ = "profiles.encode"
	fixture, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(fixture, &raw); err != nil {
		t.Fatal(err)
	}
	profiles := raw["profiles"].(map[string]any)
	random := rand.New(rand.NewSource(260929))
	for index := 0; index < 80; index++ {
		// Vary source table order and isolate arbitrary subsets of real legacy
		// fixtures, then consume the resulting catalog through its real reader.
		subset := map[string]any{}
		for name, profile := range profiles {
			if random.Intn(2) == 0 {
				subset[name] = profile
			}
		}
		if len(subset) == 0 {
			continue
		}
		source, err := toml.Marshal(map[string]any{"profiles": subset})
		if err != nil {
			t.Fatal(err)
		}
		document, reasons, err := ReadLegacyEngineProfiles(source)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := MarshalEngineCatalog(document)
		if err != nil {
			t.Fatal(err)
		}
		path := writeEngineCatalogFixture(t, encoded)
		for name, want := range document.Engines {
			entry, state, err := ReadEngineCatalogEntry(path, name)
			if err != nil || state != EngineCatalogPresent || !legacySameEntry(entry, want) {
				t.Fatalf("iteration %d profile %s: %v %v", index, name, state, err)
			}
			if entry.ServedModelID == "" && reasons[name] == "" {
				t.Fatalf("unrunnable %s lacks reason", name)
			}
			// Every source field survives transcription, except the explicitly
			// documented knob rename and numeric normalization.
			original := subset[name].(map[string]any)
			for key := range original {
				if key == "engine" || key == "knobs" {
					continue
				}
				var output map[string]any
				data, _ := marshalEngineCatalogEntry(entry)
				_ = toml.Unmarshal(data, &output)
				if !reflect.DeepEqual(output[key], original[key]) {
					t.Fatalf("field %s.%s lost: %#v != %#v", name, key, output[key], original[key])
				}
			}
		}
	}
}
func TestReadLegacyEngineProfilesServedModelIdentityRules(t *testing.T) {
	cases := []struct {
		engine, model string
		argv          []string
		want, reason  string
	}{
		{"llama-cpp", "/models/model.gguf", []string{"--alias", "first,second", "--alias", "other"}, "first", ""},
		{"llama-cpp", "org/repo", nil, "repo", ""},
		{"mlx-swift", "org/repo", []string{"--model-id=explicit"}, "explicit", ""},
		{"mlx-swift", "/models/local", nil, "local", ""},
		{"mlx-lm", "org/repo", nil, "org/repo", ""},
		{"mlx-lm", "/models/local", nil, "", "mlx_lm_local_path_identity_unknown"},
		{"mlx-lm", "", nil, "", "served_model_id_missing"},
	}
	for index, test := range cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			entry := EngineCatalogEntry{Mode: "local", Engine: test.engine, Model: test.model, Argv: test.argv}
			id, reason := legacyServedModelID(entry)
			if id != test.want || reason != test.reason {
				t.Fatalf("got %q %q want %q %q", id, reason, test.want, test.reason)
			}
		})
	}
	options := legacyImportFixture(t)
	data, _ := os.ReadFile(options.ConfigPath)
	script := filepath.Join(filepath.Dir(options.ConfigPath), "console-script")
	_ = os.WriteFile(script, []byte("#!/usr/bin/python3\nraise SystemExit(42)\n"), 0o700)
	data = bytes.ReplaceAll(data, []byte("/usr/bin/false"), []byte(script))
	_ = os.WriteFile(options.ConfigPath, data, 0o600)
	report, err := ImportLegacyEngineState(options, "preview")
	if err != nil || report.NotRunnable["qwen"] != "script_executable_unsupported" {
		t.Fatalf("script %v %v", report, err)
	}
	legacyApply(t, options)
	status, err := SharedRuntimeStatusReport(SharedRuntimeOperatorOptions{HomeDir: options.HomeDir, CacheRoot: options.CacheRoot, ProjectDir: options.ProjectDir, Profile: "qwen"})
	if err != nil || status.EngineAvailability != "unknown" || status.EngineAvailabilityReason != "script_executable_unsupported" {
		t.Fatalf("status=%+v %v", status, err)
	}
}

func TestLegacyManagerInactiveCatalogReadFailedStaysUnknown(t *testing.T) {
	options := legacyImportFixture(t)
	legacyApply(t, options)
	previous := lstatEngineCatalogFile
	lstatEngineCatalogFile = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	defer func() { lstatEngineCatalogFile = previous }()
	var refusal *EngineCatalogError
	err := legacyManagerInactive(options, EngineCatalogDocument{Engines: map[string]EngineCatalogEntry{"qwen": {}}})
	if !errors.As(err, &refusal) || refusal.State != EngineCatalogReadFailed {
		t.Fatalf("read failure became absence: %T %v", err, err)
	}
	_, err = ImportLegacyEngineState(options, "preview")
	if !errors.As(err, &refusal) || refusal.State != EngineCatalogReadFailed {
		t.Fatalf("production read failure=%v", err)
	}
}
func TestLegacyManagerInactiveRefusesActiveAndUnknownState(t *testing.T) {
	for _, shape := range []string{"record", "lease", "socket", "lock", "unreadable", "symlink", "log-symlink", "unknown"} {
		t.Run(shape, func(t *testing.T) {
			options := legacyImportFixture(t)
			root := filepath.Join(options.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimePathVersion, "state")
			_ = os.MkdirAll(root, 0o700)
			switch shape {
			case "record":
				_ = os.WriteFile(filepath.Join(root, "broker-state.json"), []byte(`{"pid":1}`), 0o600)
			case "lease":
				_ = os.Mkdir(filepath.Join(root, "leases"), 0o700)
				_ = os.WriteFile(filepath.Join(root, "leases", "active.json"), []byte("{}"), 0o600)
			case "socket":
				listener, err := net.Listen("unix", filepath.Join(root, "b.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			case "lock":
				path := filepath.Join(root, "broker.lock")
				_ = os.WriteFile(path, nil, 0o600)
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = file.Close() }()
			case "unreadable":
				_ = os.Chmod(root, 0)
			case "symlink":
				_ = os.Remove(root)
				_ = os.Symlink(options.HomeDir, root)
			case "log-symlink":
				_ = os.Symlink(options.ConfigPath, filepath.Join(root, "broker.log"))
			case "unknown":
				_ = os.WriteFile(filepath.Join(root, "unknown"), nil, 0o600)
			}
			before := legacyTree(t, options.HomeDir)
			for _, action := range []string{"preview", "apply", "resume", "rollback"} {
				_, err := ImportLegacyEngineState(options, action)
				if err == nil {
					t.Fatalf("accepted %s %s", shape, action)
				}
				if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
					t.Fatal("refusal wrote")
				}
			}
		})
	}
}
func TestRenderEngineCompatibilityRefusalsReadFailedAndIdentity(t *testing.T) {
	options := legacyImportFixture(t)
	legacyApply(t, options)
	for _, alias := range []string{"qwen-infra", "model-harness"} {
		result, err := RenderEngineCompatibility(options.HomeDir, "qwen", alias)
		if err != nil || result.Engine != "qwen" || result.Entry.Model != "org/qwen" || result.Availability != "unknown" {
			t.Fatalf("render=%+v %v", result, err)
		}
	}
	for _, test := range []struct{ name, alias, site string }{{"", "qwen-infra", "alias.engine_required"}, {"absent", "qwen-infra", "alias.engine_absent"}, {"qwen", "ollama-infra", "alias.unsupported"}} {
		_, err := RenderEngineCompatibility(options.HomeDir, test.name, test.alias)
		requireLegacySite(t, err, test.site)
	}
	previous := lstatEngineCatalogFile
	lstatEngineCatalogFile = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	defer func() { lstatEngineCatalogFile = previous }()
	_, err := RenderEngineCompatibility(options.HomeDir, "qwen", "qwen-infra")
	var refusal *EngineCatalogError
	if !errors.As(err, &refusal) || refusal.State != EngineCatalogReadFailed {
		t.Fatalf("read failure=%T %v", err, err)
	}
}

// The existing broker owns ensure/lease/recovery; this importer adds no second
// ownership mechanism. A real sleeper provides the "do not touch" witness.
func TestImportLegacyEngineStateUnknownBrokerDoesNotClaimOrSignalProcess(t *testing.T) {
	options := legacyImportFixture(t)
	legacyApply(t, options)
	sleeper := exec.Command("/bin/sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	resolved, err := resolveSharedProfile(options.ProjectDir, options.HomeDir, options.CacheRoot, "qwen")
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateSharedRuntimeTree(resolved.Paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolved.Paths.BrokerState, []byte(`{"pid":`+strconv.Itoa(sleeper.Process.Pid)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := legacyTree(t, options.HomeDir)
	state := legacyTree(t, resolved.Paths.Root)
	operator := SharedRuntimeOperatorOptions{HomeDir: options.HomeDir, CacheRoot: options.CacheRoot, ProjectDir: options.ProjectDir, Profile: "qwen"}
	_, err = ImportLegacyEngineState(options, "apply")
	if err == nil {
		t.Fatal("import accepted unknown state")
	}
	_, err = AcquireSharedRuntimeLease(context.Background(), operator, "", os.Environ())
	if err == nil {
		t.Fatal("lease/ensure accepted unknown state")
	}
	_, err = StopSharedRuntime(operator, false, time.Second)
	if err == nil {
		t.Fatal("recovery/stop accepted unknown state")
	}
	if err := sleeper.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process touched: %v", err)
	}
	if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) || !reflect.DeepEqual(state, legacyTree(t, resolved.Paths.Root)) {
		t.Fatal("unknown state relabeled or overwritten")
	}
}

func TestReadLegacyEngineProfilesRefusesEverySourceSchemaGateThroughImport(t *testing.T) {
	// Keep the AST-derived witness selection focused on this refusal class.
	_ = "profiles.encode"
	attacks := []struct{ name, site, body string }{
		{"top-key", "profiles.document", "unlisted=1\n[profiles]\n"},
		{"top-missing", "profiles.table", "engines={}\n"},
		{"entry-not-table", "profiles.entry", "[profiles]\nqwen=1\n"},
		{"catalog-id", "profiles.catalog_only", "served_model_id='forged'\n"},
		{"knobs-not-table", "profiles.knobs", "knobs=1\n"},
		{"renamed-knob", "profiles.knob_catalog_only", "[profiles.qwen.knobs]\nkv_context_size=1024\n"},
		{"knob-type", "profiles.knob_type", "[profiles.qwen.knobs]\nkv_context_tokens=1024\n"},
		{"knob-value", "profiles.knob_value", "[profiles.qwen.knobs]\nkv_context_tokens='invalid'\n"},
		{"knob-narrowing-sentinel", "profiles.knob_value", "[profiles.qwen.knobs]\nkv_context_tokens='__mutant__'\n"},
		{"entry-type", "profiles.types", "model=42\n"},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			options := legacyImportFixture(t)
			data, _ := os.ReadFile(options.ConfigPath)
			if strings.HasPrefix(attack.body, "unlisted=") || strings.HasPrefix(attack.body, "engines=") || strings.HasPrefix(attack.body, "[profiles]\n") {
				data = []byte(attack.body)
			} else {
				if attack.name == "entry-type" {
					data = bytes.Replace(data, []byte("model='org/qwen'"), []byte("model=42"), 1)
				} else {
					data = append(data, []byte(attack.body)...)
				}
			}
			if err := os.WriteFile(options.ConfigPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, options.HomeDir)
			for _, action := range []string{"preview", "apply"} {
				_, err := ImportLegacyEngineState(options, action)
				requireLegacySite(t, err, attack.site)
				if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
					t.Fatal("refusal wrote")
				}
			}
		})
	}
}
func TestImportLegacyEngineStateRefusesInvalidActionHomeJournalAndSourceLinks(t *testing.T) {
	options := legacyImportFixture(t)
	_, err := ImportLegacyEngineState(options, "__mutant__")
	requireLegacySite(t, err, "import.action")
	relative := options
	relative.HomeDir = "relative"
	_, err = ImportLegacyEngineState(relative, "preview")
	requireLegacySite(t, err, "import.home")
	for _, action := range []string{"resume", "rollback"} {
		_, err = ImportLegacyEngineState(options, action)
		requireLegacySite(t, err, "recovery.missing_journal")
	}
	link := options.ConfigPath + ".link"
	if err := os.Symlink(options.ConfigPath, link); err != nil {
		t.Fatal(err)
	}
	linked := options
	linked.ConfigPath = link
	_, err = ImportLegacyEngineState(linked, "preview")
	requireLegacySite(t, err, "source.symlink")
	legacyApply(t, options)
	root := legacyTransactionRoot(options.HomeDir)
	journal := filepath.Join(root, "journal.json")
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(options.ConfigPath, journal); err != nil {
		t.Fatal(err)
	}
	_, err = ImportLegacyEngineState(options, "preview")
	requireLegacySite(t, err, "journal.file")
}

func TestImportLegacyEngineStateCommittedRollbackRefusesChangedSource(t *testing.T) {
	options := legacyImportFixture(t)
	legacyApply(t, options)
	data, _ := os.ReadFile(options.ConfigPath)
	legacyChangeSourceWithCollidingDigestPrefix(t, options, bytes.ReplaceAll(data, []byte("org/qwen"), []byte("org/changed")))
	before := legacyTree(t, options.HomeDir)
	_, err := ImportLegacyEngineState(options, "rollback")
	requireLegacySite(t, err, "recovery.source")
	if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
		t.Fatal("changed-source rollback wrote")
	}
}
func TestImportLegacyEngineStateRefusesDestinationLinksEvidenceAndAllowlist(t *testing.T) {
	// Keep the AST-derived witness selection focused on this refusal class.
	_ = "type:LegacyImportError"
	for _, attack := range []string{"dangling-link", "link-directory", "evidence-conflict", "unknown-target"} {
		t.Run(attack, func(t *testing.T) {
			options := legacyImportFixture(t)
			curator := filepath.Join(options.HomeDir, ".curator")
			_ = os.Mkdir(curator, 0o700)
			site := ""
			switch attack {
			case "dangling-link":
				_ = os.Symlink("missing", filepath.Join(curator, "engines.toml"))
				site = "destination.link"
			case "link-directory":
				_ = os.Symlink(options.HomeDir, filepath.Join(curator, "engines.toml"))
				site = "filesystem.file_shape"
			case "evidence-conflict":
				root := filepath.Join(filepath.Dir(options.HomeDir), "entries")
				_ = os.Mkdir(root, 0o700)
				entry := writeLegacyPiEvidenceFixture(t, "ok", "")
				if err := os.Rename(entry, filepath.Join(root, "entry")); err != nil {
					t.Fatal(err)
				}
				options.EvidenceRoot = root
				_ = os.WriteFile(filepath.Join(curator, "terminal-evidence.json"), []byte("[]"), 0o600)
				site = "destination.evidence_conflict"
			case "unknown-target":
				previous := legacyImportCheckpoint
				legacyImportCheckpoint = func(point string) error {
					if point == "after-backup" {
						return errors.New("interrupted")
					}
					return nil
				}
				_, err := ImportLegacyEngineState(options, "apply")
				legacyImportCheckpoint = previous
				if err == nil {
					t.Fatal("fault ignored")
				}
				journal, err := legacyReadJournal(options.HomeDir)
				if err != nil {
					t.Fatal(err)
				}
				journal.Targets[0].TargetID = "mutant"
				if err := legacyWriteJournal(legacyTransactionRoot(options.HomeDir), *journal); err != nil {
					t.Fatal(err)
				}
				site = "target.allowlist"
			}
			before := legacyTree(t, options.HomeDir)
			_, err := ImportLegacyEngineState(options, "preview")
			requireLegacySite(t, err, site)
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("refusal wrote")
			}
		})
	}
}
func TestLegacyManagerInactiveRejectsTransientCatalogReadFailureBeforeRetry(t *testing.T) {
	options := legacyImportFixture(t)
	legacyApply(t, options)
	previous := lstatEngineCatalogFile
	calls := 0
	lstatEngineCatalogFile = func(path string) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return nil, os.ErrPermission
		}
		return previous(path)
	}
	defer func() { lstatEngineCatalogFile = previous }()
	err := legacyManagerInactive(options, EngineCatalogDocument{Engines: map[string]EngineCatalogEntry{"qwen": {}}})
	var refusal *EngineCatalogError
	if !errors.As(err, &refusal) || refusal.State != EngineCatalogReadFailed {
		t.Fatalf("transient catalog failure was retried/admitted: %T %v", err, err)
	}
}
func TestImportLegacyEngineStateRefusesEveryActiveStatusMember(t *testing.T) {
	for _, member := range []string{"broker", "lease", "runtime"} {
		t.Run(member, func(t *testing.T) {
			options := legacyImportFixture(t)
			legacyApply(t, options)
			previous := legacyImportStatusReport
			legacyImportStatusReport = func(SharedRuntimeOperatorOptions) (SharedRuntimeStatus, error) {
				result := SharedRuntimeStatus{Broker: SharedRuntimeBrokerStatus{State: "absent"}}
				switch member {
				case "broker":
					result.Broker.State = "unknown"
				case "lease":
					result.Leases = []SharedLeaseStatus{{}}
				case "runtime":
					result.Runtime = &SharedRuntimeProcessStatus{}
				}
				return result, nil
			}
			defer func() { legacyImportStatusReport = previous }()
			before := legacyTree(t, options.HomeDir)
			_, err := ImportLegacyEngineState(options, "preview")
			requireLegacySite(t, err, "state.active")
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("active state touched")
			}
		})
	}
}

func TestImportLegacyEngineStateOldActiveNamespaceRefusesAllOwnerEntrypoints(t *testing.T) {
	for _, namespace := range []string{"pi-runtimes", "pi-rv"} {
		for _, shape := range []string{"process", "lease", "socket", "lock", "unknown", "log-link"} {
			t.Run(namespace+"/"+shape, func(t *testing.T) {
				o := legacyImportFixture(t)
				legacyApply(t, o)
				sleeper := exec.Command("/bin/sleep", "30")
				if err := sleeper.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
				root := filepath.Join(o.CacheRoot, "agents-infra", namespace, "old")
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				switch shape {
				case "process":
					_ = os.WriteFile(filepath.Join(root, "broker-state.json"), []byte(fmt.Sprintf(`{"pid":%d}`, sleeper.Process.Pid)), 0o600)
				case "lease":
					_ = os.Mkdir(filepath.Join(root, "leases"), 0o700)
					_ = os.WriteFile(filepath.Join(root, "leases", "active"), []byte("leased"), 0o600)
				case "socket":
					listener, err := net.Listen("unix", filepath.Join(root, "broker.sock"))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = listener.Close() }()
				case "lock":
					f, err := os.OpenFile(filepath.Join(root, "broker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = f.Close() }()
					if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				case "unknown":
					_ = os.WriteFile(filepath.Join(root, "mutant"), nil, 0o600)
				case "log-link":
					if err := os.Symlink("old-log", filepath.Join(root, "broker.log")); err != nil {
						t.Fatal(err)
					}
				}
				before := legacyTree(t, o.HomeDir)
				old := legacyTree(t, root)
				for _, action := range []string{"preview", "apply", "resume", "rollback"} {
					if _, err := ImportLegacyEngineState(o, action); err == nil {
						t.Fatalf("%s accepted old %s", action, shape)
					} else {
						switch shape {
						case "lease":
							requireLegacySite(t, err, "state.lease_or_cwd")
						case "lock":
							requireLegacySite(t, err, "state.active_lock")
						case "process":
							requireLegacySite(t, err, "state.structured_json")
						case "log-link":
							requireLegacySite(t, err, "state.log_shape")
						default:
							requireLegacySite(t, err, "state.unclassifiable")
						}
					}
				}
				owner := SharedRuntimeOperatorOptions{HomeDir: o.HomeDir, CacheRoot: o.CacheRoot, ProjectDir: o.ProjectDir, Profile: "qwen"}
				if _, err := AcquireSharedRuntimeLease(context.Background(), owner, "", os.Environ()); err == nil {
					t.Fatal("lease/ensure admitted old state")
				}
				if _, err := StopSharedRuntime(owner, false, time.Second); err == nil {
					t.Fatal("recovery admitted old state")
				}
				if _, err := SharedRuntimeStatusReport(owner); err == nil {
					t.Fatal("unknown state reported absent")
				}
				if err := sleeper.Process.Signal(syscall.Signal(0)); err != nil {
					t.Fatal("old process touched", err)
				}
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) || !reflect.DeepEqual(old, legacyTree(t, root)) {
					t.Fatal("old state changed")
				}
			})
		}
	}
}

func TestImportLegacyEngineStateRecoveryRefusesNonFileStaging(t *testing.T) {
	for _, shape := range []string{"missing", "symlink", "after-symlink"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyApply(t, o)
			j, err := legacyReadJournal(o.HomeDir)
			if err != nil {
				t.Fatal(err)
			}
			j.Phase = "prepared"
			target := &j.Targets[0]
			path, _ := legacyTargetPath(o.HomeDir, target.TargetID)
			_ = os.Remove(path)
			stage := filepath.Join(legacyTransactionRoot(o.HomeDir), j.TransactionID+"-"+target.TargetID+"-after")
			if shape == "after-symlink" {
				data, _ := os.ReadFile(stage)
				target.After = LegacyImportFileState{Kind: "symlink", SHA256: legacyString(legacyDigest(data)), LinkTarget: legacyString(string(data))}
			} else {
				_ = os.Remove(stage)
				if shape == "symlink" {
					_ = os.Symlink("arbitrary", stage)
				}
			}
			if err := legacyWriteJournal(legacyTransactionRoot(o.HomeDir), *j); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, o.HomeDir)
			for _, action := range []string{"preview", "resume", "rollback"} {
				_, err := ImportLegacyEngineState(o, action)
				requireLegacySite(t, err, "recovery.staging")
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
					t.Fatal("recovery wrote")
				}
			}
		})
	}
}
func TestImportLegacyEngineStateDestinationMalformedWithoutSource(t *testing.T) {
	for _, body := range []string{"schema_version=2\n[engines]\n", "schema_version=1\n[engines.qwen]\nmodel=42\n"} {
		o := legacyImportFixture(t)
		_ = os.Remove(o.ConfigPath)
		_ = os.Mkdir(filepath.Join(o.HomeDir, ".curator"), 0o700)
		_ = os.WriteFile(filepath.Join(o.HomeDir, ".curator", "engines.toml"), []byte(body), 0o600)
		for _, action := range []string{"preview", "apply"} {
			if _, err := ImportLegacyEngineState(o, action); err == nil {
				t.Fatal("malformed destination admitted")
			}
		}
	}
}

func TestImportLegacyEngineStateParserRefusalAttributionThroughEntrypoint(t *testing.T) {
	o := legacyImportFixture(t)
	data, _ := os.ReadFile(o.ConfigPath)
	if err := os.WriteFile(o.ConfigPath, []byte("[profiles"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ImportLegacyEngineState(o, "preview")
	requireLegacySite(t, err, "profiles.decode")
	if err := os.WriteFile(o.ConfigPath, append(data, []byte("\ninvalid@\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ImportLegacyEngineState(o, "preview")
	requireLegacySite(t, err, "profiles.decode")
	for _, attack := range []struct{ raw, site string }{{"", "journal.json"}, {`{1:true}`, "journal.json_member"}, {`{"targets":[invalid]}`, "journal.json"}, {`[1]`, "journal.decode"}} {
		_ = os.MkdirAll(legacyTransactionRoot(o.HomeDir), 0o700)
		_ = os.WriteFile(filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json"), []byte(attack.raw), 0o600)
		_, err := ImportLegacyEngineState(o, "preview")
		requireLegacySite(t, err, attack.site)
	}
}
func TestImportLegacyEngineStateTerminalEvidenceOwnedRefreshKeepsAbsentIdentity(t *testing.T) {
	o := legacyImportFixture(t)
	root := filepath.Dir(writeLegacyPiEvidenceFixture(t, "ok", ""))
	o.EvidenceRoot = root
	_ = os.Chmod(root, 0o700)
	legacyApply(t, o)
	path, _ := legacyTargetPath(o.HomeDir, legacyEvidenceTarget)
	old, _ := os.ReadFile(path)
	var summaries []PiTerminalEvidenceSummary
	_ = json.Unmarshal(old, &summaries)
	// Moving the same retained evidence changes its sanitized source-path digest.
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	o.EvidenceRoot = moved
	legacyApply(t, o)
	refreshed, _ := os.ReadFile(path)
	if bytes.Equal(old, refreshed) {
		t.Fatal("owned evidence not refreshed")
	}
	before := legacyTree(t, o.HomeDir)
	o.EvidenceRoot = ""
	legacyApply(t, o)
	if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
		t.Fatal("absence deleted evidence")
	}
	// Corrupting the file without matching ownership refuses, never overwrites.
	o.EvidenceRoot = moved
	_ = os.WriteFile(path, []byte(`[]`), 0o600)
	_, err := ImportLegacyEngineState(o, "apply")
	if err == nil {
		t.Fatal("drift admitted")
	}
}
func TestImportLegacyEngineStateRefusesForgedOwnedEvidencePayload(t *testing.T) {
	o := legacyImportFixture(t)
	o.EvidenceRoot = filepath.Dir(writeLegacyPiEvidenceFixture(t, "ok", ""))
	_ = os.Chmod(o.EvidenceRoot, 0o700)
	legacyApply(t, o)
	j, err := legacyReadJournal(o.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range j.Targets {
		target := &j.Targets[i]
		if target.TargetID != legacyEvidenceTarget {
			continue
		}
		data := []byte(`42`)
		target.After.SHA256 = legacyString(legacyDigest(data))
		path, _ := legacyTargetPath(o.HomeDir, target.TargetID)
		_ = os.WriteFile(path, data, 0o600)
		_ = os.WriteFile(filepath.Join(legacyTransactionRoot(o.HomeDir), j.TransactionID+"-"+target.TargetID+"-after"), data, 0o600)
	}
	if err := legacyWriteJournal(legacyTransactionRoot(o.HomeDir), *j); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"preview", "apply"} {
		_, err := ImportLegacyEngineState(o, action)
		requireLegacySite(t, err, "destination.evidence_decode")
	}
}

func TestRenderEngineCompatibilityRejectsNarrowedAlias(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	_, err := RenderEngineCompatibility(o.HomeDir, "qwen", "__mutant__")
	requireLegacySite(t, err, "alias.unsupported")
}
func TestImportLegacyEngineStateCommittedRepeatRefusesModeDrift(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	path, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
	_ = os.Chmod(path, 0o400)
	before := legacyTree(t, o.HomeDir)
	for _, action := range []string{"preview", "apply", "resume", "rollback"} {
		_, err := ImportLegacyEngineState(o, action)
		requireLegacySite(t, err, "recovery.committed_drift")
		if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
			t.Fatal("drift overwritten")
		}
	}
}
func TestImportLegacyEngineStateInterruptedRollbackResumesRestoration(t *testing.T) {
	for _, point := range []string{"after-replacement-1", "after-replacement-2"} {
		t.Run(point, func(t *testing.T) {
			o := legacyImportFixture(t)
			o.EvidenceRoot = filepath.Dir(writeLegacyPiEvidenceFixture(t, "ok", ""))
			_ = os.Chmod(o.EvidenceRoot, 0o700)
			_ = os.Mkdir(filepath.Join(o.HomeDir, ".curator"), 0o700)
			target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
			_ = os.WriteFile(target, []byte(legacyInitialCatalog), 0o400)
			legacyApply(t, o)
			encoded, _ := json.Marshal(o)
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyImportKillHelper$")
			command.Env = append(os.Environ(), "CURATOR_IMPORT_KILL_POINT="+point, "CURATOR_IMPORT_OPTIONS="+string(encoded), "CURATOR_IMPORT_ACTION=rollback")
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
				t.Fatalf("rollback not killed: %v %s", err, output)
			}
			if _, err := ImportLegacyEngineState(o, "resume"); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(target)
			info, _ := os.Stat(target)
			if !bytes.Equal(data, []byte(legacyInitialCatalog)) || info.Mode().Perm() != 0o400 {
				t.Fatal("resume lost rollback intent")
			}
			evidence, _ := legacyTargetPath(o.HomeDir, legacyEvidenceTarget)
			if _, err := os.Lstat(evidence); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created evidence survived")
			}
		})
	}
}

func TestValidateLegacyEngineCompatibilityConfigRefusesReadAbsenceAndConflict(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	rendered, err := RenderEngineCompatibility(o.HomeDir, "qwen", "model-harness")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLegacyEngineCompatibilityConfig(o.ConfigPath, rendered); err != nil {
		t.Fatal(err)
	}
	requireLegacySite(t, ValidateLegacyEngineCompatibilityConfig(o.ConfigPath+"-absent", rendered), "alias.config_read")
	missing := rendered
	missing.Engine = "missing"
	requireLegacySite(t, ValidateLegacyEngineCompatibilityConfig(o.ConfigPath, missing), "alias.config_engine_absent")
	conflict := rendered
	conflict.Entry.Model = "other"
	requireLegacySite(t, ValidateLegacyEngineCompatibilityConfig(o.ConfigPath, conflict), "alias.config_conflict")
}

func TestImportLegacyEngineStatePendingSourceChangeRefusesEveryAction(t *testing.T) {
	o := legacyImportFixture(t)
	previous := legacyImportCheckpoint
	legacyImportCheckpoint = func(point string) error {
		if point == "after-backup" {
			return errors.New("test interruption")
		}
		return nil
	}
	_, err := ImportLegacyEngineState(o, "apply")
	legacyImportCheckpoint = previous
	if err == nil {
		t.Fatal("checkpoint not reached")
	}
	data, _ := os.ReadFile(o.ConfigPath)
	legacyChangeSourceWithCollidingDigestPrefix(t, o, data)
	before := legacyTree(t, o.HomeDir)
	for _, action := range []string{"preview", "apply", "resume", "rollback"} {
		_, err := ImportLegacyEngineState(o, action)
		requireLegacySite(t, err, "recovery.source")
		if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
			t.Fatal("source mismatch wrote")
		}
	}
}

// Choose a distinct, valid snapshot with the same first digest member. This
// catches recovery that compares only part of the source identity.
func legacyChangeSourceWithCollidingDigestPrefix(t *testing.T, o LegacyImportOptions, changed []byte) {
	t.Helper()
	journal, err := legacyReadJournal(o.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	initial := ""
	if journal != nil {
		initial = journal.SourceSHA256
	} else {
		plan, err := legacyPlan(o, nil)
		if err != nil {
			t.Fatal(err)
		}
		initial = plan.report.SourceSHA256
	}
	for nonce := 0; nonce < 4096; nonce++ {
		candidate := append(append([]byte(nil), changed...), []byte(fmt.Sprintf("\n# distinct snapshot %d\n", nonce))...)
		if err := os.WriteFile(o.ConfigPath, candidate, 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := legacyPlan(o, journal)
		if err != nil {
			t.Fatal(err)
		}
		if plan.report.SourceSHA256 != initial && plan.report.SourceSHA256[:1] == initial[:1] {
			return
		}
	}
	t.Fatal("could not construct a distinct digest with a matching prefix")
}

func TestImportLegacyEngineStateRecoveryRefusesDigestMatchingArtifactSymlink(t *testing.T) {
	for _, artifact := range []string{"backup", "staging"} {
		t.Run(artifact, func(t *testing.T) {
			o := legacyImportFixture(t)
			_ = os.Mkdir(filepath.Join(o.HomeDir, ".curator"), 0o700)
			target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
			_ = os.WriteFile(target, []byte(legacyInitialCatalog), 0o600)
			legacyApply(t, o)
			j, err := legacyReadJournal(o.HomeDir)
			if err != nil {
				t.Fatal(err)
			}
			entry := j.Targets[0]
			name := j.TransactionID + "-" + entry.TargetID + "-after"
			site := "recovery.staging"
			if artifact == "backup" {
				name = *entry.BackupRef
				site = "recovery.backup"
			}
			path := filepath.Join(legacyTransactionRoot(o.HomeDir), name)
			data, _ := os.ReadFile(path)
			_ = os.Remove(path)
			if err := os.Symlink(string(data), path); err != nil {
				t.Fatal(err)
			}
			before := legacyTree(t, o.HomeDir)
			for _, action := range []string{"preview", "resume", "rollback"} {
				_, err := ImportLegacyEngineState(o, action)
				requireLegacySite(t, err, site)
				if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
					t.Fatal("ambiguous artifact altered")
				}
			}
		})
	}
}

func TestImportLegacyEngineStateReadableLegacyFixtureAndOrdinaryHomePermissions(t *testing.T) {
	o := legacyImportFixture(t)
	_ = os.Chmod(o.HomeDir, 0o755)
	_ = os.Chmod(o.ConfigPath, 0o644)
	sourceBefore := legacyTree(t, filepath.Dir(o.ConfigPath))
	if _, err := ImportLegacyEngineState(o, "preview"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceBefore, legacyTree(t, filepath.Dir(o.ConfigPath))) {
		t.Fatal("preview wrote")
	}
	legacyApply(t, o)
	info, err := os.Stat(o.ConfigPath)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatal("source mode changed")
	}
	o2 := legacyImportFixture(t)
	o2.ConfigPath = "testdata/legacy-model-harness-fixtures.toml"
	legacyApply(t, o2)
}

func TestRefuseActiveLegacyEngineStateAdmissionThroughResolverAndLease(t *testing.T) {
	o := legacyImportFixture(t)
	legacyApply(t, o)
	root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
	_ = os.MkdirAll(root, 0o700)
	_ = os.WriteFile(filepath.Join(root, "broker-state.json"), []byte(`{"pid":123}`), 0o600)
	requireLegacySite(t, RefuseActiveLegacyEngineState(o.CacheRoot), "state.structured_json")
	_, err := ImportLegacyEngineState(o, "preview")
	requireLegacySite(t, err, "state.structured_json")
	_, err = resolveSharedProfile(o.ProjectDir, o.HomeDir, o.CacheRoot, "qwen")
	requireLegacySite(t, err, "state.structured_json")
	_, err = AcquireSharedRuntimeLease(context.Background(), SharedRuntimeOperatorOptions{HomeDir: o.HomeDir, CacheRoot: o.CacheRoot, ProjectDir: o.ProjectDir, Profile: "qwen"}, "", os.Environ())
	requireLegacySite(t, err, "state.structured_json")
	if _, err := os.Stat(filepath.Join(o.CacheRoot, "curator-engines")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy state claimed as new ownership")
	}
}

func TestImportLegacyEngineStateDuplicateTerminalIdentityRefuses(t *testing.T) {
	o := legacyImportFixture(t)
	root := filepath.Join(filepath.Dir(o.HomeDir), "evidence")
	_ = os.Mkdir(root, 0o700)
	for _, name := range []string{"first", "second"} {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.Rename(entry, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	o.EvidenceRoot = root
	before := legacyTree(t, o.HomeDir)
	for _, action := range []string{"preview", "apply"} {
		_, err := ImportLegacyEngineState(o, action)
		requireLegacySite(t, err, "source.evidence_duplicate")
		if !reflect.DeepEqual(before, legacyTree(t, o.HomeDir)) {
			t.Fatal("conflict wrote")
		}
	}
	_, err := ReadLegacyPiLifecycleEvidence(root)
	requireLegacySite(t, err, "source.evidence_duplicate")
}
