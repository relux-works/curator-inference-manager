//go:build darwin

package infra

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

type legacyFaultInfo struct {
	os.FileInfo
	system any
	mode   os.FileMode
}

func (info legacyFaultInfo) Sys() any { return info.system }
func (info legacyFaultInfo) Mode() os.FileMode {
	if info.mode != 0 {
		return info.mode
	}
	return info.FileInfo.Mode()
}

func TestImportLegacyEngineStateFilesystemRefusalsThroughProductionEntrypoint(t *testing.T) {
	attacks := []struct {
		name, site string
		action     string
		install    func(*testing.T, LegacyImportOptions)
	}{
		{"directory-stat", "filesystem.directory_read", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				if path == o.HomeDir {
					return nil, os.ErrPermission
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"directory-not-dir", "filesystem.directory_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				if path == o.HomeDir {
					return previous(o.ConfigPath)
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"directory-unknown-stat", "filesystem.directory_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				info, err := previous(path)
				if path == o.HomeDir && err == nil {
					return legacyFaultInfo{FileInfo: info}, nil
				}
				return info, err
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"directory-owner", "filesystem.directory_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				info, err := previous(path)
				if path == o.HomeDir && err == nil {
					stat := *info.Sys().(*syscall.Stat_t)
					stat.Uid++
					return legacyFaultInfo{FileInfo: info, system: &stat}, nil
				}
				return info, err
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"directory-adjacent-mode", "filesystem.directory_shape", "preview", func(t *testing.T, o LegacyImportOptions) { _ = os.Chmod(o.HomeDir, 0o702) }},
		{"directory-mode", "filesystem.directory_shape", "preview", func(t *testing.T, o LegacyImportOptions) { _ = os.Chmod(o.HomeDir, 0o722) }},
		{"source-lstat", "filesystem.lstat", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				if path == o.ConfigPath {
					return nil, os.ErrPermission
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"source-readlink", "filesystem.readlink", "preview", func(t *testing.T, o LegacyImportOptions) {
			_ = os.Remove(o.ConfigPath)
			_ = os.Symlink("not-read", o.ConfigPath)
			previous := legacyReadLink
			legacyReadLink = func(string) (string, error) { return "", os.ErrPermission }
			t.Cleanup(func() { legacyReadLink = previous })
		}},
		{"source-link-limit", "filesystem.link_size", "preview", func(t *testing.T, o LegacyImportOptions) {
			_ = os.Remove(o.ConfigPath)
			_ = os.Symlink("not-read", o.ConfigPath)
			previous := legacyReadLink
			legacyReadLink = func(string) (string, error) { return strings.Repeat("a", 4097), nil }
			t.Cleanup(func() { legacyReadLink = previous })
		}},
		{"source-open", "filesystem.open", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyOpen
			legacyOpen = func(path string, flags int, mode uint32) (int, error) {
				if path == o.ConfigPath {
					return -1, os.ErrPermission
				}
				return previous(path, flags, mode)
			}
			t.Cleanup(func() { legacyOpen = previous })
		}},
		{"file-stat", "filesystem.stat", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				if file.Name() == o.ConfigPath {
					return nil, os.ErrPermission
				}
				return previous(file)
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"file-type", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				info, err := previous(file)
				if file.Name() == o.ConfigPath && err == nil {
					return legacyFaultInfo{FileInfo: info, system: info.Sys(), mode: os.ModeNamedPipe | 0o600}, nil
				}
				return info, err
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"file-stat-unknown", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				info, err := previous(file)
				if file.Name() == o.ConfigPath && err == nil {
					return legacyFaultInfo{FileInfo: info}, nil
				}
				return info, err
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"file-owner", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				info, err := previous(file)
				if file.Name() == o.ConfigPath && err == nil {
					stat := *info.Sys().(*syscall.Stat_t)
					stat.Uid++
					return legacyFaultInfo{FileInfo: info, system: &stat}, nil
				}
				return info, err
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"file-adjacent-permissions", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) { _ = os.Chmod(o.ConfigPath, 0o602) }},
		{"file-permissions", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) { _ = os.Chmod(o.ConfigPath, 0o622) }},
		{"file-hardlink", "filesystem.file_shape", "preview", func(t *testing.T, o LegacyImportOptions) {
			if err := os.Link(o.ConfigPath, o.ConfigPath+".hardlink"); err != nil {
				t.Fatal(err)
			}
		}},
		{"snapshot-stat", "filesystem.snapshot_stat", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			calls := 0
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				if file.Name() == o.ConfigPath {
					calls++
					if calls == 2 {
						return nil, os.ErrPermission
					}
				}
				return previous(file)
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"snapshot-identity", "filesystem.identity", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyStat
			calls := 0
			legacyStat = func(file *os.File) (os.FileInfo, error) {
				info, err := previous(file)
				if file.Name() == o.ConfigPath {
					calls++
					if calls == 2 && err == nil {
						return legacyFaultInfo{FileInfo: info, system: info.Sys()}, nil
					}
				}
				return info, err
			}
			t.Cleanup(func() { legacyStat = previous })
		}},
		{"snapshot-replaced-inode-and-size", "filesystem.identity", "preview", func(t *testing.T, o LegacyImportOptions) {
			foreign := filepath.Join(filepath.Dir(o.ConfigPath), "replaced-source")
			if err := os.WriteFile(foreign, []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignInfo, err := os.Lstat(foreign)
			if err != nil {
				t.Fatal(err)
			}
			previous := legacyLstat
			legacyLstat = func(path string) (os.FileInfo, error) {
				if path == o.ConfigPath {
					return foreignInfo, nil
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyLstat = previous })
		}},
		{"source-read", "filesystem.read", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyReadAll
			legacyReadAll = func(io.Reader) ([]byte, error) { return nil, os.ErrPermission }
			t.Cleanup(func() { legacyReadAll = previous })
		}},
		{"source-size", "filesystem.size", "preview", func(t *testing.T, o LegacyImportOptions) {
			data, _ := os.ReadFile(o.ConfigPath)
			_ = os.WriteFile(o.ConfigPath, append(data, []byte(strings.Repeat("#comment\n", engineCatalogMaxFileBytes/8+2))...), 0o600)
		}},
		{"mkdir", "filesystem.mkdir", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyMkdir
			legacyMkdir = func(string, os.FileMode) error { return os.ErrPermission }
			t.Cleanup(func() { legacyMkdir = previous })
		}},
		{"temp", "filesystem.temp", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyCreateTemp
			legacyCreateTemp = func(string, string) (*os.File, error) { return nil, os.ErrPermission }
			t.Cleanup(func() { legacyCreateTemp = previous })
		}},
		{"chmod", "filesystem.chmod", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyChmod
			legacyChmod = func(*os.File, os.FileMode) error { return os.ErrPermission }
			t.Cleanup(func() { legacyChmod = previous })
		}},
		{"write", "filesystem.write", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyWrite
			legacyWrite = func(*os.File, []byte) (int, error) { return 0, os.ErrPermission }
			t.Cleanup(func() { legacyWrite = previous })
		}},
		{"sync", "filesystem.sync", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacySync
			legacySync = func(*os.File) error { return os.ErrPermission }
			t.Cleanup(func() { legacySync = previous })
		}},
		{"close", "filesystem.close", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyClose
			legacyClose = func(file *os.File) error {
				err := previous(file)
				if strings.Contains(filepath.Base(file.Name()), ".import-") && err == nil {
					return os.ErrPermission
				}
				return err
			}
			t.Cleanup(func() { legacyClose = previous })
		}},
		{"replace", "filesystem.replace", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyRename
			legacyRename = func(string, string) error { return os.ErrPermission }
			t.Cleanup(func() { legacyRename = previous })
		}},
		{"directory-sync-open", "filesystem.sync_open", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyOpenDirectory
			legacyOpenDirectory = func(string) (*os.File, error) { return nil, os.ErrPermission }
			t.Cleanup(func() { legacyOpenDirectory = previous })
		}},
		{"directory-sync", "filesystem.directory_sync", "apply", func(t *testing.T, o LegacyImportOptions) {
			previous := legacySync
			legacySync = func(file *os.File) error {
				info, err := file.Stat()
				if err == nil && info.IsDir() {
					return os.ErrPermission
				}
				return previous(file)
			}
			t.Cleanup(func() { legacySync = previous })
		}},
		{"cache-location", "state.cache_unknown", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyUserCacheDir
			legacyUserCacheDir = func() (string, error) { return "", os.ErrPermission }
			t.Cleanup(func() { legacyUserCacheDir = previous })
		}},
		{"manager-readdir", "state.read_unknown", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyReadDir
			legacyReadDir = func(path string) ([]os.DirEntry, error) {
				if strings.Contains(path, sharedRuntimeStateNamespace) {
					return nil, os.ErrPermission
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyReadDir = previous })
		}},
		{"source-evidence-readdir", "source.evidence_read", "preview", func(t *testing.T, o LegacyImportOptions) {
			previous := legacyReadDir
			legacyReadDir = func(path string) ([]os.DirEntry, error) {
				if path == o.EvidenceRoot {
					return nil, os.ErrPermission
				}
				return previous(path)
			}
			t.Cleanup(func() { legacyReadDir = previous })
		}},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			options := legacyImportFixture(t)
			options.EvidenceRoot = filepath.Join(filepath.Dir(options.HomeDir), "evidence")
			_ = os.Mkdir(options.EvidenceRoot, 0o700)
			if attack.name == "cache-location" {
				options.CacheRoot = ""
			}
			attack.install(t, options)
			_, err := ImportLegacyEngineState(options, attack.action)
			requireLegacySite(t, err, attack.site)
			if _, err := os.Stat(filepath.Join(options.HomeDir, ".curator", "engines.toml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fault wrote catalog")
			}
		})
	}
}
func TestImportLegacyEngineStateRecoveryFilesystemFailuresPreserveAuthority(t *testing.T) {
	for _, attack := range []string{"remove-target", "symlink", "link-replace", "remove-journal", "target-race", "journal-race", "concurrent"} {
		t.Run(attack, func(t *testing.T) {
			options := legacyImportFixture(t)
			if attack == "symlink" || attack == "link-replace" {
				_ = os.Mkdir(filepath.Join(options.HomeDir, ".curator"), 0o700)
				original := filepath.Join(options.HomeDir, "old.toml")
				_ = os.WriteFile(original, []byte(legacyInitialCatalog), 0o600)
				_ = os.Symlink("../old.toml", filepath.Join(options.HomeDir, ".curator", "engines.toml"))
			}
			legacyApply(t, options)
			action := "rollback"
			site := ""
			switch attack {
			case "remove-target":
				previous := legacyRemove
				legacyRemove = func(path string) error {
					if strings.HasSuffix(path, "engines.toml") {
						return os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyRemove = previous })
				site = "recovery.remove"
			case "symlink":
				previous := legacySymlink
				legacySymlink = func(string, string) error { return os.ErrPermission }
				t.Cleanup(func() { legacySymlink = previous })
				site = "recovery.symlink"
			case "link-replace":
				previous := legacyRename
				legacyRename = func(old, new string) error {
					if strings.HasSuffix(old, ".import-link") {
						return os.ErrPermission
					}
					return previous(old, new)
				}
				t.Cleanup(func() { legacyRename = previous })
				site = "recovery.link_replace"
			case "remove-journal":
				previous := legacyRemove
				legacyRemove = func(path string) error {
					if strings.HasSuffix(path, "journal.json") {
						return os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyRemove = previous })
				site = "recovery.journal_remove"
			case "target-race":
				previous := legacyWrite
				legacyWrite = func(file *os.File, data []byte) (int, error) {
					if bytes.Contains(data, []byte(`"phase":"rolling-back"`)) {
						path, _ := legacyTargetPath(options.HomeDir, legacyCatalogTarget)
						old, _ := os.ReadFile(path)
						_ = os.WriteFile(path, append(old, []byte("# drift\n")...), 0o600)
					}
					return previous(file, data)
				}
				t.Cleanup(func() { legacyWrite = previous })
				site = "recovery.target_race"
			case "journal-race":
				previous := legacyImportCheckpoint
				legacyImportCheckpoint = func(point string) error {
					if point == "before-staging" {
						journal, _ := legacyReadJournal(options.HomeDir)
						journal.SourceSHA256 = strings.Repeat("a", 64)
						return legacyWriteJournal(legacyTransactionRoot(options.HomeDir), *journal)
					}
					return nil
				}
				t.Cleanup(func() { legacyImportCheckpoint = previous })
				site = "import.journal_race"
			case "concurrent":
				lock, err := legacyOpenFile(filepath.Join(legacyTransactionRoot(options.HomeDir), "transaction.lock"), unix.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
				site = "import.concurrent"
			}
			_, err := ImportLegacyEngineState(options, action)
			requireLegacySite(t, err, site)
		})
	}
}
func TestLegacyManagerInactiveRefusesReadFailuresAndTypedState(t *testing.T) {
	for _, attack := range []string{"entry-not-dir", "runtime-readdir", "leases-readdir", "catalog-lstat", "status-failed"} {
		t.Run(attack, func(t *testing.T) {
			options := legacyImportFixture(t)
			legacyApply(t, options)
			root := filepath.Join(options.CacheRoot, sharedRuntimeStateNamespace, sharedRuntimePathVersion, "cold")
			_ = os.MkdirAll(root, 0o700)
			site := ""
			switch attack {
			case "entry-not-dir":
				_ = os.WriteFile(filepath.Join(filepath.Dir(root), "not-dir"), nil, 0o600)
				site = "state.shape_unknown"
			case "runtime-readdir":
				previous := legacyReadDir
				legacyReadDir = func(path string) ([]os.DirEntry, error) {
					if path == root {
						return nil, os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyReadDir = previous })
				site = "state.directory_unknown"
			case "leases-readdir":
				_ = os.Mkdir(filepath.Join(root, "leases"), 0o700)
				previous := legacyReadDir
				legacyReadDir = func(path string) ([]os.DirEntry, error) {
					if strings.HasSuffix(path, "leases") {
						return nil, os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyReadDir = previous })
				site = "state.lease_or_cwd"
			case "catalog-lstat":
				previous := legacyLstat
				legacyLstat = func(path string) (os.FileInfo, error) {
					if strings.HasSuffix(path, "engines.toml") {
						return nil, os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyLstat = previous })
				site = "state.catalog_unknown"
			case "status-failed":
				previous := sharedRuntimePathStat
				sharedRuntimePathStat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
				t.Cleanup(func() { sharedRuntimePathStat = previous })
				site = "state.status_unknown"
			}
			before := legacyTree(t, options.HomeDir)
			_, err := ImportLegacyEngineState(options, "preview")
			requireLegacySite(t, err, site)
			if !reflect.DeepEqual(before, legacyTree(t, options.HomeDir)) {
				t.Fatal("unknown read wrote state")
			}
		})
	}
}

func TestImportLegacyEngineStateOldNamespaceReadFailureStaysUnknown(t *testing.T) {
	for _, shape := range []string{"read", "shape", "cache"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes")
			_ = os.MkdirAll(root, 0o700)
			switch shape {
			case "read":
				previous := legacyReadDir
				legacyReadDir = func(path string) ([]os.DirEntry, error) {
					if path == root {
						return nil, os.ErrPermission
					}
					return previous(path)
				}
				t.Cleanup(func() { legacyReadDir = previous })
			case "shape":
				_ = os.WriteFile(filepath.Join(root, "unknown"), nil, 0o600)
			case "cache":
				previous := legacyUserCacheDir
				legacyUserCacheDir = func() (string, error) { return "", os.ErrPermission }
				t.Cleanup(func() { legacyUserCacheDir = previous })
				o.CacheRoot = ""
			}
			owner := SharedRuntimeOperatorOptions{HomeDir: o.HomeDir, CacheRoot: o.CacheRoot, ProjectDir: o.ProjectDir, Profile: "qwen"}
			_, err := AcquireSharedRuntimeLease(context.Background(), owner, "", os.Environ())
			site := "legacy_state.read_unknown"
			if shape == "shape" {
				site = "legacy_state.shape_unknown"
			}
			if shape == "cache" {
				site = "legacy_state.cache_unknown"
			}
			requireLegacySite(t, err, site)
			if shape != "cache" {
				_, err = ImportLegacyEngineState(o, "preview")
				requireLegacySite(t, err, site)
			}
		})
	}
}

func TestImportLegacyEngineStateRefusesSourceAndActiveStateRaceBeforeStaging(t *testing.T) {
	for _, shape := range []string{"source", "active"} {
		t.Run(shape, func(t *testing.T) {
			o := legacyImportFixture(t)
			previous := legacyImportCheckpoint
			legacyImportCheckpoint = func(point string) error {
				if point == "before-staging" {
					if shape == "source" {
						data, _ := os.ReadFile(o.ConfigPath)
						legacyChangeSourceWithCollidingDigestPrefix(t, o, data)
					} else {
						root := filepath.Join(o.CacheRoot, "agents-infra", "pi-runtimes", "old")
						_ = os.MkdirAll(root, 0o700)
						_ = os.WriteFile(filepath.Join(root, "broker-state.json"), []byte(`{"pid":123}`), 0o600)
					}
				}
				return nil
			}
			t.Cleanup(func() { legacyImportCheckpoint = previous })
			_, err := ImportLegacyEngineState(o, "apply")
			site := "import.source_race"
			if shape == "active" {
				site = "state.structured_json"
			}
			requireLegacySite(t, err, site)
			target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("race installed catalog")
			}
		})
	}
}

func TestImportLegacyEngineStateTargetRaceAfterLockedPlanRefuses(t *testing.T) {
	o := legacyImportFixture(t)
	target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
	previous := legacyImportCheckpoint
	legacyImportCheckpoint = func(point string) error {
		if point == "after-locked-plan" {
			if err := os.WriteFile(target, []byte(legacyInitialCatalog), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Cleanup(func() { legacyImportCheckpoint = previous })
	_, err := ImportLegacyEngineState(o, "apply")
	requireLegacySite(t, err, "import.target_race")
	data, _ := os.ReadFile(target)
	if !bytes.Equal(data, []byte(legacyInitialCatalog)) {
		t.Fatal("target race overwritten")
	}
}
func TestImportLegacyEngineStateDestinationResolvedLinkRaceRefuses(t *testing.T) {
	o := legacyImportFixture(t)
	_ = os.Mkdir(filepath.Join(o.HomeDir, ".curator"), 0o700)
	original := filepath.Join(o.HomeDir, "old.toml")
	_ = os.WriteFile(original, []byte(legacyInitialCatalog), 0o600)
	target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
	_ = os.Symlink("../old.toml", target)
	previous := legacyLstat
	legacyLstat = func(path string) (os.FileInfo, error) {
		if filepath.Base(path) == "old.toml" {
			_ = os.Remove(original)
			_ = os.Symlink(legacyInitialCatalog, original)
		}
		return previous(path)
	}
	t.Cleanup(func() { legacyLstat = previous })
	_, err := ImportLegacyEngineState(o, "preview")
	requireLegacySite(t, err, "destination.link_file")
}

func TestImportLegacyEngineStateConcurrentPartialSourceReadRefuses(t *testing.T) {
	o := legacyImportFixture(t)
	data, _ := os.ReadFile(o.ConfigPath)
	previous := legacyReadAll
	legacyReadAll = func(reader io.Reader) ([]byte, error) {
		if err := os.Truncate(o.ConfigPath, int64(len(data)-1)); err != nil {
			t.Fatal(err)
		}
		return previous(reader)
	}
	t.Cleanup(func() { legacyReadAll = previous })
	_, err := ImportLegacyEngineState(o, "preview")
	requireLegacySite(t, err, "filesystem.partial_read")
	target, _ := legacyTargetPath(o.HomeDir, legacyCatalogTarget)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial source created destination")
	}
}
func TestImportLegacyEngineStatePrivateArtifactAndDirectoryPermissionsRefuse(t *testing.T) {
	for _, artifact := range []string{"journal", "directory"} {
		t.Run(artifact, func(t *testing.T) {
			o := legacyImportFixture(t)
			legacyApply(t, o)
			site := "filesystem.file_shape"
			if artifact == "journal" {
				_ = os.Chmod(filepath.Join(legacyTransactionRoot(o.HomeDir), "journal.json"), 0o601)
			} else {
				_ = os.Chmod(filepath.Join(o.HomeDir, ".curator"), 0o701)
				site = "filesystem.directory_shape"
			}
			_, err := ImportLegacyEngineState(o, "preview")
			requireLegacySite(t, err, site)
		})
	}
}
