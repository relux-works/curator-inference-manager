//go:build !windows

package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func TestReadPiTerminalEvidenceSummaryReturnsOnlyClosedAllowlistedFields(t *testing.T) {
	entry := writeLegacyPiEvidenceFixture(t, "ok", "")
	summary, err := ReadPiTerminalEvidenceSummary(entry)
	if err != nil {
		t.Fatalf("ReadPiTerminalEvidenceSummary: %v", err)
	}
	if summary.SchemaVersion != PiTerminalEvidenceSchemaVersion || summary.TerminalStatus != "ok" || summary.ClosedAt == "" {
		t.Fatalf("summary facts=%#v", summary)
	}
	for name, value := range map[string]string{
		"session identity digest": summary.SessionIdentityDigest,
		"source path digest":      summary.SourcePathDigest,
	} {
		if len(value) != 64 || value == strings.Repeat("0", 64) {
			t.Fatalf("%s is not a populated SHA-256 digest: %q", name, value)
		}
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema_version", "session_identity_digest", "closed_at", "terminal_status", "source_path_digest"}
	got := make([]string, 0, len(fields))
	for field := range fields {
		got = append(got, field)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary keys=%v want=%v", got, want)
	}
	if strings.Contains(string(encoded), testLegacyPiEvidenceEntryID) || strings.Contains(string(encoded), entry) || strings.Contains(string(encoded), "credential-secret") {
		t.Fatalf("summary leaked raw identity, path, or event detail: %s", encoded)
	}
}

func TestReadPiTerminalEvidenceSummaryRefusesActiveAndUnclassifiableState(t *testing.T) {
	t.Run("absolute path lookup failed", func(t *testing.T) {
		previous := absolutePiEvidencePath
		absolutePiEvidencePath = func(string) (string, error) { return "", os.ErrPermission }
		t.Cleanup(func() { absolutePiEvidencePath = previous })
		_, err := ReadPiTerminalEvidenceSummary("entry")
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.path")
	})

	t.Run("directory symlink", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		link := filepath.Join(t.TempDir(), "entry-link")
		if err := os.Symlink(entry, link); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(link)
		var refusal *PiTerminalEvidenceError
		if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable {
			t.Fatalf("symlink directory was accepted: %T %v", err, err)
		}
	})
	t.Run("non-directory path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(path, []byte("not a directory"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(path)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.directory") {
			t.Fatalf("non-directory input did not refuse at entry.directory: %T %v", err, err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		_, err := ReadPiTerminalEvidenceSummary(filepath.Join(t.TempDir(), "missing"))
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.directory")
	})
	t.Run("directory open failed", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := openPiEvidenceDirectory
		openPiEvidenceDirectory = func(string) (*os.File, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { openPiEvidenceDirectory = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.directory") || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("directory open error was not refused at entry.directory: %T %v", err, err)
		}
	})
	t.Run("directory permissions", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.Chmod(entry, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.directory")
	})
	t.Run("path stat error member", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := statPiEvidenceDirectoryPath
		statPiEvidenceDirectoryPath = func(path string, stat *syscall.Stat_t) error {
			if err := syscall.Stat(path, stat); err != nil {
				return err
			}
			return os.ErrPermission
		}
		t.Cleanup(func() { statPiEvidenceDirectoryPath = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.directory")
	})
	t.Run("path owner member", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := statPiEvidenceDirectoryPath
		statPiEvidenceDirectoryPath = func(path string, stat *syscall.Stat_t) error {
			if err := syscall.Stat(path, stat); err != nil {
				return err
			}
			stat.Uid++
			return nil
		}
		t.Cleanup(func() { statPiEvidenceDirectoryPath = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.directory")
	})

	t.Run("held flock", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		lock, err := os.OpenFile(filepath.Join(entry, "active.lock"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
		_, err = ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceActive, "entry.active_lock")
	})
	t.Run("opened directory identity member", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		other, err := os.Stat(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		previous := statPiEvidenceDirectoryFile
		statPiEvidenceDirectoryFile = func(*os.File) (os.FileInfo, error) { return other, nil }
		t.Cleanup(func() { statPiEvidenceDirectoryFile = previous })
		_, err = ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.directory") {
			t.Fatalf("opened directory identity mismatch was not refused at entry.directory: %T %v", err, err)
		}
	})
	t.Run("opened directory stat failed", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := statPiEvidenceDirectoryFile
		statPiEvidenceDirectoryFile = func(*os.File) (os.FileInfo, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { statPiEvidenceDirectoryFile = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.directory") || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("directory stat error was not refused at entry.directory: %T %v", err, err)
		}
	})
	t.Run("active lock stat error with otherwise valid metadata", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := fstatPiEvidenceActiveLock
		fstatPiEvidenceActiveLock = func(_ int, stat *syscall.Stat_t) error {
			stat.Nlink = 1
			return os.ErrPermission
		}
		t.Cleanup(func() { fstatPiEvidenceActiveLock = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.active_lock") {
			t.Fatalf("active lock stat failure was not refused at entry.active_lock: %T %v", err, err)
		}
	})
	t.Run("active lock link count changed after open", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := fstatPiEvidenceActiveLock
		fstatPiEvidenceActiveLock = func(_ int, stat *syscall.Stat_t) error {
			stat.Nlink = 2
			return nil
		}
		t.Cleanup(func() { fstatPiEvidenceActiveLock = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.active_lock") {
			t.Fatalf("changed active lock link count was not refused at entry.active_lock: %T %v", err, err)
		}
	})
	t.Run("missing active lock", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.Remove(filepath.Join(entry, "active.lock")); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.active_lock") || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing active lock did not preserve its active-lock refusal: %T %v", err, err)
		}
	})

	t.Run("missing record", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.Remove(filepath.Join(entry, "record.json")); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.record") || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing record did not preserve its record refusal: %T %v", err, err)
		}
	})
	t.Run("record read returned bytes with error", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		previous := readPiEvidenceRecord
		readPiEvidenceRecord = func(reader io.Reader) ([]byte, error) {
			data, err := io.ReadAll(reader)
			if err != nil {
				return data, err
			}
			return data, os.ErrPermission
		}
		t.Cleanup(func() { readPiEvidenceRecord = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.record") {
			t.Fatalf("record read error was not refused at entry.record: %T %v", err, err)
		}
	})

	t.Run("oversized record", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.WriteFile(filepath.Join(entry, "record.json"), make([]byte, 64<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.record")
	})
	t.Run("valid record at reader bound", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		path := filepath.Join(entry, "record.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, []byte(strings.Repeat(" ", (64<<10)-len(data)))...)
		if len(data) != 64<<10 {
			t.Fatalf("padded record length=%d", len(data))
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.record")
	})

	t.Run("record trailing value", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		path := filepath.Join(entry, "record.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, []byte(` {}`)...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.record")
	})

	t.Run("missing close timestamp", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		path := filepath.Join(entry, "record.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record legacyPiEvidenceRecord
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		record.ClosedAt = ""
		data, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = ReadPiTerminalEvidenceSummary(entry)
		var refusal *PiTerminalEvidenceError
		if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceActive {
			t.Fatalf("open terminal record was not reported active: %T %v", err, err)
		}
		if refusal.Site != "entry.closed_at" {
			t.Fatalf("open terminal record refusal site=%q want entry.closed_at", refusal.Site)
		}
	})

	t.Run("unknown record member", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		path := filepath.Join(entry, "record.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data[:len(data)-1], []byte(`,"future_member":true}`)...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.record")
	})

	t.Run("unknown terminal status", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "maybe", "")
		_, err := ReadPiTerminalEvidenceSummary(entry)
		var refusal *PiTerminalEvidenceError
		if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable {
			t.Fatalf("unknown status was accepted: %T %v", err, err)
		}
		if refusal.Site != "entry.terminal_status" {
			t.Fatalf("unknown status refusal site=%q want entry.terminal_status", refusal.Site)
		}
	})
	t.Run("directory descriptor stat error", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) {
			record.DirectoryDevice = 0
			record.DirectoryInode = 0
		})
		previous := fstatPiEvidenceDirectory
		fstatPiEvidenceDirectory = func(int, *syscall.Stat_t) error { return os.ErrPermission }
		t.Cleanup(func() { fstatPiEvidenceDirectory = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.identity")
	})
	t.Run("active identity fstat error", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) {
			record.ActiveDevice = 0
			record.ActiveInode = 0
		})
		previous := statPiEvidenceFile
		calls := 0
		statPiEvidenceFile = func(file *os.File) (piEvidenceFileIdentity, error) {
			calls++
			if calls == 1 {
				return piEvidenceFileIdentity{}, os.ErrPermission
			}
			return fileStat(file)
		}
		t.Cleanup(func() { statPiEvidenceFile = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.identity")
	})
	t.Run("log identity fstat error", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) {
			record.LogDevice = 0
			record.LogInode = 0
		})
		previous := statPiEvidenceFile
		calls := 0
		statPiEvidenceFile = func(file *os.File) (piEvidenceFileIdentity, error) {
			calls++
			if calls == 2 {
				return piEvidenceFileIdentity{}, os.ErrPermission
			}
			return fileStat(file)
		}
		t.Cleanup(func() { statPiEvidenceFile = previous })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.identity")
	})

	t.Run("events after terminal record", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "after-terminal")
		_, err := ReadPiTerminalEvidenceSummary(entry)
		var refusal *PiTerminalEvidenceError
		if !errors.As(err, &refusal) || refusal.State != PiTerminalEvidenceUnclassifiable || refusal.Site != "entry.terminal_status" {
			t.Fatalf("unclassifiable terminal ordering was accepted: %T %v", err, err)
		}
	})

	t.Run("record and filesystem members", func(t *testing.T) {
		cases := []struct {
			name   string
			site   string
			change func(*legacyPiEvidenceRecord)
		}{
			{name: "schema version", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.SchemaVersion = 2 }},
			{name: "entry id length", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.EntryID = strings.Repeat("a", 30) }},
			{name: "negative byte count", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.CommittedBytes = -1 }},
			{name: "zero record count", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.CommittedRecords = 0 }},
			{name: "invalid entry id hex", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.EntryID = strings.Repeat("g", 32) }},
			{name: "uppercase entry id", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.EntryID = strings.ToUpper(record.EntryID) }},
			{name: "invalid created at", site: "entry.record", change: func(record *legacyPiEvidenceRecord) { record.CreatedAt = "yesterday" }},
			{name: "invalid closed at", site: "entry.closed_at", change: func(record *legacyPiEvidenceRecord) { record.ClosedAt = "yesterday" }},
			{name: "directory device identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.DirectoryDevice++ }},
			{name: "directory inode identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.DirectoryInode++ }},
			{name: "active lock device identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.ActiveDevice++ }},
			{name: "active lock inode identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.ActiveInode++ }},
			{name: "committed log bytes", site: "entry.log", change: func(record *legacyPiEvidenceRecord) { record.CommittedBytes++ }},
			{name: "log record count", site: "entry.terminal_status", change: func(record *legacyPiEvidenceRecord) { record.CommittedRecords++ }},
			{name: "log device identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.LogDevice++ }},
			{name: "log inode identity", site: "entry.identity", change: func(record *legacyPiEvidenceRecord) { record.LogInode++ }},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				entry := writeLegacyPiEvidenceFixture(t, "ok", "")
				mutatePiEvidenceRecord(t, entry, testCase.change)
				_, err := ReadPiTerminalEvidenceSummary(entry)
				assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, testCase.site)
			})
		}
	})

	t.Run("missing log", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		if err := os.Remove(filepath.Join(entry, "log.jsonl")); err != nil {
			t.Fatal(err)
		}
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.log")
	})

	t.Run("log exceeds reader bound", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		path := filepath.Join(entry, "log.jsonl")
		data := make([]byte, piTerminalEvidenceMaxLogBytes+1)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) { record.CommittedBytes = int64(len(data)) })
		_, err := ReadPiTerminalEvidenceSummary(entry)
		assertPiEvidenceRefusalAt(t, err, PiTerminalEvidenceUnclassifiable, "entry.log")
	})
	t.Run("classifiable log exceeds reader bound", func(t *testing.T) {
		entry := writeLegacyPiEvidenceFixture(t, "ok", "")
		line := []byte(`{"timestamp":"2026-09-29T10:00:00Z","event":"tick"}` + "\n")
		terminal := []byte(`{"timestamp":"2026-09-29T10:00:10Z","event":"session_end","fields":{"status":"ok"}}` + "\n")
		count := piTerminalEvidenceMaxLogBytes/len(line) + 1
		data := append(bytes.Repeat(line, count), terminal...)
		if len(data) <= piTerminalEvidenceMaxLogBytes {
			t.Fatalf("generated log length=%d did not exceed bound", len(data))
		}
		if err := os.WriteFile(filepath.Join(entry, "log.jsonl"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		mutatePiEvidenceRecord(t, entry, func(record *legacyPiEvidenceRecord) {
			record.CommittedBytes = int64(len(data))
			record.CommittedRecords = uint64(count + 1)
		})
		_, err := ReadPiTerminalEvidenceSummary(entry)
		if !isPiEvidenceSite(err, PiTerminalEvidenceUnclassifiable, "entry.log") {
			t.Fatalf("classifiable oversized log was not refused at the log bound: %T %v", err, err)
		}
	})
}

func assertPiEvidenceRefusalAt(t *testing.T, err error, state PiTerminalEvidenceState, site string) {
	t.Helper()
	var refusal *PiTerminalEvidenceError
	if !errors.As(err, &refusal) || refusal.State != state || refusal.Site != site {
		t.Fatalf("evidence refusal=(%T %v) want state=%s site=%s", err, err, state, site)
	}
}

func isPiEvidenceSite(err error, state PiTerminalEvidenceState, site string) bool {
	var refusal *PiTerminalEvidenceError
	return errors.As(err, &refusal) && refusal.State == state && refusal.Site == site
}

func mutatePiEvidenceRecord(t *testing.T, entry string, change func(*legacyPiEvidenceRecord)) {
	t.Helper()
	path := filepath.Join(entry, "record.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record legacyPiEvidenceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	change(&record)
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

const testLegacyPiEvidenceEntryID = "0123456789abcdef0123456789abcdef"

func writeLegacyPiEvidenceFixture(t *testing.T, status, trailingEvent string) string {
	t.Helper()
	entry := filepath.Join(t.TempDir(), "entry-"+testLegacyPiEvidenceEntryID)
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(entry, "active.lock")
	active, err := os.OpenFile(activePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var activeStat syscall.Stat_t
	if err := syscall.Fstat(int(active.Fd()), &activeStat); err != nil {
		t.Fatal(err)
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"timestamp":"2026-09-29T10:00:00Z","event":"session_start","fields":{"session":"private-session","api_key":"credential-secret"}}`,
		`{"timestamp":"2026-09-29T10:00:10Z","event":"session_end","fields":{"status":"` + status + `"}}`,
	}
	if trailingEvent != "" {
		lines = append(lines, `{"timestamp":"2026-09-29T10:00:11Z","event":"`+trailingEvent+`"}`)
	}
	logBytes := []byte(strings.Join(lines, "\n") + "\n")
	logPath := filepath.Join(entry, "log.jsonl")
	if err := os.WriteFile(logPath, logBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var logStat syscall.Stat_t
	if err := syscall.Fstat(int(logFile.Fd()), &logStat); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	var dirStat syscall.Stat_t
	if err := syscall.Stat(entry, &dirStat); err != nil {
		t.Fatal(err)
	}
	record := legacyPiEvidenceRecord{
		SchemaVersion: 1, EntryID: testLegacyPiEvidenceEntryID,
		CreatedAt: "2026-09-29T10:00:00Z", ClosedAt: "2026-09-29T10:00:12Z",
		CommittedBytes: int64(len(logBytes)), CommittedRecords: uint64(len(lines)),
		DirectoryDevice: uint64(dirStat.Dev), DirectoryInode: dirStat.Ino,
		LogDevice: uint64(logStat.Dev), LogInode: logStat.Ino,
		ActiveDevice: uint64(activeStat.Dev), ActiveInode: activeStat.Ino,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "record.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return entry
}
