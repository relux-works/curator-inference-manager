//go:build !windows

package infra

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	PiTerminalEvidenceSchemaVersion = 1
	piTerminalEvidenceMaxLogBytes   = 16 << 20
)

var absolutePiEvidencePath = filepath.Abs
var statPiEvidenceDirectoryPath = func(path string, stat *syscall.Stat_t) error { return syscall.Stat(path, stat) }
var openPiEvidenceDirectory = os.Open
var fstatPiEvidenceDirectory = func(fd int, stat *syscall.Stat_t) error { return syscall.Fstat(fd, stat) }
var statPiEvidenceDirectoryFile = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
var fstatPiEvidenceActiveLock = func(fd int, stat *syscall.Stat_t) error { return syscall.Fstat(fd, stat) }
var statPiEvidenceFile = fileStat
var readPiEvidenceRecord = func(reader io.Reader) ([]byte, error) { return io.ReadAll(reader) }

type PiTerminalEvidenceState string

const (
	PiTerminalEvidenceActive         PiTerminalEvidenceState = "active"
	PiTerminalEvidenceUnclassifiable PiTerminalEvidenceState = "unclassifiable"
)

// PiTerminalEvidenceError is a typed refusal. Its message never contains raw
// legacy event bytes, session identifiers, or source paths.
type PiTerminalEvidenceError struct {
	State PiTerminalEvidenceState
	Site  string
	Err   error
}

func (e *PiTerminalEvidenceError) Error() string {
	data, err := json.Marshal(struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Site  string `json:"site,omitempty"`
	}{
		Code: "pi_terminal_evidence_" + string(e.State), State: string(e.State),
		Site: e.Site,
	})
	if err != nil {
		return "pi_terminal_evidence_" + string(e.State)
	}
	return string(data)
}

func (e *PiTerminalEvidenceError) Unwrap() error { return e.Err }

// PiTerminalEvidenceSummary is the only record allowed to leave the legacy
// Pi lifecycle-log tree. Its four data fields are digests or terminal facts.
type PiTerminalEvidenceSummary struct {
	SchemaVersion         int    `json:"schema_version"`
	SessionIdentityDigest string `json:"session_identity_digest"`
	ClosedAt              string `json:"closed_at"`
	TerminalStatus        string `json:"terminal_status"`
	SourcePathDigest      string `json:"source_path_digest"`
}

type legacyPiEvidenceRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	EntryID          string `json:"entry_id"`
	CreatedAt        string `json:"created_at"`
	ClosedAt         string `json:"closed_at"`
	CommittedBytes   int64  `json:"committed_bytes"`
	CommittedRecords uint64 `json:"committed_records"`
	DirectoryDevice  uint64 `json:"directory_device"`
	DirectoryInode   uint64 `json:"directory_inode"`
	LogDevice        uint64 `json:"log_device"`
	LogInode         uint64 `json:"log_inode"`
	ActiveDevice     uint64 `json:"active_device"`
	ActiveInode      uint64 `json:"active_inode"`
}

type legacyPiEvidenceEvent struct {
	Timestamp string                     `json:"timestamp"`
	Event     string                     `json:"event"`
	Fields    map[string]json.RawMessage `json:"fields,omitempty"`
}

func piTerminalEvidenceRefuse(state PiTerminalEvidenceState, site string, err error) error {
	return &PiTerminalEvidenceError{State: state, Site: site, Err: err}
}

// ReadPiTerminalEvidenceSummary reads a legacy lifecycle envelope without
// copying it. It holds the envelope's active.lock while classifying the closed
// record, then returns only digests, closed_at, and the terminal status.
func ReadPiTerminalEvidenceSummary(entryDirectory string) (PiTerminalEvidenceSummary, error) {
	directoryPath, err := absolutePiEvidencePath(entryDirectory)
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.path", err)
	}
	directoryInfo, err := os.Lstat(directoryPath)
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", err)
	}
	if !directoryInfo.IsDir() {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", errors.New("entry must be a non-symlink directory"))
	}
	var pathDirectoryStat syscall.Stat_t
	if err := statPiEvidenceDirectoryPath(directoryPath, &pathDirectoryStat); err != nil || pathDirectoryStat.Uid != uint32(os.Geteuid()) || pathDirectoryStat.Mode&0o777 != 0o700 {
		if err == nil {
			err = errors.New("legacy lifecycle directory owner or permissions are invalid")
		}
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", err)
	}
	directory, err := openPiEvidenceDirectory(directoryPath)
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", err)
	}
	defer func() { _ = directory.Close() }()
	openedDirectoryInfo, err := statPiEvidenceDirectoryFile(directory)
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", err)
	}
	if !os.SameFile(directoryInfo, openedDirectoryInfo) {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.directory", errors.New("entry directory changed identity while opening"))
	}

	activeFile, _, err := openPiEvidenceFile(filepath.Join(directoryPath, "active.lock"))
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.active_lock", err)
	}
	defer func() { _ = activeFile.Close() }()
	var activeStat syscall.Stat_t
	if err := fstatPiEvidenceActiveLock(int(activeFile.Fd()), &activeStat); err != nil || activeStat.Nlink != 1 {
		if err == nil {
			err = errors.New("active lock link count is invalid")
		}
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.active_lock", err)
	}
	if err := syscall.Flock(int(activeFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceActive, "entry.active_lock", errors.New("legacy Pi session is active"))
		}
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.active_lock", err)
	}
	defer func() { _ = syscall.Flock(int(activeFile.Fd()), syscall.LOCK_UN) }()
	if err := inventoryPiEvidenceChildren(directoryPath); err != nil {
		return PiTerminalEvidenceSummary{}, err
	}

	var directoryStat syscall.Stat_t
	if err := fstatPiEvidenceDirectory(int(directory.Fd()), &directoryStat); err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.identity", err)
	}
	recordFile, _, err := openPiEvidenceFile(filepath.Join(directoryPath, "record.json"))
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", err)
	}
	defer func() { _ = recordFile.Close() }()
	recordBytes, err := readPiEvidenceRecord(io.LimitReader(recordFile, 64<<10))
	if err != nil || len(recordBytes) == 64<<10 {
		if err == nil {
			err = errors.New("legacy lifecycle record exceeds 64 KiB")
		}
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", err)
	}
	var record legacyPiEvidenceRecord
	if err := legacyDecodeClosedJSON(recordBytes, &record); err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", errors.New("legacy lifecycle record is malformed"))
	}
	if record.SchemaVersion != 1 || len(record.EntryID) != 32 || record.CommittedBytes < 0 || record.CommittedRecords == 0 {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", errors.New("legacy lifecycle record identity or counters are invalid"))
	}
	if _, err := hex.DecodeString(record.EntryID); err != nil || strings.ToLower(record.EntryID) != record.EntryID {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", errors.New("legacy lifecycle entry identity is invalid"))
	}
	if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.record", errors.New("legacy lifecycle created_at is invalid"))
	}
	if record.ClosedAt == "" {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceActive, "entry.closed_at", errors.New("legacy Pi session has no closed_at timestamp"))
	}
	if _, err := time.Parse(time.RFC3339Nano, record.ClosedAt); err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.closed_at", errors.New("legacy lifecycle closed_at is invalid"))
	}
	activeIdentity, err := statPiEvidenceFile(activeFile)
	if err != nil || record.DirectoryDevice != uint64(directoryStat.Dev) || record.DirectoryInode != directoryStat.Ino || record.ActiveDevice != activeIdentity.device || record.ActiveInode != activeIdentity.inode {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.identity", errors.New("legacy lifecycle filesystem identity does not match"))
	}

	logFile, logInfo, err := openPiEvidenceFile(filepath.Join(directoryPath, "log.jsonl"))
	if err != nil {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.log", err)
	}
	defer func() { _ = logFile.Close() }()
	if logInfo.Size() != record.CommittedBytes || logInfo.Size() > piTerminalEvidenceMaxLogBytes {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.log", errors.New("legacy log size is not committed or exceeds the summary reader limit"))
	}
	logStat, err := statPiEvidenceFile(logFile)
	if err != nil || record.LogDevice != logStat.device || record.LogInode != logStat.inode {
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.identity", errors.New("legacy log filesystem identity does not match"))
	}
	status, count, err := classifyLegacyPiTerminalStatus(logFile)
	if err != nil || count != record.CommittedRecords {
		if err == nil {
			err = errors.New("legacy log record count does not match its committed envelope")
		}
		return PiTerminalEvidenceSummary{}, piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.terminal_status", err)
	}

	return PiTerminalEvidenceSummary{
		SchemaVersion:         PiTerminalEvidenceSchemaVersion,
		SessionIdentityDigest: digestPiEvidenceValue("curator.pi.session.v1\x00", record.EntryID),
		ClosedAt:              record.ClosedAt, TerminalStatus: status,
		SourcePathDigest: digestPiEvidenceValue("curator.pi.source-path.v1\x00", filepath.Clean(directoryPath)),
	}, nil
}

func inventoryPiEvidenceChildren(path string) error {
	children, err := os.ReadDir(path)
	if err != nil {
		return piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.children_read", err)
	}
	for _, child := range children {
		if child.Name() != "active.lock" && child.Name() != "record.json" && child.Name() != "log.jsonl" {
			return piTerminalEvidenceRefuse(PiTerminalEvidenceUnclassifiable, "entry.child_unknown", errors.New("unclassified lifecycle child"))
		}
		// Each admitted name is opened below with the existing regular-file,
		// no-symlink, owner/mode and single-link checks before its data is read.
	}
	return nil
}

type piEvidenceFileIdentity struct{ device, inode uint64 }

func fileStat(file *os.File) (piEvidenceFileIdentity, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return piEvidenceFileIdentity{}, err
	}
	return piEvidenceFileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func openPiEvidenceFile(path string) (*os.File, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0o077 != 0 || pathInfo.Mode().Perm()&0o400 == 0 {
		return nil, nil, errors.New("legacy lifecycle child must be a private regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		if err == nil {
			err = errors.New("legacy lifecycle child changed identity while opening")
		}
		return nil, nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 {
		_ = file.Close()
		if err == nil {
			err = errors.New("legacy lifecycle child identity or permissions are invalid")
		}
		return nil, nil, err
	}
	return file, openedInfo, nil
}

func classifyLegacyPiTerminalStatus(file *os.File) (string, uint64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	scanner := bufio.NewScanner(io.LimitReader(file, piTerminalEvidenceMaxLogBytes+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var count uint64
	terminalStatus := ""
	terminalCount := 0
	for scanner.Scan() {
		if terminalCount != 0 {
			return "", count, errors.New("legacy events follow the terminal session_end event")
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			return "", count, errors.New("legacy log contains an empty event")
		}
		var event legacyPiEvidenceEvent
		if err := legacyDecodeClosedJSON(line, &event); err != nil {
			return "", count, errors.New("legacy log contains an unclassifiable event")
		}
		if event.Event == "" {
			return "", count, errors.New("legacy log event name is absent")
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			return "", count, errors.New("legacy log event timestamp is invalid")
		}
		count++
		if event.Event == "session_end" {
			terminalCount++
			raw, ok := event.Fields["status"]
			if !ok || json.Unmarshal(raw, &terminalStatus) != nil || (terminalStatus != "ok" && terminalStatus != "error") {
				return "", count, errors.New("legacy terminal status is unknown")
			}
			for field, value := range event.Fields {
				if field != "status" && field != "error" {
					return "", count, fmt.Errorf("legacy session_end field %q is unrecognized", field)
				}
				if field == "error" {
					var detail string
					if json.Unmarshal(value, &detail) != nil {
						return "", count, errors.New("legacy terminal error field is malformed")
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", count, err
	}
	if terminalCount != 1 || terminalStatus == "" {
		return "", count, errors.New("legacy log does not contain exactly one classifiable terminal event")
	}
	return terminalStatus, count, nil
}

func digestPiEvidenceValue(prefix, value string) string {
	digest := sha256.Sum256([]byte(prefix + value))
	return hex.EncodeToString(digest[:])
}
