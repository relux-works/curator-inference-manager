package infra

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const engineWeightsPrefix = "gguf-sha256:"

var engineWeightsIDPattern = regexp.MustCompile(`^gguf-sha256:[0-9a-f]{64}$`)

// EngineWeightsObservation belongs to one running instance, never to a pathname cache.
type EngineWeightsObservation struct {
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
}

// engineWeightsFailureRecord preserves a startup refusal without treating a
// persisted diagnostic as an in-memory error object.
type engineWeightsFailureRecord struct {
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
	Message string         `json:"message"`
}

type EngineWeightsStatus struct {
	ID           string     `json:"id,omitempty"`
	Verification string     `json:"verification"`
	ObservedID   string     `json:"observed_id,omitempty"`
	ObservedAt   *time.Time `json:"observed_at,omitempty"`
}

func validateEngineWeightsID(id string) error {
	if strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return engineCatalogMalformed("entry.weights.id", errors.New("weights id cannot contain whitespace"))
	}
	if id != "" && !strings.HasPrefix(id, engineWeightsPrefix) {
		return &EngineCatalogError{State: EngineCatalogUnsupported, Site: "entry.weights.id.unsupported", Err: errors.New("unsupported weights id prefix")}
	}
	if !engineWeightsIDPattern.MatchString(id) {
		return engineCatalogMalformed("entry.weights.id", errors.New("weights id must match ^gguf-sha256:[0-9a-f]{64}$ exactly"))
	}
	return nil
}

func validateEngineWeightsTable(raw any) error {
	table, ok := raw.(map[string]any)
	if !ok {
		return engineCatalogMalformed("catalog.weights", errors.New("weights must be a table"))
	}
	for key := range table {
		if key != "id" {
			return engineCatalogMalformed("catalog.weights.keys", fmt.Errorf("unknown weights member %q", key))
		}
	}
	id, ok := table["id"].(string)
	if !ok {
		return engineCatalogMalformed("entry.weights.id", errors.New("weights.id is required and must be a string"))
	}
	return validateEngineWeightsID(id)
}

func engineWeightsScope(entry EngineCatalogEntry) error {
	if entry.Engine != "llama-cpp" || entry.Mode != "local" || hasCatalogRemoteFields(entry) || entry.Model == "" {
		return &EngineCatalogError{State: EngineCatalogUnsupported, Site: "entry.weights.scope", Err: errors.New("weights require a local llama-cpp entry with one unsplit model file")}
	}
	return nil
}

func engineWeightsModelPath(entry EngineCatalogEntry, config string) string {
	if filepath.IsAbs(entry.Model) {
		return entry.Model
	}
	return filepath.Join(filepath.Dir(config), entry.Model)
}

// engineWeightsFile keeps device/inode (via SameFile), size, mtime and ctime from the
// descriptor used to hash. Rechecking the path also detects atomic replacement.
type engineWeightsFile struct {
	path string
	info os.FileInfo
}

func (file *engineWeightsFile) unchanged() error {
	if file == nil {
		return nil
	}
	current, err := os.Stat(file.path)
	if err != nil || !sameEngineWeightsFile(file.info, current) {
		return sharedRuntimeError("engine_weights_file_changed", errors.New("model file identity, size, mtime or ctime changed after hashing"))
	}
	return nil
}
func sameEngineWeightsFile(a, b os.FileInfo) bool {
	if b == nil || !b.Mode().IsRegular() || !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	ka, supported := engineWeightsKey("", a)
	kb, _ := engineWeightsKey("", b)
	return !supported || ka == kb
}

func hashEngineWeights(path string) (*EngineWeightsObservation, *engineWeightsFile, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, sharedRuntimeError("engine_weights_file_missing", errors.New("model file is absent"))
	}
	if err != nil {
		return nil, nil, sharedRuntimeError("engine_weights_read_failed", errors.New("model file cannot be inspected"))
	}
	if !info.Mode().IsRegular() {
		return nil, nil, sharedRuntimeError("engine_weights_not_regular", errors.New("model must be one regular file"))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, sharedRuntimeError("engine_weights_read_failed", errors.New("model file cannot be opened"))
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !sameEngineWeightsFile(info, opened) {
		return nil, nil, sharedRuntimeError("engine_weights_file_changed", errors.New("model changed while opening"))
	}
	hash := sha256.New()
	// Decode the bounded metadata through the hash: validation and digest see
	// exactly the same bytes from the same descriptor, with no reopen or rewind.
	metadata, err := decodeEngineGGUF(io.LimitReader(io.TeeReader(file, hash), 32<<20))
	if err != nil {
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			return nil, nil, sharedRuntimeError("engine_weights_read_failed", errors.New("model metadata cannot be read"))
		}
		return nil, nil, sharedRuntimeError("engine_weights_not_gguf", errors.New("model must have valid GGUF v2/v3 header and metadata"))
	}
	if _, split := metadata.values["split.no"]; split || metadata.number("split.count") > 1 {
		return nil, nil, sharedRuntimeError("engine_weights_split_unsupported", errors.New("split GGUF metadata is unsupported"))
	}
	if _, err := io.Copy(hash, file); err != nil {
		return nil, nil, sharedRuntimeError("engine_weights_read_failed", errors.New("model bytes cannot be read"))
	}
	identity := &engineWeightsFile{path: path, info: opened}
	after, err := file.Stat()
	if err != nil || !sameEngineWeightsFile(opened, after) {
		return nil, nil, sharedRuntimeError("engine_weights_file_changed", errors.New("model changed while hashing"))
	}
	if err := identity.unchanged(); err != nil {
		return nil, nil, err
	}
	return &EngineWeightsObservation{ID: fmt.Sprintf("%s%x", engineWeightsPrefix, hash.Sum(nil)), ObservedAt: time.Now().UTC()}, identity, nil
}

// ComputeEngineWeights is read-only and resolves relative models beside config.
func ComputeEngineWeights(config, name string) (*EngineWeightsObservation, error) {
	entry, state, err := ReadEngineCatalogEntry(config, name)
	if err != nil {
		return nil, err
	}
	if state != EngineCatalogPresent {
		return nil, engineCatalogMalformed("weights.entry", errors.New("engine entry is absent"))
	}
	if err := engineWeightsScope(entry); err != nil {
		return nil, err
	}
	observed, _, err := hashEngineWeights(engineWeightsModelPath(entry, config))
	return observed, err
}

func engineWeightsMismatch(expected, recorded, actual string) error {
	return &SharedRuntimeError{Code: "engine_weights_mismatch", Details: map[string]any{"expected_weights_id": expected, "recorded_weights_id": recorded, "actual_weights_id": actual}, Err: errors.New("weights identities disagree")}
}

func declaredEngineWeights(entry *EngineCatalogEntry) EngineWeightsStatus {
	if entry == nil || entry.Weights == nil {
		return EngineWeightsStatus{Verification: "unpinned"}
	}
	return EngineWeightsStatus{ID: entry.Weights.ID, Verification: "unknown"}
}

func observedEngineWeights(entry *EngineCatalogEntry, observed *EngineWeightsObservation) EngineWeightsStatus {
	status := declaredEngineWeights(entry)
	if status.ID == "" || observed == nil {
		return status
	}
	status.Verification = "mismatch"
	if observed.ID == status.ID {
		status.Verification = "verified"
		status.ObservedID, status.ObservedAt = observed.ID, &observed.ObservedAt
	}
	return status
}

func verifyCatalogWeights(resolved sharedResolvedProfile) (*EngineWeightsObservation, *engineWeightsFile, error) {
	entry := resolved.CatalogObservation
	if entry == nil || entry.Weights == nil {
		return nil, nil, nil
	}
	root := engineWeightsCacheRoot(resolved)
	if resolved.NoWeightsCache {
		root = ""
	}
	observed, file, err := cachedEngineWeights(engineWeightsModelPath(*entry, resolved.Profile.Source), root)
	if err != nil {
		return nil, nil, err
	}
	if observed.ID != entry.Weights.ID {
		return observed, file, engineWeightsMismatch(resolved.ExpectedWeightsID, entry.Weights.ID, observed.ID)
	}
	return observed, file, nil
}

func validateExpectedEngineWeights(resolved sharedResolvedProfile, observed *EngineWeightsObservation) error {
	pin := resolved.ExpectedWeightsID
	if pin != "" {
		if err := validateEngineWeightsID(pin); err != nil {
			return err
		}
		if resolved.CatalogObservation == nil || resolved.CatalogObservation.Weights == nil {
			return sharedRuntimeError("engine_weights_unpinned", errors.New("an expected weights id cannot certify an unpinned entry"))
		}
	}
	status := declaredEngineWeights(resolved.CatalogObservation)
	if status.ID == "" {
		return nil
	}
	if observed == nil {
		return sharedRuntimeError("engine_weights_unverified", errors.New("running instance has no verified weights identity"))
	}
	if observed.ID != status.ID || (pin != "" && pin != observed.ID) {
		return engineWeightsMismatch(pin, status.ID, observed.ID)
	}
	return nil
}

func engineCatalogErrorState(err error) EngineCatalogState {
	var refusal *EngineCatalogError
	if errors.As(err, &refusal) {
		return refusal.State
	}
	return EngineCatalogMalformed
}
