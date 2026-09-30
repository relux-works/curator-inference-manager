//go:build darwin

package infra

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenSharedFilesystemLogSinkRefusesOpenFailures(t *testing.T) {
	root := t.TempDir()
	sink := &sharedFilesystemLogSink{path: filepath.Join(root, "missing", "runtime.log")}
	if _, _, err := sink.Open(time.Now()); err == nil {
		t.Fatal("filesystem log sink admitted an open failure")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}
}

func TestOpenSharedRotatingLogRefusesFileStatFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.log")
	want := errors.New("injected runtime log stat failure")
	oldStat := sharedRuntimeLogFileStat
	var opened *os.File
	sharedRuntimeLogFileStat = func(file *os.File) (os.FileInfo, error) {
		opened = file
		return nil, want
	}
	t.Cleanup(func() { sharedRuntimeLogFileStat = oldStat })
	writer, err := openSharedRotatingLog(path, 64, 2, &fakeSharedLogClock{now: time.Now()})
	if !errors.Is(err, want) || sharedRuntimeErrorCode(err) != "shared_runtime_state_path_invalid" {
		t.Fatalf("runtime log stat refusal=%v want=%v", err, want)
	}
	if writer != nil {
		_ = writer.Close()
		t.Fatal("runtime log writer was returned after its active file stat failed")
	}
	if opened == nil {
		t.Fatal("file-stat failure was injected before production log open")
	}
	if _, err := opened.Write([]byte("x")); err == nil {
		t.Fatal("runtime log descriptor was not closed after stat failure")
	}
}

func TestRotateSharedFilesystemLogSinkRefusesRenameFailures(t *testing.T) {
	root := t.TempDir()
	sink := &sharedFilesystemLogSink{path: filepath.Join(root, "runtime.log")}
	if _, err := sink.Rotate(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("filesystem log sink admitted a failed active-log rename")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}
	if _, err := os.Stat(sink.path); err == nil {
		t.Fatal("failed rotation unexpectedly created a replacement active log")
	}
}

func TestPruneSharedFilesystemLogSinkRejectsEveryUnsafeArchive(t *testing.T) {
	tests := []struct {
		name, site string
		create     func(*testing.T, string, string)
	}{
		{name: "fifo type", site: "shared_runtime_state_path_invalid", create: func(t *testing.T, _, archive string) {
			if err := unix.Mkfifo(archive, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permissive mode", site: "shared_runtime_state_path_invalid", create: func(t *testing.T, _, archive string) {
			if err := os.WriteFile(archive, []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(archive, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "multiple links", site: "shared_runtime_state_path_invalid", create: func(t *testing.T, root, archive string) {
			target := filepath.Join(root, "foreign.log")
			if err := os.WriteFile(target, []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, archive); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "segment exceeds byte limit", site: "shared_runtime_state_path_invalid", create: func(t *testing.T, _, archive string) {
			if err := os.WriteFile(archive, []byte("oversized"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "prune removal failure", site: "shared_runtime_state_path_invalid", create: func(t *testing.T, _, archive string) {
			if err := os.WriteFile(archive, []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "runtime.log.00000000000000000001.20260829T120000.000000000Z")
			testCase.create(t, root, archive)
			if testCase.name == "prune removal failure" {
				if err := os.Chmod(root, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
			}
			sink := &sharedFilesystemLogSink{path: filepath.Join(root, "runtime.log")}
			if err := sink.Prune(4, 1); err == nil {
				t.Fatal("filesystem log sink admitted an unsafe managed archive")
			} else {
				requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
			}
		})
	}
}

func TestNextArchivePathRejectsSequenceOverflow(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.log")
	archive := path + ".18446744073709551615.20260829T120000.000000000Z"
	if err := os.WriteFile(archive, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &sharedFilesystemLogSink{path: path}
	if _, err := sink.nextArchivePath(time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("archive sequence wrapped after its maximum value")
	} else {
		requireSharedPathRefusal(t, err, "shared_runtime_state_path_invalid")
	}
}

func TestArchivesRefusesDirectoryReadFailure(t *testing.T) {
	sink := &sharedFilesystemLogSink{path: filepath.Join(t.TempDir(), "missing", "runtime.log")}
	if _, _, err := sink.archives(); err == nil {
		t.Fatal("filesystem log sink admitted an unreadable archive directory")
	} else {
		var shared *SharedRuntimeError
		if !errors.As(err, &shared) || shared.Code != "shared_runtime_state_path_invalid" {
			t.Fatalf("archive directory refusal=%v", err)
		}
	}
}
