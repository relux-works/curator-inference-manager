package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	EngineCatalogSchemaVersion   = 1
	EngineCatalogFileName        = "engines.toml"
	engineCatalogMaxFileBytes    = 1 << 20
	engineCatalogDefaultHost     = "127.0.0.1"
	engineCatalogPortBase        = 49152
	engineCatalogPortCount       = 16384
	engineCatalogStartupDefault  = 600
	engineCatalogShutdownDefault = 30
)

type EngineCatalogState string

const (
	EngineCatalogPresent    EngineCatalogState = "present"
	EngineCatalogAbsent     EngineCatalogState = "absent"
	EngineCatalogMalformed  EngineCatalogState = "malformed"
	EngineCatalogReadFailed EngineCatalogState = "read_failed"
	EngineCatalogConflict   EngineCatalogState = "conflict"
)

// EngineCatalogError is the typed refusal returned by the catalog and
// compatibility resolver. Absent is a value, not an error, so callers can
// distinguish it from every failed read or parse.
type EngineCatalogError struct {
	State EngineCatalogState
	Site  string
	Err   error
}

func (e *EngineCatalogError) Error() string {
	code := "engine_catalog_" + string(e.State)
	payload := struct {
		Code    string `json:"code"`
		State   string `json:"state"`
		Site    string `json:"site,omitempty"`
		Message string `json:"message,omitempty"`
	}{Code: code, State: string(e.State), Site: e.Site}
	if e.Err != nil {
		payload.Message = e.Err.Error()
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return code
	}
	return string(data)
}

func (e *EngineCatalogError) Unwrap() error { return e.Err }

func EngineCatalogExitCode(err error) (int, bool) {
	var refusal *EngineCatalogError
	if errors.As(err, &refusal) {
		return 1, true
	}
	return 0, false
}

type EngineCatalogDocument struct {
	SchemaVersion int                           `toml:"schema_version"`
	Engines       map[string]EngineCatalogEntry `toml:"engines"`
}

// MarshalEngineCatalog encodes a validated schema v1 document, including the
// legacy knob-name/value translation required by the shared catalog reader.
func MarshalEngineCatalog(document EngineCatalogDocument) ([]byte, error) {
	if document.SchemaVersion != EngineCatalogSchemaVersion || document.Engines == nil {
		return nil, engineCatalogMalformed("catalog.schema", errors.New("schema_version 1 and an engines table are required"))
	}
	for name, entry := range document.Engines {
		if err := validateEngineCatalogEntry(name, entry); err != nil {
			return nil, err
		}
	}
	return marshalEngineCatalogDocument(document)
}

// EngineCatalogEntry is schema v1's closed transcription of a legacy
// model-harness profile. Runtime-only broker settings are manager defaults;
// they are not writable catalog fields.
type EngineCatalogEntry struct {
	Engine             string                           `toml:"engine,omitempty"`
	Model              string                           `toml:"model,omitempty"`
	ServedModelID      string                           `toml:"served_model_id,omitempty"`
	Executable         string                           `toml:"executable,omitempty"`
	Argv               []string                         `toml:"argv,omitempty"`
	Knobs              *EngineCatalogKnobs              `toml:"knobs,omitempty"`
	Mode               string                           `toml:"mode"`
	SSHExecutable      string                           `toml:"ssh_executable,omitempty"`
	SSHTarget          string                           `toml:"ssh_target,omitempty"`
	RemoteExecutable   string                           `toml:"remote_executable,omitempty"`
	RemoteConfig       string                           `toml:"remote_config,omitempty"`
	RemoteProfile      string                           `toml:"remote_profile,omitempty"`
	RemoteHost         string                           `toml:"remote_host,omitempty"`
	RemotePort         int                              `toml:"remote_port,omitempty"`
	Stress             *EngineCatalogStressPolicy       `toml:"stress,omitempty"`
	Supervision        *EngineCatalogSupervisionPolicy  `toml:"supervision,omitempty"`
	PinnedDistribution *EngineCatalogPinnedDistribution `toml:"pinned_distribution,omitempty"`
}

type sharedResolvedProfile struct {
	Project           string
	HomeDir           string
	ProfileName       string
	EngineIdentity    SharedRuntimeEngineIdentity
	Profile           PiProfile
	Sharing           PiRuntimeSharing
	RuntimeKey        string
	ProfileDigest     string
	Paths             SharedRuntimePaths
	CatalogEntry      bool
	CatalogMode       string
	NotRunnableCode   string
	NotRunnableReason string
}

type EngineCatalogKnobs struct {
	// Legacy values stay strings in memory; v1 writes numeric values as TOML
	// integers under keys accepted by the skill-agents-management reader.
	KVContextTokens     string `toml:"-"`
	PrefillChunkTokens  string `toml:"-"`
	KVContextSize       any    `toml:"kv_context_size,omitempty"`
	PrefillChunkSize    any    `toml:"prefill_chunk_size,omitempty"`
	ReasoningEffort     string `toml:"reasoning_effort,omitempty"`
	SpeculativeDecoding string `toml:"speculative_decoding,omitempty"`
}

type EngineCatalogStressPolicy struct {
	PromptTokens               int `toml:"prompt_tokens"`
	MaxOutputTokens            int `toml:"max_output_tokens"`
	StartupTimeoutSeconds      int `toml:"startup_timeout_seconds"`
	RequestTimeoutSeconds      int `toml:"request_timeout_seconds"`
	SampleIntervalMilliseconds int `toml:"sample_interval_milliseconds"`
}

type EngineCatalogSupervisionPolicy struct {
	FatalOutputSubstrings    []string `toml:"fatal_output_substrings"`
	RestartOnFailure         bool     `toml:"restart_on_failure"`
	MaxRestarts              int      `toml:"max_restarts"`
	RestartWindowSeconds     int      `toml:"restart_window_seconds"`
	RestartDelayMilliseconds int      `toml:"restart_delay_milliseconds"`
}

type EngineCatalogPinnedDistribution struct {
	Package      string `toml:"package"`
	SitePackages string `toml:"site_packages"`
	Commit       string `toml:"commit"`
}

func marshalEngineCatalogDocument(document EngineCatalogDocument) ([]byte, error) {
	prepared := EngineCatalogDocument{SchemaVersion: document.SchemaVersion, Engines: make(map[string]EngineCatalogEntry, len(document.Engines))}
	for name, entry := range document.Engines {
		encoded, err := engineCatalogEntryForEncoding(entry)
		if err != nil {
			return nil, err
		}
		prepared.Engines[name] = encoded
	}
	return toml.Marshal(prepared)
}

func engineCatalogEntryForEncoding(entry EngineCatalogEntry) (EngineCatalogEntry, error) {
	if entry.Knobs == nil {
		return entry, nil
	}
	knobs := *entry.Knobs
	knobs.KVContextSize = engineCatalogKnobSizeWireValue(knobs.KVContextTokens)
	knobs.PrefillChunkSize = engineCatalogKnobSizeWireValue(knobs.PrefillChunkTokens)
	knobs.KVContextTokens = ""
	knobs.PrefillChunkTokens = ""
	entry.Knobs = &knobs
	return entry, nil
}

func marshalEngineCatalogEntry(entry EngineCatalogEntry) ([]byte, error) {
	prepared, err := engineCatalogEntryForEncoding(entry)
	if err != nil {
		return nil, err
	}
	return toml.Marshal(prepared)
}

func engineCatalogKnobSizeWireValue(value string) any {
	if value == "" {
		return nil
	}
	if value == "unbounded" {
		return value
	}
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return number
	}
	// Preserve invalid values in test encodings so the reader can prove refusal.
	return value
}

func normalizeEngineCatalogEntry(entry EngineCatalogEntry) (EngineCatalogEntry, error) {
	if entry.Knobs == nil {
		return entry, nil
	}
	knobs := *entry.Knobs
	var err error
	if knobs.KVContextSize != nil {
		knobs.KVContextTokens, err = engineCatalogKnobSizeLegacyValue(knobs.KVContextSize, true)
		if err != nil {
			return EngineCatalogEntry{}, fmt.Errorf("kv_context_size: %w", err)
		}
	}
	if knobs.PrefillChunkSize != nil {
		knobs.PrefillChunkTokens, err = engineCatalogKnobSizeLegacyValue(knobs.PrefillChunkSize, true)
		if err != nil {
			return EngineCatalogEntry{}, fmt.Errorf("prefill_chunk_size: %w", err)
		}
	}
	knobs.KVContextSize = nil
	knobs.PrefillChunkSize = nil
	entry.Knobs = &knobs
	return entry, nil
}

func engineCatalogKnobSizeLegacyValue(value any, allowUnbounded bool) (string, error) {
	switch typed := value.(type) {
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case int:
		return strconv.Itoa(typed), nil
	case string:
		if allowUnbounded && typed == "unbounded" {
			return typed, nil
		}
		return "", fmt.Errorf("must be an integer%s", engineCatalogUnboundedSuffix(allowUnbounded))
	default:
		return "", fmt.Errorf("must be an integer%s, got %T", engineCatalogUnboundedSuffix(allowUnbounded), value)
	}
}

func engineCatalogUnboundedSuffix(allowUnbounded bool) string {
	if allowUnbounded {
		return " or unbounded"
	}
	return ""
}

func engineCatalogPositiveInteger(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number > 0
}

var engineCatalogNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var engineCatalogServedModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
var engineCatalogPinnedPackagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var engineCatalogPinnedCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var engineCatalogCredentialKeyStems = [...]string{
	"accesskey", "apikey", "auth", "authorization", "bearer", "clientsecret", "cookie", "credential", "jwt",
	"pass", "passphrase", "passw", "privatekey", "privkey", "pwd", "secret", "sessionid", "sessionkey",
	"sessionsecret", "sessiontoken", "signingkey", "token",
}
var engineCatalogDocumentKeys = [...]string{"schema_version", "engines"}
var engineCatalogEntryKeys = [...]string{
	"engine", "model", "served_model_id", "executable", "argv", "knobs", "mode", "ssh_executable",
	"ssh_target", "remote_executable", "remote_config", "remote_profile", "remote_host", "remote_port",
	"stress", "supervision", "pinned_distribution",
}
var engineCatalogKnobKeys = [...]string{"kv_context_size", "prefill_chunk_size", "reasoning_effort", "speculative_decoding"}
var engineCatalogStressKeys = [...]string{
	"prompt_tokens", "max_output_tokens", "startup_timeout_seconds", "request_timeout_seconds", "sample_interval_milliseconds",
}
var engineCatalogSupervisionKeys = [...]string{
	"fatal_output_substrings", "restart_on_failure", "max_restarts", "restart_window_seconds", "restart_delay_milliseconds",
}
var engineCatalogPinnedDistributionKeys = [...]string{"package", "site_packages", "commit"}
var lstatEngineCatalogFile = os.Lstat
var openEngineCatalogFile = os.Open
var statEngineCatalogFile = func(file *os.File) (os.FileInfo, error) { return file.Stat() }
var readEngineCatalogFile = func(reader io.Reader) ([]byte, error) { return io.ReadAll(reader) }
var userEngineCatalogHomeDir = os.UserHomeDir

func engineCatalogMalformed(site string, err error) error {
	return &EngineCatalogError{State: EngineCatalogMalformed, Site: site, Err: err}
}

func engineCatalogReadFailed(site string, err error) error {
	return &EngineCatalogError{State: EngineCatalogReadFailed, Site: site, Err: err}
}

func engineCatalogConflict(site string, err error) error {
	return &EngineCatalogError{State: EngineCatalogConflict, Site: site, Err: err}
}

func validEngineCatalogName(name string) bool {
	if !engineCatalogNamePattern.MatchString(name) {
		return false
	}
	compact := strings.NewReplacer(".", "", "_", "", "-", "").Replace(strings.ToLower(name))
	if strings.Contains(compact, "token") && strings.HasSuffix(compact, "tokens") {
		return false
	}
	for _, stem := range engineCatalogCredentialKeyStems {
		switch stem {
		case "auth":
			if engineCatalogHasFieldToken(name, stem) || strings.HasSuffix(compact, stem) ||
				strings.HasPrefix(compact, stem) && engineCatalogContainsCredentialStem(strings.TrimPrefix(compact, stem)) {
				return false
			}
		case "pass":
			if engineCatalogHasFieldToken(name, stem) {
				return false
			}
		default:
			if strings.Contains(compact, stem) {
				return false
			}
		}
	}
	return true
}

func engineCatalogContainsCredentialStem(compact string) bool {
	for _, stem := range engineCatalogCredentialKeyStems {
		if stem == "auth" || stem == "pass" {
			continue
		}
		if strings.Contains(compact, stem) {
			return true
		}
	}
	return false
}

func engineCatalogASCIIAlphaNumeric(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9')
}

func engineCatalogHasFieldToken(value, expected string) bool {
	var token strings.Builder
	previous := byte(0)
	flush := func() bool {
		if token.Len() == 0 {
			return false
		}
		matches := token.String() == expected
		token.Reset()
		return matches
	}
	for index := 0; index < len(value); index++ {
		current := value[index]
		if !engineCatalogASCIIAlphaNumeric(current) {
			if flush() {
				return true
			}
			previous = 0
			continue
		}
		if current >= 'A' && current <= 'Z' && ((previous >= 'a' && previous <= 'z') || (previous >= '0' && previous <= '9')) {
			if flush() {
				return true
			}
		}
		if current >= 'A' && current <= 'Z' {
			current += 'a' - 'A'
		}
		token.WriteByte(current)
		previous = value[index]
	}
	return flush()
}

func validateEngineCatalogExactKeys(data []byte) error {
	var rawDocument map[string]any
	if err := toml.Unmarshal(data, &rawDocument); err != nil {
		return engineCatalogMalformed("catalog.decode", err)
	}
	if rawDocument == nil {
		return engineCatalogMalformed("catalog.decode", errors.New("catalog document must be a table"))
	}
	if err := engineCatalogRejectUnknownKeys(rawDocument, "document", engineCatalogDocumentKeys[:]...); err != nil {
		return err
	}
	rawEngines, ok := rawDocument["engines"].(map[string]any)
	if !ok {
		return engineCatalogMalformed("catalog.engines", errors.New("engines table is required"))
	}
	for name, rawEntry := range rawEngines {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return engineCatalogMalformed("catalog.engine", fmt.Errorf("entry %q must be a table", name))
		}
		if err := engineCatalogRejectUnknownKeys(entry, "entry "+name, engineCatalogEntryKeys[:]...); err != nil {
			return err
		}
		for _, nested := range []struct {
			name string
			keys []string
		}{
			{name: "knobs", keys: engineCatalogKnobKeys[:]},
			{name: "stress", keys: engineCatalogStressKeys[:]},
			{name: "supervision", keys: engineCatalogSupervisionKeys[:]},
			{name: "pinned_distribution", keys: engineCatalogPinnedDistributionKeys[:]},
		} {
			rawNested, present := entry[nested.name]
			if !present {
				continue
			}
			nestedTable, ok := rawNested.(map[string]any)
			if !ok {
				return engineCatalogMalformed("catalog."+nested.name, fmt.Errorf("%s must be a table", nested.name))
			}
			if err := engineCatalogRejectUnknownKeys(nestedTable, "entry "+name+"."+nested.name, nested.keys...); err != nil {
				return err
			}
		}
	}
	return nil
}

func engineCatalogRejectUnknownKeys(table map[string]any, location string, allowed ...string) error {
	accepted := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		accepted[key] = struct{}{}
	}
	for key := range table {
		if _, ok := accepted[key]; !ok {
			return engineCatalogMalformed("catalog.keys", fmt.Errorf("unknown key %q in %s", key, location))
		}
	}
	return nil
}

// ReadEngineCatalogEntry loads one entry. A missing file or missing name is
// absent; malformed bytes and unreadable paths are separate typed refusals.
func ReadEngineCatalogEntry(path, name string) (EngineCatalogEntry, EngineCatalogState, error) {
	info, err := lstatEngineCatalogFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return EngineCatalogEntry{}, EngineCatalogAbsent, nil
	}
	if err != nil {
		return EngineCatalogEntry{}, EngineCatalogReadFailed, engineCatalogReadFailed("catalog.read", err)
	}
	if !info.Mode().IsRegular() {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.file", errors.New("catalog must be a regular non-symlink file"))
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.permissions", errors.New("catalog must be readable by its owner and inaccessible to group and other users"))
	}
	if info.Size() > engineCatalogMaxFileBytes {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.size", errors.New("catalog exceeds the 1 MiB schema limit"))
	}
	file, err := openEngineCatalogFile(path)
	if err != nil {
		return EngineCatalogEntry{}, EngineCatalogReadFailed, engineCatalogReadFailed("catalog.read", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := statEngineCatalogFile(file)
	if err != nil {
		return EngineCatalogEntry{}, EngineCatalogReadFailed, engineCatalogReadFailed("catalog.stat", err)
	}
	if !os.SameFile(info, opened) {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.file", errors.New("catalog changed identity while opening"))
	}
	data, err := readEngineCatalogFile(io.LimitReader(file, engineCatalogMaxFileBytes+1))
	if err != nil {
		return EngineCatalogEntry{}, EngineCatalogReadFailed, engineCatalogReadFailed("catalog.read", err)
	}
	if len(data) > engineCatalogMaxFileBytes {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.size", errors.New("catalog exceeds the 1 MiB schema limit"))
	}

	if err := validateEngineCatalogExactKeys(data); err != nil {
		return EngineCatalogEntry{}, EngineCatalogMalformed, err
	}
	decoder := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	var document EngineCatalogDocument
	if err := decoder.Decode(&document); err != nil {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.decode", err)
	}
	if document.SchemaVersion != EngineCatalogSchemaVersion {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.schema_version", fmt.Errorf("schema_version must equal %d", EngineCatalogSchemaVersion))
	}
	if document.Engines == nil {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.engines", errors.New("engines table is required"))
	}
	for candidateName, candidateEntry := range document.Engines {
		if !validEngineCatalogName(candidateName) {
			return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.engine_name", errors.New("engine name is incompatible with the shared catalog reader"))
		}
		if candidateEntry.Engine == "" || candidateEntry.Engine != strings.TrimSpace(candidateEntry.Engine) {
			site := "catalog.engine"
			if candidateName == name {
				site = "entry.engine"
			}
			return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed(site, fmt.Errorf("engine kind for %q must be a non-empty trimmed string", candidateName))
		}
	}
	// A requested Pi profile name outside the catalog grammar cannot exist as
	// an engines.toml entry. Treat it as absent so the compatibility reader can
	// retain legacy profile names; malformed names stored in the catalog above
	// still refuse the whole document.
	if !validEngineCatalogName(name) {
		return EngineCatalogEntry{}, EngineCatalogAbsent, nil
	}
	entry, present := document.Engines[name]
	if !present {
		return EngineCatalogEntry{}, EngineCatalogAbsent, nil
	}
	entry, err = normalizeEngineCatalogEntry(entry)
	if err != nil {
		return EngineCatalogEntry{}, EngineCatalogMalformed, engineCatalogMalformed("catalog.knobs", err)
	}
	if err := validateEngineCatalogEntry(name, entry); err != nil {
		return EngineCatalogEntry{}, EngineCatalogMalformed, err
	}
	return entry, EngineCatalogPresent, nil
}

func validateEngineCatalogEntry(name string, entry EngineCatalogEntry) error {
	if !validEngineCatalogName(name) {
		return engineCatalogMalformed("entry.name", errors.New("engine name must be a safe shared-catalog identifier"))
	}
	switch entry.Mode {
	case "local":
		if !knownCatalogEngine(entry.Engine) {
			return engineCatalogMalformed("entry.engine", fmt.Errorf("unknown engine kind %q", entry.Engine))
		}
		if strings.IndexByte(entry.Model, 0) >= 0 {
			return engineCatalogMalformed("entry.model", errors.New("must be NUL-free"))
		}
		if !filepath.IsAbs(entry.Executable) || strings.IndexByte(entry.Executable, 0) >= 0 {
			return engineCatalogMalformed("entry.executable", errors.New("must be an absolute NUL-free path"))
		}
		hostCount, portCount := 0, 0
		for _, token := range entry.Argv {
			if token == "" || strings.IndexByte(token, 0) >= 0 {
				return engineCatalogMalformed("entry.argv", errors.New("tokens must be non-empty and NUL-free"))
			}
			if token == "{host}" {
				hostCount++
			}
			if token == "{port}" {
				portCount++
			}
			if token != "{host}" && strings.Contains(token, "{host}") || token != "{port}" && strings.Contains(token, "{port}") {
				return engineCatalogMalformed("entry.endpoint_placeholders", errors.New("endpoint placeholders must occupy whole argv tokens"))
			}
		}
		if hostCount != 1 || portCount != 1 {
			return engineCatalogMalformed("entry.endpoint_placeholders", errors.New("argv must contain exactly one whole {host} and one whole {port} token"))
		}
		if hasCatalogRemoteFields(entry) {
			return engineCatalogMalformed("entry.locality", errors.New("local mode cannot declare SSH or remote fields"))
		}
		if err := validateEngineCatalogKnobs(entry.Engine, entry.Knobs); err != nil {
			return err
		}
		if err := validateEngineCatalogStress(entry.Stress); err != nil {
			return err
		}
		if err := validateEngineCatalogSupervision(entry.Supervision); err != nil {
			return err
		}
		if err := validateEngineCatalogPinnedDistribution(entry.PinnedDistribution); err != nil {
			return err
		}
		if entry.ServedModelID != "" {
			if _, ok := engineCatalogServedModelID(entry); !ok {
				return engineCatalogMalformed("entry.served_model_id", errors.New("must match the served model id token grammar"))
			}
		}
	case "ssh":
		if !knownCatalogEngine(entry.Engine) {
			return engineCatalogMalformed("entry.engine", fmt.Errorf("unknown engine kind %q", entry.Engine))
		}
		if entry.Model != "" || entry.Knobs != nil {
			return engineCatalogMalformed("entry.ssh_identity", errors.New("SSH mode cannot declare model or knobs"))
		}
		if entry.Executable != "" || len(entry.Argv) != 0 {
			return engineCatalogMalformed("entry.ssh_argv", errors.New("SSH mode cannot declare executable or argv"))
		}
		if !engineCatalogNamePattern.MatchString(entry.SSHTarget) {
			return engineCatalogMalformed("entry.ssh_target", errors.New("must be a simple host alias"))
		}
		if entry.SSHExecutable != "" && (!filepath.IsAbs(entry.SSHExecutable) || strings.IndexByte(entry.SSHExecutable, 0) >= 0) {
			return engineCatalogMalformed("entry.ssh_executable", errors.New("must be an absolute NUL-free path"))
		}
		if !filepath.IsAbs(entry.RemoteExecutable) || strings.IndexByte(entry.RemoteExecutable, 0) >= 0 {
			return engineCatalogMalformed("entry.remote_executable", errors.New("must be an absolute NUL-free path"))
		}
		if entry.RemoteConfig != "" && !filepath.IsAbs(entry.RemoteConfig) {
			return engineCatalogMalformed("entry.remote_config", errors.New("must be an absolute path when set"))
		}
		if !engineCatalogNamePattern.MatchString(entry.RemoteProfile) {
			return engineCatalogMalformed("entry.remote_profile", errors.New("must be a simple profile name"))
		}
		if entry.RemoteHost != engineCatalogDefaultHost {
			return engineCatalogMalformed("entry.remote_host", fmt.Errorf("must equal %s", engineCatalogDefaultHost))
		}
		if entry.RemotePort < 1 || entry.RemotePort > 65535 {
			return engineCatalogMalformed("entry.remote_port", errors.New("must be between 1 and 65535"))
		}
		if entry.ServedModelID != "" {
			if _, ok := engineCatalogServedModelID(entry); !ok || entry.ServedModelID == entry.RemoteProfile {
				return engineCatalogMalformed("entry.served_model_id", errors.New("must be a valid served model id distinct from the remote profile name"))
			}
		}
		if entry.Stress != nil || entry.Supervision != nil || entry.PinnedDistribution != nil {
			return engineCatalogMalformed("entry.ssh_local_policy", errors.New("SSH mode cannot declare stress, supervision, or pinned distribution"))
		}
	default:
		return engineCatalogMalformed("entry.mode", errors.New("mode must equal local or ssh"))
	}
	return nil
}

func knownCatalogEngine(engine string) bool {
	switch engine {
	case "mlx-lm", "mlx-swift", "llama-cpp":
		return true
	default:
		return false
	}
}

func hasCatalogRemoteFields(entry EngineCatalogEntry) bool {
	return entry.SSHExecutable != "" || entry.SSHTarget != "" || entry.RemoteExecutable != "" || entry.RemoteConfig != "" || entry.RemoteProfile != "" || entry.RemoteHost != "" || entry.RemotePort != 0
}

func validateEngineCatalogKnobs(engine string, knobs *EngineCatalogKnobs) error {
	if knobs == nil {
		return nil
	}
	if value := knobs.KVContextTokens; value != "" && value != "unbounded" {
		if number, err := strconv.Atoi(value); err != nil || number <= 0 {
			return engineCatalogMalformed("entry.knobs.kv_context_size", errors.New("must be a positive integer or unbounded"))
		}
	}
	if knobs.KVContextTokens == "unbounded" && engine == "llama-cpp" {
		return engineCatalogMalformed("entry.knobs.kv_context_size", errors.New("unbounded has no finite llama-cpp context-size equivalent"))
	}
	if value := knobs.PrefillChunkTokens; value != "" {
		if !engineCatalogPositiveInteger(value) {
			return engineCatalogMalformed("entry.knobs.prefill_chunk_size", errors.New("must be a positive integer"))
		}
	}
	if value := knobs.ReasoningEffort; value != "" && value != "low" && value != "medium" && value != "xhigh" {
		return engineCatalogMalformed("entry.knobs.reasoning_effort", errors.New("must equal low, medium, or xhigh"))
	}
	if value := knobs.SpeculativeDecoding; value != "" {
		if engine != "llama-cpp" {
			return engineCatalogMalformed("entry.knobs.speculative_decoding", errors.New("speculative decoding is supported only by llama-cpp"))
		}
		if value != "off" && value != "ngram" && value != "mtp" {
			return engineCatalogMalformed("entry.knobs.speculative_decoding", errors.New("must equal off, ngram, or mtp"))
		}
	}
	return nil
}

func validateEngineCatalogStress(policy *EngineCatalogStressPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.PromptTokens < 1024 || policy.PromptTokens > 1_000_000 {
		return engineCatalogMalformed("entry.stress.prompt_tokens", errors.New("must be between 1024 and 1000000"))
	}
	if policy.MaxOutputTokens < 1 || policy.MaxOutputTokens > 4096 {
		return engineCatalogMalformed("entry.stress.max_output_tokens", errors.New("must be between 1 and 4096"))
	}
	if policy.StartupTimeoutSeconds < 1 || policy.StartupTimeoutSeconds > 3600 {
		return engineCatalogMalformed("entry.stress.startup_timeout_seconds", errors.New("must be between 1 and 3600"))
	}
	if policy.RequestTimeoutSeconds < 1 || policy.RequestTimeoutSeconds > 86400 {
		return engineCatalogMalformed("entry.stress.request_timeout_seconds", errors.New("must be between 1 and 86400"))
	}
	if policy.SampleIntervalMilliseconds < 50 || policy.SampleIntervalMilliseconds > 10000 {
		return engineCatalogMalformed("entry.stress.sample_interval_milliseconds", errors.New("must be between 50 and 10000"))
	}
	return nil
}

func validateEngineCatalogSupervision(policy *EngineCatalogSupervisionPolicy) error {
	if policy == nil {
		return nil
	}
	if len(policy.FatalOutputSubstrings) < 1 || len(policy.FatalOutputSubstrings) > 16 {
		return engineCatalogMalformed("entry.supervision.fatal_output_substrings", errors.New("must contain between 1 and 16 entries"))
	}
	for _, marker := range policy.FatalOutputSubstrings {
		if marker == "" || len(marker) > 512 || strings.IndexByte(marker, 0) >= 0 {
			return engineCatalogMalformed("entry.supervision.fatal_output_substrings", errors.New("entries must be non-empty, at most 512 bytes, and NUL-free"))
		}
	}
	if policy.MaxRestarts < 1 || policy.MaxRestarts > 100 {
		return engineCatalogMalformed("entry.supervision.max_restarts", errors.New("must be between 1 and 100"))
	}
	if policy.RestartWindowSeconds < 1 || policy.RestartWindowSeconds > 86400 {
		return engineCatalogMalformed("entry.supervision.restart_window_seconds", errors.New("must be between 1 and 86400"))
	}
	if policy.RestartDelayMilliseconds < 0 || policy.RestartDelayMilliseconds > 60000 {
		return engineCatalogMalformed("entry.supervision.restart_delay_milliseconds", errors.New("must be between 0 and 60000"))
	}
	return nil
}

func validateEngineCatalogPinnedDistribution(pin *EngineCatalogPinnedDistribution) error {
	if pin == nil {
		return nil
	}
	if !engineCatalogPinnedPackagePattern.MatchString(pin.Package) {
		return engineCatalogMalformed("entry.pinned_distribution.package", errors.New("must be a simple distribution name"))
	}
	if !filepath.IsAbs(pin.SitePackages) || strings.IndexByte(pin.SitePackages, 0) >= 0 {
		return engineCatalogMalformed("entry.pinned_distribution.site_packages", errors.New("must be an absolute NUL-free path"))
	}
	if !engineCatalogPinnedCommitPattern.MatchString(pin.Commit) {
		return engineCatalogMalformed("entry.pinned_distribution.commit", errors.New("must be a full 40-character lowercase git commit hash"))
	}
	return nil
}

func engineCatalogPath(homeDir string) (string, error) {
	if homeDir == "" {
		var err error
		homeDir, err = userEngineCatalogHomeDir()
		if err != nil {
			return "", engineCatalogReadFailed("catalog.home", err)
		}
	}
	if !filepath.IsAbs(homeDir) {
		return "", engineCatalogReadFailed("catalog.home", errors.New("home directory must be absolute"))
	}
	return filepath.Join(homeDir, ".curator", EngineCatalogFileName), nil
}

func catalogEndpointPort(name string, entry EngineCatalogEntry) int {
	// served_model_id is manager readiness metadata. Keeping it out of the
	// endpoint identity preserves the legacy Pi endpoint when a catalog entry
	// adds an explicit id to an otherwise matching profile.
	identityEntry := entry
	identityEntry.ServedModelID = ""
	data, err := marshalEngineCatalogEntry(identityEntry)
	if err != nil {
		data = []byte(fmt.Sprintf("%#v", entry))
	}
	identity := append(append([]byte(name), 0), data...)
	digest := sha256.Sum256(identity)
	return engineCatalogPortBase + int(binary.BigEndian.Uint16(digest[:2])%engineCatalogPortCount)
}

func engineCatalogEndpoint(port int) string {
	return fmt.Sprintf("http://%s:%d/v1", engineCatalogDefaultHost, port)
}

func engineCatalogRuntimeArgv(entry EngineCatalogEntry, port int, servedModelID string) []string {
	if entry.Mode == "ssh" {
		return engineCatalogSSHArgv(entry, port)
	}
	argv := engineCatalogResolvedBaseArgv(entry, port)
	if entry.Model != "" {
		argv = append(argv, "--model", entry.Model)
	}
	argv = append(argv, engineCatalogKnobArgv(entry.Engine, entry.Knobs)...)
	switch entry.Engine {
	case "llama-cpp":
		argv = removeCatalogFlags(argv, "-a", "--alias")
		argv = append(argv, "--alias", servedModelID)
	case "mlx-swift":
		argv = removeCatalogFlags(argv, "--model-id")
		argv = append(argv, "--model-id", servedModelID)
	}
	return argv
}

func engineCatalogIdentityArgv(entry EngineCatalogEntry, port int) []string {
	if entry.Mode == "ssh" {
		return engineCatalogSSHArgv(entry, port)
	}
	argv := engineCatalogResolvedBaseArgv(entry, port)
	if entry.Model != "" {
		argv = append(argv, "--model", entry.Model)
	}
	return append(argv, engineCatalogKnobArgv(entry.Engine, entry.Knobs)...)
}

func engineCatalogResolvedBaseArgv(entry EngineCatalogEntry, port int) []string {
	argv := make([]string, len(entry.Argv))
	portText := strconv.Itoa(port)
	for index, token := range entry.Argv {
		switch token {
		case "{host}":
			argv[index] = engineCatalogDefaultHost
		case "{port}":
			argv[index] = portText
		default:
			argv[index] = token
		}
	}
	return argv
}

func removeCatalogFlags(argv []string, flags ...string) []string {
	removed := make(map[string]bool, len(flags))
	for _, flag := range flags {
		removed[flag] = true
	}
	result := make([]string, 0, len(argv))
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		matched := false
		for flag := range removed {
			if token == flag || strings.HasPrefix(token, flag+"=") {
				matched = true
				break
			}
		}
		if matched {
			if token == "-a" || token == "--alias" || token == "--model-id" {
				if index+1 < len(argv) {
					index++
				}
			}
			continue
		}
		result = append(result, token)
	}
	return result
}

func engineCatalogKnobArgv(engine string, knobs *EngineCatalogKnobs) []string {
	if knobs == nil {
		return nil
	}
	var argv []string
	if value := knobs.KVContextTokens; engineCatalogPositiveInteger(value) {
		if engine == "llama-cpp" {
			argv = append(argv, "--ctx-size", value)
		} else {
			argv = append(argv, "--max-kv-size", value)
		}
	}
	if value := knobs.PrefillChunkTokens; engineCatalogPositiveInteger(value) {
		if engine == "llama-cpp" {
			argv = append(argv, "--ubatch-size", value)
		} else {
			argv = append(argv, "--prefill-step-size", value)
		}
	}
	if value := knobs.ReasoningEffort; value != "" {
		if engine == "mlx-lm" {
			argv = append(argv, "--chat-template-args", fmt.Sprintf(`{"reasoning_effort": %q}`, value))
		} else {
			argv = append(argv, "--reasoning-effort", value)
		}
	}
	if value := knobs.SpeculativeDecoding; value != "" {
		if value == "off" {
			return argv
		}
		if value == "ngram" {
			value = "ngram-mod"
		} else {
			value = "draft-mtp"
		}
		argv = append(argv, "--spec-type", value)
	}
	return argv
}

func engineCatalogSSHArgv(entry EngineCatalogEntry, port int) []string {
	remote := []string{entry.RemoteExecutable, "run", entry.RemoteProfile}
	if entry.RemoteConfig != "" {
		remote = append(remote, "--config", entry.RemoteConfig)
	}
	remote = append(remote, "--host", entry.RemoteHost, "--port", strconv.Itoa(entry.RemotePort))
	remoteCommand := make([]string, len(remote))
	for index, token := range remote {
		remoteCommand[index] = shellQuoteCatalog(token)
	}
	return []string{
		"-tt",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("%s:%d:%s:%d", engineCatalogDefaultHost, port, entry.RemoteHost, entry.RemotePort),
		entry.SSHTarget,
		strings.Join(remoteCommand, " "),
	}
}

func shellQuoteCatalog(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func engineCatalogSharing(entry EngineCatalogEntry, runtime PiRuntime) PiRuntimeSharing {
	sharing := PiRuntimeSharing{
		Mode: "shared", LingerSeconds: 0, MaxLeases: 8,
		MaxSegmentBytes: 1 << 20, MaxSegments: 7,
		HeartbeatIntervalSeconds: 15, LeaseStaleSeconds: 60,
		BrokerStartTimeoutSeconds: runtime.StartupTimeoutSeconds + runtime.ShutdownTimeoutSeconds + 30,
		RestartLimit:              0, RestartInitialBackoffSeconds: 1, RestartMaxBackoffSeconds: 1,
		StableRunSeconds: 60, QuarantineSeconds: 30, ResourcePressureMode: "disabled",
	}
	if entry.Supervision == nil {
		sharing.restartFailureThreshold = 1
		return sharing
	}
	policy := entry.Supervision
	sharing.FatalOutputSubstrings = append([]string(nil), policy.FatalOutputSubstrings...)
	sharing.StableRunSeconds = policy.RestartWindowSeconds
	sharing.RestartInitialBackoffMilliseconds = policy.RestartDelayMilliseconds
	sharing.RestartMaxBackoffMilliseconds = policy.RestartDelayMilliseconds
	sharing.RestartInitialBackoffSeconds = (policy.RestartDelayMilliseconds + 999) / 1000
	sharing.RestartMaxBackoffSeconds = sharing.RestartInitialBackoffSeconds
	sharing.restartInitialBackoff = time.Duration(policy.RestartDelayMilliseconds) * time.Millisecond
	sharing.restartMaxBackoff = sharing.restartInitialBackoff
	sharing.restartBackoffSet = true
	if policy.RestartOnFailure {
		sharing.RestartLimit = policy.MaxRestarts
		sharing.restartFailureThreshold = policy.MaxRestarts + 1
	} else {
		sharing.RestartLimit = 0
		sharing.restartFailureThreshold = 1
	}
	sharing.fatalOutputRestartFailureThreshold = policy.MaxRestarts + 1
	return sharing
}

func resolveCatalogProfile(project string, homeDir, cacheRoot, name string, entry EngineCatalogEntry, catalogFile string) (sharedResolvedProfile, error) {
	port := catalogEndpointPort(name, entry)
	model, ok := engineCatalogServedModelID(entry)
	notRunnableCode := ""
	notRunnableReason := ""
	if !ok || model == "" {
		notRunnableCode = "served_model_id_missing"
		notRunnableReason = "catalog entry has no served_model_id; readiness cannot be established"
	} else {
		notRunnableCode, notRunnableReason = engineCatalogExecutableNotRunnable(entry)
	}
	argv := engineCatalogRuntimeArgv(entry, port, model)
	executable := entry.Executable
	if entry.Mode == "ssh" {
		executable = entry.SSHExecutable
		if executable == "" {
			executable = "/usr/bin/ssh"
		}
	}
	startupTimeout := engineCatalogStartupDefault
	if entry.Stress != nil {
		startupTimeout = entry.Stress.StartupTimeoutSeconds
	}
	runtime := PiRuntime{
		Executable: executable, Argv: argv, ReadinessPath: "/models",
		StartupTimeoutSeconds: startupTimeout, ShutdownTimeoutSeconds: engineCatalogShutdownDefault,
	}
	sharing := engineCatalogSharing(entry, runtime)
	runtime.Sharing = &sharing
	profile := PiProfile{
		Provider: "catalog-engine", Publisher: entry.Engine, Family: entry.Engine,
		Model: model, BaseURL: engineCatalogEndpoint(port), API: "openai-completions",
		Reasoning: false, Input: []string{"text"},
		Runtime: runtime, Source: catalogFile,
	}
	runtimeKey, profileDigest := SharedRuntimeKey(profile)
	paths, err := ResolveSharedRuntimePaths(cacheRoot, runtimeKey)
	if err != nil {
		// Catalog runnability is platform-neutral. An unsupported runtime
		// platform cannot erase an already established non-runnable reason;
		// all other path failures and runnable platform refusals still apply.
		var shared *SharedRuntimeError
		if notRunnableCode == "" || !errors.As(err, &shared) || shared.Code != "shared_runtime_platform_unsupported" {
			return sharedResolvedProfile{}, err
		}
	}
	return sharedResolvedProfile{
		Project: project, HomeDir: homeDir, ProfileName: name,
		EngineIdentity: SharedRuntimeEngineIdentity{Name: name, Key: runtimeKey},
		Profile:        profile, Sharing: *runtime.Sharing, RuntimeKey: runtimeKey,
		ProfileDigest: profileDigest, Paths: paths, CatalogEntry: true, CatalogMode: entry.Mode,
		NotRunnableCode: notRunnableCode, NotRunnableReason: notRunnableReason,
	}, nil
}

func engineCatalogExecutableNotRunnable(entry EngineCatalogEntry) (code, reason string) {
	executable := entry.Executable
	if entry.Mode == "ssh" {
		executable = entry.SSHExecutable
		if executable == "" {
			executable = "/usr/bin/ssh"
		}
	}
	file, err := os.Open(executable)
	if err != nil {
		return "executable_unavailable", "configured engine executable cannot be read on this host"
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "executable_unavailable", "configured engine executable identity cannot be determined"
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "executable_not_runnable", "configured engine executable is not a regular executable file"
	}
	var prefix [2]byte
	n, readErr := io.ReadFull(file, prefix[:])
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "executable_unavailable", "configured engine executable type cannot be read"
	}
	if n == len(prefix) && prefix[0] == '#' && prefix[1] == '!' {
		return "script_executable_unsupported", "configured engine executable is a script; use its native interpreter with a module entry point"
	}
	return "", ""
}

func engineCatalogNotRunnableError(resolved sharedResolvedProfile) error {
	return sharedRuntimeError("engine_not_runnable", fmt.Errorf("%s: %s", resolved.NotRunnableCode, resolved.NotRunnableReason))
}

// engineCatalogServedModelID returns the catalog's declared readiness identity.
// Runtime code never derives this value from model or alias argv.
func engineCatalogServedModelID(entry EngineCatalogEntry) (string, bool) {
	return entry.ServedModelID, engineCatalogServedModelIDPattern.MatchString(entry.ServedModelID)
}
