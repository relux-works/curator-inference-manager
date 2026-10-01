//go:build !windows

package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"golang.org/x/sys/unix"
)

// Targets and evidence paths are owner-local; the journal never chooses a path.
const legacyCatalogTarget = "engine-catalog"
const legacyEvidenceTarget = "terminal-evidence"
const legacyImportDirectory = ".legacy-import"

type LegacyImportOptions struct {
	HomeDir      string
	ConfigPath   string
	EvidenceRoot string
	CacheRoot    string
	ProjectDir   string
}
type LegacyImportReport struct {
	FieldPlan    map[string]map[string]string `json:"field_plan"`
	Action       string                       `json:"action"`
	SourceSHA256 string                       `json:"source_sha256"`
	Targets      []string                     `json:"targets"`
	NotRunnable  map[string]string            `json:"not_runnable"`
	Checks       map[string]EngineCheckReport `json:"checks"`
}
type legacyImportPlan struct {
	report      LegacyImportReport
	after       map[string][]byte
	before      map[string]LegacyImportFileState
	beforeBytes map[string][]byte
}

// Fault checkpoints are also used by the subprocess crash-recovery tests.
var legacyImportCheckpoint = func(string) error { return nil }
var legacyLstat = os.Lstat
var legacyReadDir = os.ReadDir
var legacyReadLink = os.Readlink
var legacyOpen = unix.Open
var legacyStat = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
var legacyReadAll = io.ReadAll
var legacyMkdir = os.Mkdir
var legacyCreateTemp = os.CreateTemp
var legacyChmod = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }
var legacyWrite = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
var legacySync = func(file *os.File) error { return file.Sync() }
var legacyClose = func(file *os.File) error { return file.Close() }
var legacyRename = os.Rename
var legacyRemove = os.Remove
var legacySymlink = os.Symlink
var legacyOpenDirectory = os.Open
var legacyUserCacheDir = os.UserCacheDir
var legacyImportStatusReport = SharedRuntimeStatusReport

func legacyTargetPath(home, id string) (string, error) {
	switch id {
	case legacyCatalogTarget:
		return filepath.Join(home, ".curator", "engines.toml"), nil
	case legacyEvidenceTarget:
		return filepath.Join(home, ".curator", "terminal-evidence.json"), nil
	}
	return "", legacyImportRefuse("target.allowlist", errors.New("target is not owned by importer"))
}
func legacyTransactionRoot(home string) string {
	return filepath.Join(home, ".curator", legacyImportDirectory)
}

// ImportLegacyEngineState is preview-first. Recovery and rollback preflight all
// targets and backups before replacing anything; legacy inputs remain read-only.
func ImportLegacyEngineState(options LegacyImportOptions, action string) (LegacyImportReport, error) {
	empty := LegacyImportReport{Action: action, Targets: []string{}, NotRunnable: map[string]string{}}
	if action != "preview" && action != "apply" && action != "resume" && action != "rollback" {
		return empty, legacyImportRefuse("import.action", errors.New("unknown action"))
	}
	if !filepath.IsAbs(options.HomeDir) {
		return empty, legacyImportRefuse("import.home", errors.New("absolute home required"))
	}
	if err := legacySafeDirectoryWithMask(options.HomeDir, false, 0o022); err != nil {
		return empty, err
	}
	if err := legacySafeDirectory(filepath.Join(options.HomeDir, ".curator"), true); err != nil {
		return empty, err
	}
	journal, err := legacyReadJournal(options.HomeDir)
	if err != nil {
		return empty, err
	}
	if action == "preview" {
		plan, err := legacyPlan(options, journal)
		if err == nil && journal != nil && journal.Phase != "committed" && journal.SourceSHA256 != plan.report.SourceSHA256 {
			return empty, legacyImportRefuse("recovery.source", errors.New("source changed"))
		}
		plan.report.Action = action
		return plan.report, err
	}
	if action == "resume" || action == "rollback" {
		if journal == nil {
			return empty, legacyImportRefuse("recovery.missing_journal", errors.New("no transaction"))
		}
	}
	// All source and manager-state checks happen before creating a lock or staging.
	plan, err := legacyPlan(options, journal)
	if err != nil {
		return empty, err
	}
	plan.report.Action = action
	if journal != nil && journal.Phase != "committed" {
		if journal.SourceSHA256 != plan.report.SourceSHA256 {
			return empty, legacyImportRefuse("recovery.source", errors.New("source changed"))
		}
		if action == "apply" {
			action = "resume"
		}
		if journal.Phase == "rolling-back" && action == "resume" {
			action = "rollback"
		}
	}
	if journal != nil && action == "rollback" && journal.SourceSHA256 != plan.report.SourceSHA256 {
		return empty, legacyImportRefuse("recovery.source", errors.New("source changed"))
	}
	if len(plan.after) == 0 && (journal == nil || journal.Phase == "committed") && action != "rollback" {
		return plan.report, nil
	}
	if err := legacyImportCheckpoint("before-staging"); err != nil {
		return empty, err
	}
	if _, err := legacyPlan(options, journal); err != nil {
		return empty, err
	}
	root := legacyTransactionRoot(options.HomeDir)
	if err := legacyCreatePrivateDirectory(filepath.Dir(root)); err != nil {
		return empty, err
	}
	if err := legacyCreatePrivateDirectory(root); err != nil {
		return empty, err
	}
	lock, err := legacyOpenFile(filepath.Join(root, "transaction.lock"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return empty, err
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return empty, legacyImportRefuse("import.concurrent", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	// Re-read under the owner-local transaction lock; no stale preflight writes.
	latest, err := legacyReadJournal(options.HomeDir)
	if err != nil {
		return empty, err
	}
	if !legacyJournalsEqual(journal, latest) {
		return empty, legacyImportRefuse("import.journal_race", errors.New("journal changed"))
	}
	fresh, err := legacyPlan(options, latest)
	if err != nil {
		return empty, err
	}
	if fresh.report.SourceSHA256 != plan.report.SourceSHA256 {
		return empty, legacyImportRefuse("import.source_race", errors.New("source changed after preview"))
	}
	if err := legacyImportCheckpoint("after-locked-plan"); err != nil {
		return empty, err
	}
	if action == "resume" || action == "rollback" {
		if err := legacyRecover(options, *journal, action); err != nil {
			return empty, err
		}
		return plan.report, nil
	}

	next := LegacyImportJournal{Format: LegacyImportJournalFormat, TransactionID: "mig-" + plan.report.SourceSHA256[:32], SourceSHA256: plan.report.SourceSHA256, Phase: "prepared"}
	ids := make([]string, 0, len(plan.after))
	for id := range plan.after {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path, _ := legacyTargetPath(options.HomeDir, id)
		current, _, err := legacySnapshot(path)
		if err != nil {
			return empty, err
		}
		if !legacyStatesEqual(current, plan.before[id]) {
			return empty, legacyImportRefuse("import.target_race", errors.New("target changed"))
		}
		after := LegacyImportFileState{Kind: "file", SHA256: legacyString(legacyDigest(plan.after[id])), Mode: 0o600}
		target := LegacyImportTarget{TargetID: id, Before: current, After: after}
		prefix := next.TransactionID + "-" + id
		if current.Kind != "absent" {
			target.BackupRef = legacyString(prefix + "-before")
			target.BackupSHA256 = current.SHA256
			if err := legacyAtomicFile(filepath.Join(root, *target.BackupRef), plan.beforeBytes[id], 0o600); err != nil {
				return empty, err
			}
		}
		if err := legacyAtomicFile(filepath.Join(root, prefix+"-after"), plan.after[id], 0o600); err != nil {
			return empty, err
		}
		next.Targets = append(next.Targets, target)
	}
	if err := legacyWriteJournal(root, next); err != nil {
		return empty, err
	}
	if err := legacyImportCheckpoint("after-backup"); err != nil {
		return empty, err
	}
	if err := legacyRecover(options, next, "resume"); err != nil {
		return empty, err
	}
	return plan.report, nil
}
func legacyJournalsEqual(a, b *LegacyImportJournal) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func legacyStatesEqual(a, b LegacyImportFileState) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func legacyPlan(options LegacyImportOptions, journal *LegacyImportJournal) (legacyImportPlan, error) {
	plan := legacyImportPlan{report: LegacyImportReport{Targets: []string{}, NotRunnable: map[string]string{}}, after: map[string][]byte{}, before: map[string]LegacyImportFileState{}, beforeBytes: map[string][]byte{}}
	presence := map[string]map[string]bool{}
	plan.report.FieldPlan = map[string]map[string]string{}
	source := EngineCatalogDocument{SchemaVersion: 1, Engines: map[string]EngineCatalogEntry{}}
	digest := []byte("legacy-import-input/v1\x00")
	if options.ConfigPath != "" {
		state, data, err := legacySnapshotWithMask(options.ConfigPath, 0o022)
		if err != nil {
			return plan, err
		}
		if state.Kind == "symlink" {
			return plan, legacyImportRefuse("source.symlink", errors.New("source must be regular"))
		}
		if state.Kind != "absent" {
			source, plan.report.NotRunnable, err = readLegacyEngineProfiles(data, presence)
			if err != nil {
				return plan, err
			}
			digest = append(digest, []byte("profiles\x00")...)
			digest = append(digest, data...)
		}
	}
	summaries, err := legacyReadEvidence(options.EvidenceRoot)
	if err != nil {
		return plan, err
	}
	summaryBytes, err := json.Marshal(summaries)
	if err != nil {
		return plan, err
	}
	digest = append(digest, []byte("\x00evidence\x00")...)
	digest = append(digest, summaryBytes...)
	plan.report.SourceSHA256 = legacyDigest(digest)
	if err := legacyManagerInactive(options, source); err != nil {
		return plan, err
	}
	if journal != nil {
		if _, _, err := legacyRecoveryPreflight(options, *journal, "resume"); err != nil {
			return plan, err
		}
	}
	if journal != nil && journal.Phase != "committed" {
		for _, target := range journal.Targets {
			plan.report.Targets = append(plan.report.Targets, target.TargetID)
		}
		return plan, nil
	}
	for _, id := range []string{legacyCatalogTarget, legacyEvidenceTarget} {
		path, _ := legacyTargetPath(options.HomeDir, id)
		before, data, err := legacySnapshot(path)
		if err != nil {
			return plan, err
		}
		plan.before[id] = before
		plan.beforeBytes[id] = data
	}
	catalogState := plan.before[legacyCatalogTarget]
	catalogBytes := plan.beforeBytes[legacyCatalogTarget]
	if catalogState.Kind == "symlink" {
		path, _ := legacyTargetPath(options.HomeDir, legacyCatalogTarget)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return plan, legacyImportRefuse("destination.link", err)
		}
		state, data, err := legacySnapshot(resolved)
		if err != nil {
			return plan, err
		}
		if state.Kind != "file" {
			return plan, legacyImportRefuse("destination.link_file", errors.New("link must resolve to file"))
		}
		catalogBytes = data
	}
	before := EngineCatalogDocument{SchemaVersion: 1, Engines: map[string]EngineCatalogEntry{}}
	if catalogState.Kind != "absent" {
		before, err = legacyCatalogDocument(catalogBytes)
		if err != nil {
			return plan, err
		}
	}
	owned := false
	if journal != nil && journal.Phase == "committed" {
		for _, target := range journal.Targets {
			if target.TargetID == legacyCatalogTarget {
				owned = legacyStatesEqual(catalogState, target.After)
			}
		}
	}
	merged, changed, err := legacyMergeCatalog(before, source, owned, presence, plan.report.FieldPlan)
	if err != nil {
		return plan, err
	}
	// Reports describe the effective merged fields consumed by apply. Presence
	// does not replace retained destination argv with a source default in preview.
	plan.report.Checks = map[string]EngineCheckReport{}
	for name := range source.Engines {
		entry := merged.Engines[name]
		_, plan.report.NotRunnable[name] = legacyServedModelID(entry)
		if code, _ := engineCatalogExecutableNotRunnable(entry); code != "" {
			plan.report.NotRunnable[name] = code
		}
		kind := entry.Engine
		if entry.Mode == "ssh" {
			kind = "ssh"
		}
		plan.report.Checks[name] = CheckEngineEntry(EngineCheckEntry{Name: name, Kind: kind, Argv: engineCatalogIdentityArgv(entry, 1), Directory: filepath.Join(options.HomeDir, ".curator")})
	}
	if changed {
		data, err := MarshalEngineCatalog(merged)
		if err != nil {
			return plan, err
		}
		plan.after[legacyCatalogTarget] = data
	}
	if len(summaries) > 0 {
		state := plan.before[legacyEvidenceTarget]
		old := plan.beforeBytes[legacyEvidenceTarget]
		if state.Kind != "absent" && !bytes.Equal(old, summaryBytes) {
			evidenceOwned := false
			if journal != nil && journal.Phase == "committed" {
				for _, target := range journal.Targets {
					if target.TargetID == legacyEvidenceTarget {
						evidenceOwned = legacyStatesEqual(state, target.After)
					}
				}
			}
			if !evidenceOwned {
				return plan, legacyImportRefuse("destination.evidence_conflict", errors.New("existing terminal evidence differs"))
			}
			// The old bytes are a digest-verified importer output. Keep summaries
			// whose source disappears; update only explicitly present identities.
			var previous []PiTerminalEvidenceSummary
			if err := legacyDecodeClosedJSON(old, &previous); err != nil {
				return plan, legacyImportRefuse("destination.evidence_decode", err)
			}
			byIdentity := map[string]PiTerminalEvidenceSummary{}
			for _, summary := range previous {
				byIdentity[summary.SessionIdentityDigest] = summary
			}
			for _, summary := range summaries {
				byIdentity[summary.SessionIdentityDigest] = summary
			}
			summaries = summaries[:0]
			for _, summary := range byIdentity {
				summaries = append(summaries, summary)
			}
			sort.Slice(summaries, func(i, j int) bool { return summaries[i].SessionIdentityDigest < summaries[j].SessionIdentityDigest })
			summaryBytes, _ = json.Marshal(summaries) // Only fixed string/integer summary fields.
			if !bytes.Equal(old, summaryBytes) {
				plan.after[legacyEvidenceTarget] = summaryBytes
			}
		}
		if state.Kind == "absent" {
			plan.after[legacyEvidenceTarget] = summaryBytes
		}
	}
	for id := range plan.after {
		plan.report.Targets = append(plan.report.Targets, id)
	}
	sort.Strings(plan.report.Targets)
	return plan, nil
}

func legacyReadEvidence(root string) ([]PiTerminalEvidenceSummary, error) {
	summaries := []PiTerminalEvidenceSummary{}
	if root == "" {
		return summaries, nil
	}
	if err := legacySafeDirectory(root, true); err != nil {
		return nil, err
	}
	entries, err := legacyReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return summaries, nil
	}
	if err != nil {
		return nil, legacyImportRefuse("source.evidence_read", err)
	}
	seenIdentities := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, legacyImportRefuse("source.evidence_shape", errors.New("unclassifiable lifecycle entry"))
		}
		summary, err := ReadPiTerminalEvidenceSummary(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if seenIdentities[summary.SessionIdentityDigest] {
			return nil, legacyImportRefuse("source.evidence_duplicate", errors.New("conflicting lifecycle identity"))
		}
		seenIdentities[summary.SessionIdentityDigest] = true
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].SessionIdentityDigest < summaries[j].SessionIdentityDigest })
	return summaries, nil
}

// Read-only broker inspection uses the existing state namespace. No ownership,
// cleanup, lease creation, signals or quarantine actions are performed.
func legacyManagerInactive(options LegacyImportOptions, source EngineCatalogDocument) error {
	cache := options.CacheRoot
	if cache == "" {
		var err error
		cache, err = legacyUserCacheDir()
		if err != nil {
			return legacyImportRefuse("state.cache_unknown", err)
		}
	}
	if err := RefuseActiveLegacyEngineState(cache); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(cache, sharedRuntimeStateNamespace, sharedRuntimePathVersion), filepath.Join(cache, sharedRuntimeStateNamespace, sharedRuntimeRendezvousName)} {
		if err := legacySafeDirectory(path, true); err != nil {
			return err
		}
		entries, err := legacyReadDir(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return legacyImportRefuse("state.read_unknown", err)
		}
		// A cold state root may contain ledgers and logs. Active records, leases,
		// sockets or unknown shapes refuse rather than guessing terminal ownership.
		for _, entry := range entries {
			if !entry.IsDir() {
				return legacyImportRefuse("state.shape_unknown", errors.New("state directory required"))
			}
			if err := legacyScanColdRuntime(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}
	// Existing configured selected engines additionally use the production status
	// reader. A failed status read is unknown, never a cold/absent fallback.
	catalog := filepath.Join(options.HomeDir, ".curator", "engines.toml")
	for name := range source.Engines {
		info, err := legacyLstat(catalog)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return legacyImportRefuse("state.catalog_unknown", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		_, state, catalogErr := ReadEngineCatalogEntry(catalog, name)
		if catalogErr != nil {
			return catalogErr
		}
		if state == EngineCatalogAbsent {
			continue
		}
		status, err := legacyImportStatusReport(SharedRuntimeOperatorOptions{HomeDir: options.HomeDir, CacheRoot: cache, ProjectDir: options.ProjectDir, Profile: name})
		if err != nil {
			return legacyImportRefuse("state.status_unknown", err)
		}
		if status.Broker.State != "absent" || len(status.Leases) != 0 || status.Runtime != nil {
			return legacyImportRefuse("state.active", errors.New("engine is active or unclassifiable"))
		}
	}
	return nil
}
func legacyScanColdRuntime(path string) error {
	if err := legacySafeDirectory(path, false); err != nil {
		return err
	}
	entries, err := legacyReadDir(path)
	if err != nil {
		return legacyImportRefuse("state.directory_unknown", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		switch name {
		case "cwd", "leases":
			if err := legacySafeDirectory(filepath.Join(path, name), false); err != nil {
				return err
			}
			contents, err := legacyReadDir(filepath.Join(path, name))
			if err != nil || len(contents) != 0 {
				return legacyImportRefuse("state.lease_or_cwd", errors.New("nonempty or unreadable state"))
			}
		case sharedRuntimeBrokerLogName, sharedRuntimeLogName:
			state, _, err := legacySnapshot(filepath.Join(path, name))
			if err != nil {
				return err
			}
			if state.Kind != "file" {
				return legacyImportRefuse("state.log_shape", errors.New("unclassifiable state"))
			}
		case sharedRuntimeRestartLedgerName, sharedRuntimeBrokerStateName:
			if err := legacyValidateColdState(filepath.Join(path, name)); err != nil {
				return err
			}
		case "broker.lock":
			file, err := legacyOpenFile(filepath.Join(path, name), unix.O_RDONLY, 0)
			if err != nil {
				return err
			}
			err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			}
			_ = legacyClose(file)
			if err != nil {
				return legacyImportRefuse("state.active_lock", err)
			}
		default:
			return legacyImportRefuse("state.unclassifiable", errors.New("active record, socket, or unknown state"))
		}
	}
	return nil
}

func legacySafeDirectory(path string, absentOK bool) error {
	return legacySafeDirectoryWithMask(path, absentOK, 0o077)
}
func legacySafeDirectoryWithMask(path string, absentOK bool, unsafePermissions os.FileMode) error {
	info, err := legacyLstat(path)
	if absentOK && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return legacyImportRefuse("filesystem.directory_read", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&unsafePermissions != 0 {
		return legacyImportRefuse("filesystem.directory_shape", errors.New("private owner directory required"))
	}
	return nil
}
func legacyCreatePrivateDirectory(path string) error {
	if err := legacyMkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return legacyImportRefuse("filesystem.mkdir", err)
	}
	return legacySafeDirectory(path, false)
}
func legacyOpenFile(path string, flags int, mode uint32) (*os.File, error) {
	return legacyOpenFileWithMask(path, flags, mode, 0o077)
}
func legacyOpenFileWithMask(path string, flags int, mode uint32, unsafePermissions os.FileMode) (*os.File, error) {
	fd, err := legacyOpen(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, legacyImportRefuse("filesystem.open", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := legacyStat(file)
	if err != nil {
		_ = legacyClose(file)
		return nil, legacyImportRefuse("filesystem.stat", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&unsafePermissions != 0 || stat.Nlink != 1 {
		_ = legacyClose(file)
		return nil, legacyImportRefuse("filesystem.file_shape", errors.New("private single-link regular file required"))
	}
	return file, nil
}
func legacySnapshot(path string) (LegacyImportFileState, []byte, error) {
	return legacySnapshotWithMask(path, 0o077)
}
func legacySnapshotWithMask(path string, unsafePermissions os.FileMode) (LegacyImportFileState, []byte, error) {
	absent := LegacyImportFileState{Kind: "absent"}
	info, err := legacyLstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return absent, nil, nil
	}
	if err != nil {
		return absent, nil, legacyImportRefuse("filesystem.lstat", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := legacyReadLink(path)
		if err != nil {
			return absent, nil, legacyImportRefuse("filesystem.readlink", err)
		}
		if len(target) > 4096 {
			return absent, nil, legacyImportRefuse("filesystem.link_size", errors.New("link too long"))
		}
		data := []byte(target)
		return LegacyImportFileState{Kind: "symlink", SHA256: legacyString(legacyDigest(data)), LinkTarget: legacyString(target)}, data, nil
	}
	file, err := legacyOpenFileWithMask(path, unix.O_RDONLY, 0, unsafePermissions)
	if err != nil {
		return absent, nil, err
	}
	defer func() { _ = legacyClose(file) }()
	opened, err := legacyStat(file)
	if err != nil {
		return absent, nil, legacyImportRefuse("filesystem.snapshot_stat", err)
	}
	if !os.SameFile(info, opened) {
		return absent, nil, legacyImportRefuse("filesystem.identity", errors.New("file changed"))
	}
	data, err := legacyReadAll(io.LimitReader(file, engineCatalogMaxFileBytes+1))
	if err != nil {
		return absent, nil, legacyImportRefuse("filesystem.read", err)
	}
	if len(data) > engineCatalogMaxFileBytes {
		return absent, nil, legacyImportRefuse("filesystem.size", errors.New("file too large"))
	}
	if int64(len(data)) != opened.Size() {
		return absent, nil, legacyImportRefuse("filesystem.partial_read", errors.New("file length changed during read"))
	}
	return LegacyImportFileState{Kind: "file", SHA256: legacyString(legacyDigest(data)), Mode: int(info.Mode().Perm())}, data, nil
}
func legacyReadJournal(home string) (*LegacyImportJournal, error) {
	root := legacyTransactionRoot(home)
	if err := legacySafeDirectory(root, true); err != nil {
		return nil, err
	}
	state, data, err := legacySnapshot(filepath.Join(root, "journal.json"))
	if err != nil {
		return nil, err
	}
	if state.Kind == "absent" {
		return nil, nil
	}
	if state.Kind != "file" {
		return nil, legacyImportRefuse("journal.file", errors.New("journal must be regular"))
	}
	journal, err := ParseLegacyImportJournal(data)
	if err != nil {
		return nil, err
	}
	return &journal, nil
}
func legacyWriteJournal(root string, journal LegacyImportJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return legacyImportRefuse("journal.encode", err)
	}
	if _, err := ParseLegacyImportJournal(data); err != nil {
		return err
	}
	return legacyAtomicFile(filepath.Join(root, "journal.json"), data, 0o600)
}
func legacyAtomicFile(path string, data []byte, mode int) error {
	directory := filepath.Dir(path)
	if err := legacySafeDirectory(directory, false); err != nil {
		return err
	}
	file, err := legacyCreateTemp(directory, ".import-")
	if err != nil {
		return legacyImportRefuse("filesystem.temp", err)
	}
	temporary := file.Name()
	defer func() { _ = legacyClose(file); _ = legacyRemove(temporary) }()
	if err := legacyChmod(file, os.FileMode(mode)); err != nil {
		return legacyImportRefuse("filesystem.chmod", err)
	}
	if _, err := legacyWrite(file, data); err != nil {
		return legacyImportRefuse("filesystem.write", err)
	}
	if err := legacySync(file); err != nil {
		return legacyImportRefuse("filesystem.sync", err)
	}
	if err := legacyClose(file); err != nil {
		return legacyImportRefuse("filesystem.close", err)
	}
	if err := legacyRename(temporary, path); err != nil {
		return legacyImportRefuse("filesystem.replace", err)
	}
	return legacySyncDirectory(directory)
}
func legacySyncDirectory(path string) error {
	directory, err := legacyOpenDirectory(path)
	if err != nil {
		return legacyImportRefuse("filesystem.sync_open", err)
	}
	defer func() { _ = directory.Close() }()
	if err := legacySync(directory); err != nil {
		return legacyImportRefuse("filesystem.directory_sync", err)
	}
	return nil
}
func legacyRecover(options LegacyImportOptions, journal LegacyImportJournal, action string) error {
	// Admission is revalidated before recovery, every replacement and commit.
	// If it changes after a partial apply, unwind only digest-verified importer
	// targets. The rollback does not acquire, stop or relabel the new process.
	if err := legacyRecoveryAdmission(options, journal); err != nil {
		return err
	}
	journal.Phase = "applying"
	if action == "rollback" {
		journal.Phase = "rolling-back"
	}
	var admissionErr error
	admission := func() error {
		admissionErr = legacyRecoveryAdmission(options, journal)
		return admissionErr
	}
	err := legacyReplaceTargets(options, journal, action, admission)
	if admissionErr != nil {
		return errors.Join(err, legacyReplaceTargets(options, journal, "rollback", func() error { return nil }))
	}
	return err
}

func legacyRecoveryAdmission(options LegacyImportOptions, journal LegacyImportJournal) error {
	plan, err := legacyPlan(options, &journal)
	if err != nil {
		return err
	}
	if plan.report.SourceSHA256 != journal.SourceSHA256 {
		return legacyImportRefuse("recovery.source", errors.New("source changed"))
	}
	return nil
}

func legacyReplaceTargets(options LegacyImportOptions, journal LegacyImportJournal, action string, admission func() error) error {
	root := legacyTransactionRoot(options.HomeDir)
	desired, current, err := legacyRecoveryPreflight(options, journal, action)
	if err != nil {
		return err
	}
	journal.Phase = "applying"
	if action == "rollback" {
		journal.Phase = "rolling-back"
	}
	if err := legacyWriteJournal(root, journal); err != nil {
		return err
	}
	for index, target := range journal.Targets {
		path, _ := legacyTargetPath(options.HomeDir, target.TargetID)
		want := target.After
		if action == "rollback" {
			want = target.Before
		}
		if !legacyStatesEqual(current[target.TargetID], want) {
			// Recheck immediately before the individual atomic replacement as well.
			live, _, err := legacySnapshot(path)
			if err != nil {
				return err
			}
			if !legacyStatesEqual(live, current[target.TargetID]) {
				return legacyImportRefuse("recovery.target_race", errors.New("target changed after preflight"))
			}
			if err := admission(); err != nil {
				return err
			}
			if want.Kind == "absent" {
				if err := legacyRemove(path); err != nil {
					return legacyImportRefuse("recovery.remove", err)
				}
				if err := legacySyncDirectory(filepath.Dir(path)); err != nil {
					return err
				}
			} else if want.Kind == "symlink" {
				temporary := path + ".import-link"
				if err := legacySymlink(*want.LinkTarget, temporary); err != nil {
					return legacyImportRefuse("recovery.symlink", err)
				}
				if err := legacyRename(temporary, path); err != nil {
					_ = legacyRemove(temporary)
					return legacyImportRefuse("recovery.link_replace", err)
				}
				if err := legacySyncDirectory(filepath.Dir(path)); err != nil {
					return err
				}
			} else if err := legacyAtomicFile(path, desired[target.TargetID], want.Mode); err != nil {
				return err
			}
		}
		if err := legacyImportCheckpoint(fmt.Sprintf("after-replacement-%d", index+1)); err != nil {
			return err
		}
	}
	if err := admission(); err != nil {
		return err
	}
	if action == "rollback" {
		// Persist the original journal until all restores are durable, then retire
		// just that journal. Private backups stay available as operator evidence.
		if err := legacyRemove(filepath.Join(root, "journal.json")); err != nil {
			return legacyImportRefuse("recovery.journal_remove", err)
		}
		return legacySyncDirectory(root)
	}
	journal.Phase = "committed"
	return legacyWriteJournal(root, journal)
}

// ReadLegacyPiLifecycleEvidence is a compatibility read, never retirement.
func ReadLegacyPiLifecycleEvidence(root string) ([]PiTerminalEvidenceSummary, error) {
	return legacyReadEvidence(root)
}

func legacyRecoveryPreflight(options LegacyImportOptions, journal LegacyImportJournal, action string) (map[string][]byte, map[string]LegacyImportFileState, error) {
	root := legacyTransactionRoot(options.HomeDir)
	desired := map[string][]byte{}
	current := map[string]LegacyImportFileState{}
	// Validate all artifacts and targets first. A later bad backup cannot cause
	// an earlier target to be replaced and leave a partially guessed rollback.
	for _, target := range journal.Targets {
		path, err := legacyTargetPath(options.HomeDir, target.TargetID)
		if err != nil {
			return nil, nil, err
		}
		live, _, err := legacySnapshot(path)
		if err != nil {
			return nil, nil, err
		}
		current[target.TargetID] = live

		if target.Before.Kind != "absent" {
			state, data, err := legacySnapshot(filepath.Join(root, *target.BackupRef))
			if err != nil {
				return nil, nil, err
			}
			if state.Kind != "file" || *state.SHA256 != *target.BackupSHA256 {
				return nil, nil, legacyImportRefuse("recovery.backup", errors.New("missing or corrupt backup"))
			}
			if action == "rollback" {
				desired[target.TargetID] = data
			}
		}
		state, data, err := legacySnapshot(filepath.Join(root, journal.TransactionID+"-"+target.TargetID+"-after"))
		if err != nil {
			return nil, nil, err
		}
		if state.Kind != "file" || target.After.Kind != "file" || *state.SHA256 != *target.After.SHA256 {
			return nil, nil, legacyImportRefuse("recovery.staging", errors.New("missing or corrupt staging"))
		}
		if action != "rollback" {
			desired[target.TargetID] = data
		}
	}
	if err := ValidateLegacyImportObservedTargets(journal, current); err != nil {
		return nil, nil, err
	}
	return desired, current, nil
}

// RefuseActiveLegacyEngineState is a read-only compatibility admission gate.
// It never takes ownership of the legacy namespace or intervenes in a process.
func RefuseActiveLegacyEngineState(cache string) error {
	if cache == "" {
		var err error
		cache, err = legacyUserCacheDir()
		if err != nil {
			return legacyImportRefuse("legacy_state.cache_unknown", err)
		}
	}
	for _, name := range []string{"pi-runtimes", "pi-rv"} {
		root := filepath.Join(cache, "agents-infra", name)
		if err := legacySafeDirectory(root, true); err != nil {
			return err
		}
		entries, err := legacyReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return legacyImportRefuse("legacy_state.read_unknown", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				return legacyImportRefuse("legacy_state.shape_unknown", errors.New("unclassifiable legacy engine state"))
			}
			if err := legacyScanColdRuntime(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
