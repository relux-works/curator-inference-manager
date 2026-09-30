//go:build !windows

package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSharedRuntimeListenerBusyHasNominalBrokerExitCode(t *testing.T) {
	code, ok := SharedRuntimeExitCode(sharedRuntimeError("runtime_listener_occupied", errors.New("busy")))
	if !ok || code != sharedRuntimeExitListenerBusy {
		t.Fatalf("listener-busy exit=(%d,%t) want=(%d,true)", code, ok, sharedRuntimeExitListenerBusy)
	}
}

func TestSharedRuntimeQuarantineHasNominalBrokerExitCode(t *testing.T) {
	code, ok := SharedRuntimeExitCode(sharedRuntimeError("shared_runtime_quarantined", errors.New("quarantined")))
	if !ok || code != sharedRuntimeExitQuarantined {
		t.Fatalf("quarantine exit=(%d,%t) want=(%d,true)", code, ok, sharedRuntimeExitQuarantined)
	}
}

// TestT260929 locks the typed refusal payload used by the launcher mismatch gate.
func TestT260929(t *testing.T) {
	err := sharedRuntimeMismatch("runtime_authorization_mismatch", "exec_plan_digest")
	var refusal *SharedRuntimeError
	if !errors.As(err, &refusal) {
		t.Fatalf("mismatch was not a typed refusal: %T %v", err, err)
	}
	if refusal.Code != "runtime_authorization_mismatch" || refusal.MismatchField != "exec_plan_digest" || refusal.Err == nil || refusal.Error() == "" {
		t.Fatalf("mismatch refusal payload=%#v", refusal)
	}
}

func TestSharedRuntimeDigestsExcludeSharingAndCoverTheExecutionPlan(t *testing.T) {
	profile := mustParsedPiProfile(t, false)
	profile.Runtime.Sharing = &PiRuntimeSharing{Mode: "shared", LingerSeconds: 0, MaxLeases: 1, MaxSegmentBytes: 1024, MaxSegments: 2, HeartbeatIntervalSeconds: 1, LeaseStaleSeconds: 2, BrokerStartTimeoutSeconds: 40}
	key, digest := SharedRuntimeKey(profile)

	peer := profile
	peer.Runtime.Sharing = &PiRuntimeSharing{Mode: "shared", LingerSeconds: 300, MaxLeases: 16, MaxSegmentBytes: 2048, MaxSegments: 4, HeartbeatIntervalSeconds: 15, LeaseStaleSeconds: 60, BrokerStartTimeoutSeconds: 240}
	peer.Source = "/another/project/config.toml"
	peerKey, peerDigest := SharedRuntimeKey(peer)
	if key != peerKey || digest != peerDigest {
		t.Fatal("sharing policy or project provenance changed runtime identity")
	}

	mutated := peer
	mutated.Runtime.Argv = append([]string(nil), peer.Runtime.Argv...)
	mutated.Runtime.Argv[0] = "different"
	mutatedKey, mutatedDigest := SharedRuntimeKey(mutated)
	if mutatedKey == key || mutatedDigest == digest {
		t.Fatal("runtime argv mutation did not change profile and runtime digests")
	}
	if SharedRuntimeExecPlanDigest(profile, "/runtime/cwd") == SharedRuntimeExecPlanDigest(profile, "/other/cwd") {
		t.Fatal("runtime cwd is absent from exec plan digest")
	}
	absentProfileDigest := SharedRuntimeProfileDigest(profile)
	absentExecDigest := SharedRuntimeExecPlanDigest(profile, "/runtime/cwd")
	cacheBudget := int64(6_442_450_944)
	profile.CacheBudgetBytes = &cacheBudget
	lowerProfileDigest := SharedRuntimeProfileDigest(profile)
	lowerExecDigest := SharedRuntimeExecPlanDigest(profile, "/runtime/cwd")
	if lowerProfileDigest == absentProfileDigest || lowerExecDigest == absentExecDigest {
		t.Fatal("cache budget presence is absent from a shared-runtime digest")
	}
	higher := int64(12_884_901_888)
	profile.CacheBudgetBytes = &higher
	if SharedRuntimeProfileDigest(profile) == lowerProfileDigest || SharedRuntimeExecPlanDigest(profile, "/runtime/cwd") == lowerExecDigest {
		t.Fatal("cache budget value is absent from a shared-runtime digest")
	}
}

func TestResolveSharedRuntimePathsAreHashOnlyContainedAndBounded(t *testing.T) {
	cache, err := os.MkdirTemp("/tmp", "agents-infra-shared-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	key := strings.Repeat("a", 64)
	paths, err := ResolveSharedRuntimePaths(cache, key)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(paths.Root) != key || filepath.Base(paths.RendezvousDir) != key[:32] || filepath.Base(paths.RendezvousSocket) != "b.sock" {
		t.Fatalf("unexpected shared paths: %#v", paths)
	}
	if err := CreateSharedRuntimeTree(paths); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Root, paths.LeasesDir, paths.RuntimeCWD, paths.RendezvousDir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("managed directory %s: info=%v err=%v", path, info, err)
		}
	}
	if _, err := ResolveSharedRuntimePaths(cache, "profile-name"); err == nil {
		t.Fatal("non-hash runtime key admitted")
	} else if sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
		t.Fatalf("non-hash runtime key refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
	longBase, err := os.MkdirTemp("/tmp", "agents-infra-long-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(longBase) })
	longCache := filepath.Join(longBase, strings.Repeat("x", 110))
	if err := os.Mkdir(longCache, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveSharedRuntimePaths(longCache, key)
	var sharedErr *SharedRuntimeError
	if !errors.As(err, &sharedErr) || sharedErr.Code != "broker_rendezvous_path_too_long" {
		t.Fatalf("long rendezvous refusal=%v", err)
	}
}

func TestResolveSharedRuntimePathsRejectsNonHashRuntimeKey(t *testing.T) {
	if _, err := ResolveSharedRuntimePaths(t.TempDir(), "profile-name"); err == nil {
		t.Fatal("ResolveSharedRuntimePaths admitted a non-hash runtime key")
	} else if sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
		t.Fatalf("ResolveSharedRuntimePaths refusal code=%q err=%v", sharedRuntimeErrorCode(err), err)
	}
}

func TestResolveSharedRuntimePathsRefusesCacheRootFailures(t *testing.T) {
	key := strings.Repeat("a", 64)
	t.Run("user-cache-dir", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CACHE_HOME", "")
		if _, err := ResolveSharedRuntimePaths("", key); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("undetermined user cache root refusal=%v", err)
		}
	})
	t.Run("abs-from-removed-working-directory", func(t *testing.T) {
		original, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		removed := t.TempDir()
		if err := os.Chdir(removed); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(removed); err != nil {
			_ = os.Chdir(original)
			t.Fatal(err)
		}
		_, resolveErr := ResolveSharedRuntimePaths("relative-cache", key)
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
		if sharedRuntimeErrorCode(resolveErr) != "shared_runtime_state_path_invalid" {
			t.Fatalf("abs failure refusal=%v", resolveErr)
		}
	})
	t.Run("symlink-resolution", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing-cache-root")
		if _, err := ResolveSharedRuntimePaths(missing, key); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("missing cache root refusal=%v", err)
		}
	})
	t.Run("stat-failure", func(t *testing.T) {
		previous := sharedRuntimePathStat
		sharedRuntimePathStat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { sharedRuntimePathStat = previous })
		if _, err := ResolveSharedRuntimePaths(t.TempDir(), key); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("cache stat failure refusal=%v", err)
		}
	})
	t.Run("non-directory-cache-root", func(t *testing.T) {
		cacheFile := filepath.Join(t.TempDir(), "cache-file")
		if err := os.WriteFile(cacheFile, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveSharedRuntimePaths(cacheFile, key); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("non-directory cache root refusal=%v", err)
		}
	})
	t.Run("rendezvous-path-limit", func(t *testing.T) {
		base, err := os.MkdirTemp("/tmp", "curator-rendezvous-bound-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(base) })
		longCache := filepath.Join(base, strings.Repeat("x", 110))
		if err := os.Mkdir(longCache, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveSharedRuntimePaths(longCache, key); sharedRuntimeErrorCode(err) != "broker_rendezvous_path_too_long" {
			t.Fatalf("overlong rendezvous path refusal=%v", err)
		}
	})
}

func TestReadSharedBrokerRecordRefusesReadAndDecodeFailures(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "read error", setup: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "wrong typed record", setup: func(path string) error { return os.WriteFile(path, []byte(`{"stage":17}`), 0o600) }},
		{name: "trailing record", setup: func(path string) error { return os.WriteFile(path, []byte(`{} {}`), 0o600) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broker-state.json")
			if err := testCase.setup(path); err != nil {
				t.Fatal(err)
			}
			record, present, err := readSharedBrokerRecord(path)
			if record != nil || present || sharedRuntimeErrorCode(err) != "shared_runtime_state_unreadable" {
				t.Fatalf("unsafe broker state record=%#v present=%t err=%v", record, present, err)
			}
		})
	}
}

func TestWriteSharedJSONAtomicRefusesMalformedAndFilesystemFailures(t *testing.T) {
	t.Run("marshal error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broker-state.json")
		if err := writeSharedJSONAtomic(path, make(chan int)); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("unmarshalable state refusal=%v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marshal failure wrote state: %v", err)
		}
	})
	t.Run("directory open error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing", "broker-state.json")
		if err := writeSharedJSONAtomic(path, SharedBrokerRecord{Stage: "test"}); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("missing state directory refusal=%v", err)
		}
	})
	t.Run("temporary file open error", func(t *testing.T) {
		previous := sharedRuntimeJSONAtomicOpenat
		sharedRuntimeJSONAtomicOpenat = func(int, string, int, uint32) (int, error) { return -1, os.ErrPermission }
		t.Cleanup(func() { sharedRuntimeJSONAtomicOpenat = previous })
		directory := t.TempDir()
		err := writeSharedJSONAtomic(filepath.Join(directory, "broker-state.json"), SharedBrokerRecord{Stage: "test"})
		if sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("temporary file open refusal=%v", err)
		}
		entries, readErr := os.ReadDir(directory)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("failed Openat left filesystem entries=%v err=%v", entries, readErr)
		}
	})
	t.Run("write error", func(t *testing.T) {
		previous := sharedRuntimeJSONAtomicWrite
		sharedRuntimeJSONAtomicWrite = func(*os.File, []byte) (int, error) { return 0, os.ErrPermission }
		t.Cleanup(func() { sharedRuntimeJSONAtomicWrite = previous })
		path := filepath.Join(t.TempDir(), "broker-state.json")
		err := writeSharedJSONAtomic(path, SharedBrokerRecord{Stage: "test"})
		if sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("short/failed write refusal=%v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed write installed broker state: %v", err)
		}
	})
	t.Run("rename error", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "broker-state.json")
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeSharedJSONAtomic(destination, SharedBrokerRecord{Stage: "test"}); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("rename refusal=%v", err)
		}
	})
	t.Run("directory sync error", func(t *testing.T) {
		previous := sharedRuntimeJSONAtomicDirSync
		sharedRuntimeJSONAtomicDirSync = func(int) error { return os.ErrPermission }
		t.Cleanup(func() { sharedRuntimeJSONAtomicDirSync = previous })
		path := filepath.Join(t.TempDir(), "broker-state.json")
		if err := writeSharedJSONAtomic(path, SharedBrokerRecord{Stage: "test"}); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("directory sync refusal=%v", err)
		}
	})
}

func TestRemoveSharedLeaseMirrorsRefusesEveryUnsafeShape(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := removeSharedLeaseMirrors(path); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("read error refusal=%v", err)
		}
	})
	t.Run("directory_entry", func(t *testing.T) {
		directory := t.TempDir()
		entry := filepath.Join(directory, "lease.json")
		if err := os.Mkdir(entry, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := removeSharedLeaseMirrors(directory); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("directory mirror refusal=%v", err)
		}
		if _, err := os.Lstat(entry); err != nil {
			t.Fatalf("refusal removed the unexpected directory entry: %v", err)
		}
	})
	t.Run("symlink_entry", func(t *testing.T) {
		directory := t.TempDir()
		foreign := filepath.Join(t.TempDir(), "foreign-lease")
		if err := os.WriteFile(foreign, []byte("leave untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(directory, "lease.json")
		if err := os.Symlink(foreign, entry); err != nil {
			t.Fatal(err)
		}
		if err := removeSharedLeaseMirrors(directory); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("symlink mirror refusal=%v", err)
		}
		if got, err := os.ReadFile(foreign); err != nil || string(got) != "leave untouched" {
			t.Fatalf("refusal changed foreign target: data=%q err=%v", got, err)
		}
	})
	t.Run("non_json_entry", func(t *testing.T) {
		directory := t.TempDir()
		entry := filepath.Join(directory, "lease.txt")
		if err := os.WriteFile(entry, []byte("lease"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := removeSharedLeaseMirrors(directory); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("non-JSON mirror refusal=%v", err)
		}
		if got, err := os.ReadFile(entry); err != nil || string(got) != "lease" {
			t.Fatalf("refusal removed non-JSON entry: data=%q err=%v", got, err)
		}
	})
	t.Run("remove error", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "lease.json"), []byte("lease"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o500); err != nil {
			t.Fatal(err)
		}
		err := removeSharedLeaseMirrors(directory)
		if chmodErr := os.Chmod(directory, 0o700); chmodErr != nil {
			t.Fatalf("restore lease directory permissions: %v", chmodErr)
		}
		if sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
			t.Fatalf("lease mirror remove refusal=%v", err)
		}
	})
}

func TestCreateSharedRuntimeTreeRefusesEveryUnsafePath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, SharedRuntimePaths)
	}{
		{
			name: "missing-cache-root",
			setup: func(t *testing.T, paths SharedRuntimePaths) {
				t.Helper()
				if err := os.Remove(paths.CanonicalCacheRoot); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "leases-component-is-not-directory",
			setup: func(t *testing.T, paths SharedRuntimePaths) {
				t.Helper()
				if err := os.MkdirAll(paths.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.LeasesDir, []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cwd-component-is-not-directory",
			setup: func(t *testing.T, paths SharedRuntimePaths) {
				t.Helper()
				if err := os.MkdirAll(paths.LeasesDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.RuntimeCWD, []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rendezvous-component-is-not-directory",
			setup: func(t *testing.T, paths SharedRuntimePaths) {
				t.Helper()
				if err := os.MkdirAll(paths.LeasesDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(paths.RuntimeCWD, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(paths.RendezvousDir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.RendezvousDir, []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache, err := os.MkdirTemp("/tmp", "ct-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(cache); err != nil {
					t.Errorf("remove cache fixture: %v", err)
				}
			})
			paths, err := ResolveSharedRuntimePaths(cache, strings.Repeat("d", 64))
			if err != nil {
				t.Fatal(err)
			}
			test.setup(t, paths)
			if err := CreateSharedRuntimeTree(paths); sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
				t.Fatalf("CreateSharedRuntimeTree error=%v, want shared_runtime_state_path_invalid", err)
			}
		})
	}
}

func TestCuratorEnginesSharedRuntimeRootIsDisjointFromLegacyAgentsInfra(t *testing.T) {
	cache, err := os.MkdirTemp("/tmp", "curator-state-root-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	key := strings.Repeat("b", 64)
	paths, err := ResolveSharedRuntimePaths(cache, key)
	if err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(paths.CanonicalCacheRoot, "agents-infra", "pi-runtimes", key)
	legacyRendezvous := filepath.Join(paths.CanonicalCacheRoot, "agents-infra", "pi-rv", key[:32])
	if isPathWithinRoot(paths.Root, legacyRoot) {
		t.Fatalf("curator-engines would own legacy broker state root %s", legacyRoot)
	}
	if isPathWithinRoot(paths.RendezvousDir, legacyRendezvous) {
		t.Fatalf("curator-engines would share the legacy broker rendezvous root %s", legacyRendezvous)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(paths.Root))) != "curator-engines" {
		t.Fatalf("new broker root is not under its own namespace: %s", paths.Root)
	}
}

func isPathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func validSharedAuthorizationFrame() sharedRuntimeAuthorizationFrame {
	return sharedRuntimeAuthorizationFrame{
		Schema:          sharedRuntimeAuthSchema,
		ProtocolVersion: SharedRuntimeProtocolVersion,
		RuntimeKey:      strings.Repeat("b", 64),
		LauncherPID:     123,
		ExecPlanDigest:  strings.Repeat("c", 64),
	}
}

func rawSharedAuthorizationFrame(t *testing.T, frame sharedRuntimeAuthorizationFrame) []byte {
	t.Helper()
	var body bytes.Buffer
	if err := writeAuthorizationFrame(&body, frame); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(body.Bytes())
}

func TestSharedAuthorizationDecoderClosesTheDecodedMemberMultiset(t *testing.T) {
	frame := validSharedAuthorizationFrame()
	valid := rawSharedAuthorizationFrame(t, frame)
	escaped := bytes.ReplaceAll(valid, []byte(`"schema"`), []byte(`"\u0073chema"`))
	tests := []struct {
		name   string
		raw    []byte
		ok     bool
		reason string
		field  string
	}{
		{name: "valid", raw: valid, ok: true},
		{name: "escaped valid member", raw: escaped, ok: true},
		{name: "unknown", raw: bytes.Replace(valid, []byte(`}`), []byte(`,"future_extension":true}`), 1), reason: "frame_unknown_field", field: "future_extension"},
		{name: "duplicate same value", raw: bytes.Replace(valid, []byte(`{`), []byte(`{"schema":"`+sharedRuntimeAuthSchema+`",`), 1), reason: "frame_duplicate_field", field: "schema"},
		{name: "plain escaped duplicate", raw: bytes.Replace(valid, []byte(`{`), []byte(`{"\u0073chema":"`+sharedRuntimeAuthSchema+`",`), 1), reason: "frame_duplicate_field", field: "schema"},
		{name: "missing", raw: removeJSONMember(t, valid, "runtime_key"), reason: "frame_missing_field", field: "runtime_key"},
		{name: "second object", raw: append(append([]byte(nil), valid...), []byte(` {}`)...), reason: "frame_not_single_object"},
		{name: "array", raw: []byte(`[]`), reason: "frame_not_single_object"},
		{name: "truncated", raw: valid[:len(valid)-1], reason: "frame_unparseable"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, evidence, err := decodeSharedRuntimeAuthorizationFrame(testCase.raw)
			if testCase.ok {
				if err != nil || !reflect.DeepEqual(got, frame) || evidence.Decision != "admit" {
					t.Fatalf("valid decode got=%#v evidence=%#v err=%v", got, evidence, err)
				}
				return
			}
			var sharedErr *SharedRuntimeError
			if !errors.As(err, &sharedErr) || sharedErr.Code != "protocol_violation" || sharedErr.Reason != testCase.reason || sharedErr.MismatchField != testCase.field {
				t.Fatalf("refusal=%v want reason=%s field=%q evidence=%#v", err, testCase.reason, testCase.field, evidence)
			}
		})
	}
}

func removeJSONMember(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	delete(values, name)
	out, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
