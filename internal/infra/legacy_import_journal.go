package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

const LegacyImportJournalFormat = "manager-legacy-import-journal/v1"
const legacyJournalLimit = 262144

var legacyTransactionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var legacyTargetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var legacyDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type LegacyImportError struct {
	Site string
	Err  error
}

func (e *LegacyImportError) Error() string {
	message := "legacy_import_refused: " + e.Site
	if e.Site == "profiles.credential_flag" {
		message += ": " + e.Err.Error()
	}
	return message
}
func (e *LegacyImportError) Unwrap() error { return e.Err }
func legacyImportRefuse(site string, err error) error {
	return &LegacyImportError{Site: site, Err: err}
}

type LegacyImportFileState struct {
	Kind       string  `json:"kind"`
	SHA256     *string `json:"sha256"`
	Mode       int     `json:"mode"`
	LinkTarget *string `json:"linkTarget"`
}
type LegacyImportTarget struct {
	TargetID     string                `json:"targetId"`
	Before       LegacyImportFileState `json:"before"`
	After        LegacyImportFileState `json:"after"`
	BackupRef    *string               `json:"backupRef"`
	BackupSHA256 *string               `json:"backupSha256"`
}
type LegacyImportJournal struct {
	Format        string               `json:"format"`
	TransactionID string               `json:"transactionId"`
	SourceSHA256  string               `json:"sourceSha256"`
	Phase         string               `json:"phase"`
	Targets       []LegacyImportTarget `json:"targets"`
}

func legacyDigest(data []byte) string   { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func legacyString(value string) *string { return &value }

// ParseLegacyImportJournal enforces the frozen grammar before any filesystem use.
func ParseLegacyImportJournal(data []byte) (LegacyImportJournal, error) {
	var journal LegacyImportJournal
	if len(data) > legacyJournalLimit {
		return journal, legacyImportRefuse("journal.size", errors.New("oversize"))
	}
	if !utf8.Valid(data) {
		return journal, legacyImportRefuse("journal.utf8", errors.New("invalid UTF-8"))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := legacyJSONValue(decoder); err != nil {
		return journal, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return journal, legacyImportRefuse("journal.trailing", errors.New("trailing bytes"))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return journal, legacyImportRefuse("journal.decode", err)
	}
	if err := legacyRequired(raw, "format", "transactionId", "sourceSha256", "phase", "targets"); err != nil {
		return journal, err
	}
	if err := legacyClosedJSONKeys(data, reflect.TypeOf(journal)); err != nil {
		return journal, err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return journal, legacyImportRefuse("journal.decode", err)
	}
	if journal.Format != LegacyImportJournalFormat {
		return journal, legacyImportRefuse("journal.format", errors.New("format"))
	}
	if !legacyTransactionPattern.MatchString(journal.TransactionID) {
		return journal, legacyImportRefuse("journal.transaction", errors.New("transaction"))
	}
	if !legacyDigestPattern.MatchString(journal.SourceSHA256) {
		return journal, legacyImportRefuse("journal.source", errors.New("digest"))
	}
	if journal.Phase != "prepared" && journal.Phase != "applying" && journal.Phase != "committed" && journal.Phase != "rolling-back" {
		return journal, legacyImportRefuse("journal.phase", errors.New("phase"))
	}
	if len(journal.Targets) < 1 || len(journal.Targets) > 64 {
		return journal, legacyImportRefuse("journal.targets", errors.New("target count"))
	}
	var targets []map[string]json.RawMessage
	if err := json.Unmarshal(raw["targets"], &targets); err != nil {
		return journal, legacyImportRefuse("journal.target_decode", err)
	}
	seen := map[string]bool{}
	for index, target := range journal.Targets {
		if err := legacyRequired(targets[index], "targetId", "before", "after", "backupRef", "backupSha256"); err != nil {
			return journal, err
		}
		if !legacyTargetPattern.MatchString(target.TargetID) {
			return journal, legacyImportRefuse("journal.target_id", errors.New("target id"))
		}
		if seen[target.TargetID] {
			return journal, legacyImportRefuse("journal.duplicate_target", errors.New("duplicate target"))
		}
		seen[target.TargetID] = true
		for _, name := range []string{"before", "after"} {
			var state map[string]json.RawMessage
			if err := json.Unmarshal(targets[index][name], &state); err != nil {
				return journal, legacyImportRefuse("journal.state_decode", err)
			}
			if err := legacyRequired(state, "kind", "sha256", "mode", "linkTarget"); err != nil {
				return journal, err
			}
			if bytes.Equal(bytes.TrimSpace(state["mode"]), []byte("null")) {
				return journal, legacyImportRefuse("journal.null_mode", errors.New("mode must be integer"))
			}
		}
		if err := legacyValidateState(target.Before); err != nil {
			return journal, err
		}
		if err := legacyValidateState(target.After); err != nil {
			return journal, err
		}
		if target.Before.Kind == "absent" {
			if target.BackupRef != nil || target.BackupSHA256 != nil {
				return journal, legacyImportRefuse("journal.absent_backup", errors.New("absent backup"))
			}
		} else {
			if target.BackupRef == nil || !legacyTargetPattern.MatchString(*target.BackupRef) {
				return journal, legacyImportRefuse("journal.backup_ref", errors.New("backup ref"))
			}
			if target.BackupSHA256 == nil || *target.BackupSHA256 != *target.Before.SHA256 {
				return journal, legacyImportRefuse("journal.backup_digest", errors.New("backup digest"))
			}
		}
	}
	return journal, nil
}
func legacyRequired(raw map[string]json.RawMessage, names ...string) error {
	if err := legacyKnownMembers(raw, names...); err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := raw[name]; !ok {
			return legacyImportRefuse("journal.required", fmt.Errorf("missing %s", name))
		}
	}
	return nil
}

// Go's struct decoder matches field names case-insensitively. Check the raw
// keys against the canonical tags before permitting that decoder to run.
func legacyKnownMembers(raw map[string]json.RawMessage, names ...string) error {
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
	}
	for name := range raw {
		if !known[name] {
			return legacyImportRefuse("journal.decode", errors.New("noncanonical member"))
		}
	}
	return nil
}

// The destination types, rather than a second schema, supply the closed JSON
// key sets. Maps remain open only where the production type declares a map.
func legacyDecodeClosedJSON(data []byte, destination any) error {
	if !utf8.Valid(data) {
		return legacyImportRefuse("journal.utf8", errors.New("invalid UTF-8"))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := legacyJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return legacyImportRefuse("journal.trailing", errors.New("trailing bytes"))
	}
	if err := legacyClosedJSONKeys(data, reflect.TypeOf(destination).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func legacyClosedJSONKeys(data []byte, kind reflect.Type) error {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	// Scalar custom JSON decoders (notably time.Time) own their wire format.
	if reflect.PointerTo(kind).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	switch kind.Kind() {
	case reflect.Struct:
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		var names []string
		for i := 0; i < kind.NumField(); i++ {
			names = append(names, strings.Split(kind.Field(i).Tag.Get("json"), ",")[0])
		}
		if err := legacyKnownMembers(raw, names...); err != nil {
			return err
		}
		for i, name := range names {
			if value, present := raw[name]; present {
				if err := legacyClosedJSONKeys(value, kind.Field(i).Type); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := legacyClosedJSONKeys(value, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func legacyValidateState(state LegacyImportFileState) error {
	if state.Mode < 0 || state.Mode > 511 {
		return legacyImportRefuse("journal.mode", errors.New("mode"))
	}
	if state.Kind == "absent" {
		if state.SHA256 != nil || state.Mode != 0 || state.LinkTarget != nil {
			return legacyImportRefuse("journal.absent_state", errors.New("absent state"))
		}
		return nil
	}
	if state.Kind != "file" && state.Kind != "symlink" {
		return legacyImportRefuse("journal.kind", errors.New("kind"))
	}
	if state.SHA256 == nil || !legacyDigestPattern.MatchString(*state.SHA256) {
		return legacyImportRefuse("journal.state_digest", errors.New("digest"))
	}
	if state.Kind == "file" {
		if state.LinkTarget != nil {
			return legacyImportRefuse("journal.file_link", errors.New("file link"))
		}
	} else {
		if state.Mode != 0 || state.LinkTarget == nil || len(*state.LinkTarget) < 1 || len(*state.LinkTarget) > 4096 {
			return legacyImportRefuse("journal.symlink", errors.New("symlink"))
		}
		if *state.SHA256 != legacyDigest([]byte(*state.LinkTarget)) {
			return legacyImportRefuse("journal.link_digest", errors.New("link digest"))
		}
	}
	return nil
}

// legacyJSONValue detects duplicate object members recursively, before decoding.
func legacyJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return legacyImportRefuse("journal.json", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err = decoder.Token()
			if err != nil {
				return legacyImportRefuse("journal.json_member", err)
			}
			name, ok := token.(string)
			if !ok || seen[name] {
				return legacyImportRefuse("journal.duplicate_member", errors.New("duplicate member"))
			}
			seen[name] = true
			if err := legacyJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := legacyJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return legacyImportRefuse("journal.delimiter", errors.New("unexpected delimiter"))
	}
	if _, err := decoder.Token(); err != nil {
		return legacyImportRefuse("journal.json_end", err)
	}
	return nil
}

// ValidateLegacyImportObservedTargets is the journal's recovery semantic gate.
// The production recovery caller supplies snapshots resolved by its allow-list.
func ValidateLegacyImportObservedTargets(journal LegacyImportJournal, observed map[string]LegacyImportFileState) error {
	for _, target := range journal.Targets {
		live, present := observed[target.TargetID]
		if !present {
			return legacyImportRefuse("recovery.observation_missing", errors.New("missing target observation"))
		}
		if journal.Phase == "committed" {
			if !legacyStateEqual(live, target.After) {
				return legacyImportRefuse("recovery.committed_drift", errors.New("committed target differs"))
			}
		} else if !legacyStateEqual(live, target.Before) && !legacyStateEqual(live, target.After) {
			return legacyImportRefuse("recovery.target_drift", errors.New("target differs from both states"))
		}
	}
	return nil
}
func legacyStateEqual(a, b LegacyImportFileState) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
