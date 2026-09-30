package infra

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

type legacyFixtureDocument struct {
	Profiles map[string]legacyFixtureProfile `toml:"profiles"`
}

type legacyFixtureKnobs struct {
	KVContextTokens     string `toml:"kv_context_tokens,omitempty"`
	PrefillChunkTokens  string `toml:"prefill_chunk_tokens,omitempty"`
	ReasoningEffort     string `toml:"reasoning_effort,omitempty"`
	SpeculativeDecoding string `toml:"speculative_decoding,omitempty"`
}

type legacyFixtureStress struct {
	PromptTokens               int `toml:"prompt_tokens"`
	MaxOutputTokens            int `toml:"max_output_tokens"`
	StartupTimeoutSeconds      int `toml:"startup_timeout_seconds"`
	RequestTimeoutSeconds      int `toml:"request_timeout_seconds"`
	SampleIntervalMilliseconds int `toml:"sample_interval_milliseconds"`
}

type legacyFixtureSupervision struct {
	FatalOutputSubstrings    []string `toml:"fatal_output_substrings"`
	RestartOnFailure         bool     `toml:"restart_on_failure"`
	MaxRestarts              int      `toml:"max_restarts"`
	RestartWindowSeconds     int      `toml:"restart_window_seconds"`
	RestartDelayMilliseconds int      `toml:"restart_delay_milliseconds"`
}

type legacyFixturePinnedDistribution struct {
	Package      string `toml:"package"`
	SitePackages string `toml:"site_packages"`
	Commit       string `toml:"commit"`
}

type legacyFixtureProfile struct {
	Mode               string                           `toml:"mode"`
	Engine             string                           `toml:"engine,omitempty"`
	Model              string                           `toml:"model,omitempty"`
	Executable         string                           `toml:"executable,omitempty"`
	Argv               []string                         `toml:"argv,omitempty"`
	Knobs              *legacyFixtureKnobs              `toml:"knobs,omitempty"`
	SSHExecutable      string                           `toml:"ssh_executable,omitempty"`
	SSHTarget          string                           `toml:"ssh_target,omitempty"`
	RemoteExecutable   string                           `toml:"remote_executable,omitempty"`
	RemoteConfig       string                           `toml:"remote_config,omitempty"`
	RemoteProfile      string                           `toml:"remote_profile,omitempty"`
	RemoteHost         string                           `toml:"remote_host,omitempty"`
	RemotePort         int                              `toml:"remote_port,omitempty"`
	Stress             *legacyFixtureStress             `toml:"stress,omitempty"`
	Supervision        *legacyFixtureSupervision        `toml:"supervision,omitempty"`
	PinnedDistribution *legacyFixturePinnedDistribution `toml:"pinned_distribution,omitempty"`
}

func (profile legacyFixtureProfile) catalogEntry(name string) EngineCatalogEntry {
	engine := profile.Engine
	if profile.Mode == "local" && engine == "" {
		engine = "mlx-lm"
	}
	if profile.Mode == "ssh" && engine == "" {
		// The legacy SSH schema has no engine field. Schema v1 requires a known
		// engine kind for the co-reader, so this fixture supplies one catalog
		// discriminator without changing any transcribed legacy field.
		engine = "mlx-lm"
	}
	var knobs *EngineCatalogKnobs
	if profile.Knobs != nil {
		knobs = &EngineCatalogKnobs{
			KVContextTokens: profile.Knobs.KVContextTokens, PrefillChunkTokens: profile.Knobs.PrefillChunkTokens,
			ReasoningEffort: profile.Knobs.ReasoningEffort, SpeculativeDecoding: profile.Knobs.SpeculativeDecoding,
		}
	}
	var stress *EngineCatalogStressPolicy
	if profile.Stress != nil {
		stress = &EngineCatalogStressPolicy{
			PromptTokens: profile.Stress.PromptTokens, MaxOutputTokens: profile.Stress.MaxOutputTokens,
			StartupTimeoutSeconds: profile.Stress.StartupTimeoutSeconds, RequestTimeoutSeconds: profile.Stress.RequestTimeoutSeconds,
			SampleIntervalMilliseconds: profile.Stress.SampleIntervalMilliseconds,
		}
	}
	var supervision *EngineCatalogSupervisionPolicy
	if profile.Supervision != nil {
		supervision = &EngineCatalogSupervisionPolicy{
			FatalOutputSubstrings: append([]string(nil), profile.Supervision.FatalOutputSubstrings...),
			RestartOnFailure:      profile.Supervision.RestartOnFailure, MaxRestarts: profile.Supervision.MaxRestarts,
			RestartWindowSeconds: profile.Supervision.RestartWindowSeconds, RestartDelayMilliseconds: profile.Supervision.RestartDelayMilliseconds,
		}
	}
	var pinned *EngineCatalogPinnedDistribution
	if profile.PinnedDistribution != nil {
		pinned = &EngineCatalogPinnedDistribution{
			Package: profile.PinnedDistribution.Package, SitePackages: profile.PinnedDistribution.SitePackages,
			Commit: profile.PinnedDistribution.Commit,
		}
	}
	entry := EngineCatalogEntry{
		Mode: profile.Mode, Engine: engine, Model: profile.Model,
		Executable: profile.Executable, Argv: append([]string(nil), profile.Argv...),
		Knobs: knobs, SSHExecutable: profile.SSHExecutable,
		SSHTarget: profile.SSHTarget, RemoteExecutable: profile.RemoteExecutable,
		RemoteConfig: profile.RemoteConfig, RemoteProfile: profile.RemoteProfile,
		RemoteHost: profile.RemoteHost, RemotePort: profile.RemotePort,
		Stress: stress, Supervision: supervision,
		PinnedDistribution: pinned,
	}
	entry.ServedModelID = legacyFixtureModelID(entry)
	if entry.ServedModelID == "" {
		// Some accepted legacy profiles are benchmark/policy records without a
		// model selection. The fixture supplies an explicit contract id so the
		// complete legacy field set still round-trips through schema validation.
		entry.ServedModelID = "fixture-model-" + name
	}
	return entry
}

func legacyFixtureModelID(entry EngineCatalogEntry) string {
	if entry.Mode == "ssh" {
		// The checked-in legacy SSH fixture names a remote profile but does not
		// contain that remote profile's own model metadata.
		return "fixture-remote-model-id"
	}
	for index, argument := range entry.Argv {
		if argument == "--alias" || argument == "-a" {
			if index+1 < len(entry.Argv) {
				return strings.SplitN(entry.Argv[index+1], ",", 2)[0]
			}
		}
		if strings.HasPrefix(argument, "--alias=") || strings.HasPrefix(argument, "-a=") {
			return strings.SplitN(strings.SplitN(argument, "=", 2)[1], ",", 2)[0]
		}
	}
	model := entry.Model
	if model == "" {
		for index, argument := range entry.Argv {
			if argument == "--model" || argument == "-m" {
				if index+1 < len(entry.Argv) {
					model = entry.Argv[index+1]
					break
				}
			}
			if strings.HasPrefix(argument, "--model=") || strings.HasPrefix(argument, "-m=") {
				model = strings.SplitN(argument, "=", 2)[1]
				break
			}
		}
	}
	if model == "" {
		return ""
	}
	return path.Base(model)
}

func TestEngineCatalogSchemaV1RoundTripsEveryLegacyProfileField(t *testing.T) {
	fixture, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var legacy legacyFixtureDocument
	if err := toml.Unmarshal(fixture, &legacy); err != nil {
		t.Fatalf("decode legacy model-harness fixture: %v", err)
	}
	entries := make(map[string]EngineCatalogEntry, len(legacy.Profiles))
	for name, profile := range legacy.Profiles {
		entries[name] = profile.catalogEntry(name)
	}
	catalog, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: entries})
	if err != nil {
		t.Fatalf("encode engines.toml v1: %v", err)
	}
	path := writeEngineCatalogFixture(t, catalog)
	for name, legacyProfile := range legacy.Profiles {
		got, state, err := ReadEngineCatalogEntry(path, name)
		if err != nil || state != EngineCatalogPresent {
			t.Fatalf("ReadEngineCatalogEntry(%q) state=%s err=%v", name, state, err)
		}
		want := legacyProfile.catalogEntry(name)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("profile %q did not round-trip losslessly:\n got: %#v\nwant: %#v", name, got, want)
		}
		if legacyProfile.Mode == "local" && legacyProfile.Engine == "" && got.Engine != "mlx-lm" {
			t.Errorf("profile %q omitted engine but did not transcribe the legacy default mlx-lm: got %q", name, got.Engine)
		}
	}
}

func TestWriteEngineCatalogContractFixturesForCoReader(t *testing.T) {
	outputDir := os.Getenv("CURATOR_ENGINE_CATALOG_CONTRACT_OUT")
	if outputDir == "" {
		t.Skip("nested runtimecatalog contract test supplies the fixture output directory")
	}
	legacyBytes, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var legacy legacyFixtureDocument
	if err := toml.Unmarshal(legacyBytes, &legacy); err != nil {
		t.Fatalf("decode real legacy profile fixtures: %v", err)
	}
	fixtures := map[string]EngineCatalogEntry{
		"local":           legacy.Profiles["llama-cpp-knobs"].catalogEntry("llama-cpp-knobs"),
		"local-unbounded": legacy.Profiles["mlx-lm-unbounded"].catalogEntry("mlx-lm-unbounded"),
		"ssh":             legacy.Profiles["qwen-remote"].catalogEntry("qwen-remote"),
	}
	if fixtures["local"].Engine != "llama-cpp" || fixtures["local-unbounded"].Knobs == nil || fixtures["local-unbounded"].Knobs.KVContextTokens != "unbounded" || fixtures["ssh"].Mode != "ssh" {
		t.Fatalf("contract fixtures did not select real local and SSH profiles: %#v", fixtures)
	}
	for kind, entry := range fixtures {
		name := "contract-" + strings.ReplaceAll(kind, "_", "-")
		data, err := MarshalEngineCatalog(EngineCatalogDocument{
			SchemaVersion: EngineCatalogSchemaVersion,
			Engines:       map[string]EngineCatalogEntry{name: entry},
		})
		if err != nil {
			t.Fatalf("encode %s contract fixture with manager writer: %v", kind, err)
		}
		if err := os.MkdirAll(outputDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outputDir, kind+"-engines.toml"), data, 0o600); err != nil {
			t.Fatalf("write %s contract fixture: %v", kind, err)
		}
	}

	const seed int64 = 0x5eedc0de
	const iterations = 128
	names := generatedEngineCatalogNameCorpus(seed, iterations)
	manifest := struct {
		Seed       int64 `json:"seed"`
		Iterations int   `json:"iterations"`
		Cases      []struct {
			Name            string `json:"name"`
			File            string `json:"file"`
			Mode            string `json:"mode"`
			Class           string `json:"class"`
			ManagerAccepted bool   `json:"manager_accepted"`
		} `json:"cases"`
	}{Seed: seed, Iterations: iterations}
	for index, name := range names {
		mode := "local"
		entry := EngineCatalogEntry{
			Mode: "local", Engine: "mlx-lm", ServedModelID: fmt.Sprintf("served-%03d", index),
			Model: fmt.Sprintf("org/model-%03d", index), Executable: "/opt/fixture/mlx_lm.server",
			Argv:  []string{"serve", "--host", "{host}", "--port", "{port}"},
			Knobs: &EngineCatalogKnobs{KVContextTokens: "32768", PrefillChunkTokens: "1024"},
		}
		if index%2 == 1 {
			mode = "ssh"
			entry = EngineCatalogEntry{
				Mode: "ssh", Engine: "llama-cpp", ServedModelID: fmt.Sprintf("remote-served-%03d", index),
				SSHExecutable: "/usr/bin/ssh", SSHTarget: "model-host", RemoteExecutable: "/opt/model-harness",
				RemoteProfile: "qwen-remote", RemoteHost: "127.0.0.1", RemotePort: 18012,
			}
		}
		document := EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{name: entry}}
		managerData, managerErr := MarshalEngineCatalog(document)
		rawData, rawErr := marshalEngineCatalogDocument(document)
		if rawErr != nil {
			t.Fatalf("encode raw generated catalog %q: %v", name, rawErr)
		}
		managerAccepted := managerErr == nil
		data := rawData
		if managerAccepted {
			data = managerData
		} else if !isEngineCatalogSite(managerErr, EngineCatalogMalformed, "entry.name") {
			t.Fatalf("generated manager rejection for %q was not an entry-name refusal: %v", name, managerErr)
		}
		catalogPath := writeEngineCatalogFixture(t, data)
		_, state, readErr := ReadEngineCatalogEntry(catalogPath, name)
		if managerAccepted && (state != EngineCatalogPresent || readErr != nil) {
			t.Fatalf("manager writer accepted but manager reader refused %q: state=%s err=%v", name, state, readErr)
		}
		if !managerAccepted && (state != EngineCatalogMalformed || !isEngineCatalogSite(readErr, EngineCatalogMalformed, "catalog.engine_name")) {
			t.Fatalf("manager writer refused but manager reader did not reject %q as a co-reader-incompatible name: state=%s err=%v", name, state, readErr)
		}
		file := fmt.Sprintf("generated-%03d.toml", index)
		if err := os.WriteFile(filepath.Join(outputDir, file), data, 0o600); err != nil {
			t.Fatalf("write generated %s catalog: %v", mode, err)
		}
		manifest.Cases = append(manifest.Cases, struct {
			Name            string `json:"name"`
			File            string `json:"file"`
			Mode            string `json:"mode"`
			Class           string `json:"class"`
			ManagerAccepted bool   `json:"manager_accepted"`
		}{Name: name, File: file, Mode: mode, Class: "generated-name", ManagerAccepted: managerAccepted})
	}
	for iteration := 0; iteration < iterations; iteration++ {
		class := []string{"sibling-missing-engine", "sibling-empty-engine", "sibling-untrimmed-engine", "sibling-case-variant-engine"}[iteration%4]
		validEntry := EngineCatalogEntry{
			Mode: "local", Engine: "mlx-lm", ServedModelID: fmt.Sprintf("sibling-served-%03d", iteration),
			Model: fmt.Sprintf("org/sibling-model-%03d", iteration), Executable: "/opt/fixture/mlx_lm.server",
			Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
		}
		sibling := validEntry
		sibling.ServedModelID = fmt.Sprintf("sibling-second-%03d", iteration)
		document := EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{
			"target-qwen": validEntry, "sibling-qwen": sibling,
		}}
		data, err := marshalEngineCatalogDocument(document)
		if err != nil {
			t.Fatalf("encode generated %s differential input: %v", class, err)
		}
		section := "[engines.sibling-qwen]"
		sectionIndex := strings.Index(string(data), section)
		if sectionIndex < 0 {
			t.Fatalf("generated two-entry document lacks sibling table: %s", data)
		}
		suffix := string(data[sectionIndex:])
		engineIndex := strings.Index(suffix, "engine = ")
		if engineIndex < 0 {
			t.Fatalf("generated sibling table lacks engine discriminator: %s", suffix)
		}
		lineRemainder := suffix[engineIndex:]
		lineLength := strings.IndexByte(lineRemainder, '\n')
		if lineLength < 0 {
			t.Fatalf("generated sibling engine discriminator is not line-terminated: %s", suffix)
		}
		engineLine := suffix[engineIndex : engineIndex+lineLength+1]
		switch class {
		case "sibling-missing-engine":
			suffix = suffix[:engineIndex] + suffix[engineIndex+len(engineLine):]
		case "sibling-empty-engine":
			suffix = suffix[:engineIndex] + "engine = \"\"\n" + suffix[engineIndex+len(engineLine):]
		case "sibling-untrimmed-engine":
			suffix = suffix[:engineIndex] + "engine = \" mlx-lm\"\n" + suffix[engineIndex+len(engineLine):]
		case "sibling-case-variant-engine":
			suffix = suffix[:engineIndex] + "ENGINE = \"mlx-lm\"\n" + suffix[engineIndex+len(engineLine):]
		}
		data = append(append([]byte(nil), data[:sectionIndex]...), []byte(suffix)...)
		catalogPath := writeEngineCatalogFixture(t, data)
		_, state, readErr := ReadEngineCatalogEntry(catalogPath, "target-qwen")
		managerAccepted := state == EngineCatalogPresent && readErr == nil
		if managerAccepted || !isEngineCatalogState(readErr, EngineCatalogMalformed) {
			t.Fatalf("document-level co-reader mutation %s was not refused by manager: state=%s err=%v\n%s", class, state, readErr, data)
		}
		file := fmt.Sprintf("generated-sibling-%03d.toml", iteration)
		if err := os.WriteFile(filepath.Join(outputDir, file), data, 0o600); err != nil {
			t.Fatalf("write generated sibling differential catalog: %v", err)
		}
		manifest.Cases = append(manifest.Cases, struct {
			Name            string `json:"name"`
			File            string `json:"file"`
			Mode            string `json:"mode"`
			Class           string `json:"class"`
			ManagerAccepted bool   `json:"manager_accepted"`
		}{Name: "target-qwen", File: file, Mode: "local+sibling", Class: class, ManagerAccepted: managerAccepted})
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "generated-catalogs.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write generated catalog manifest: %v", err)
	}
}

func generatedEngineCatalogNameCorpus(seed int64, iterations int) []string {
	stems := []string{
		"accesskey", "apikey", "auth", "authorization", "bearer", "clientsecret", "cookie", "credential", "jwt",
		"pass", "passphrase", "passw", "privatekey", "privkey", "pwd", "secret", "sessionid", "sessionkey",
		"sessionsecret", "sessiontoken", "signingkey", "token",
	}
	names := []string{"qwen-local", "mlx_swift", "profile.01", "author", "passthrough"}
	for index, stem := range stems {
		names = append(names, fmt.Sprintf("engine-%s-case-%03d", stem, index))
	}
	names = append(names, "context-window-tokens", "engine.context.tokens", "profile-01-tokens")
	rng := rand.New(rand.NewSource(seed))
	for iteration := 0; iteration < iterations; iteration++ {
		switch iteration % 4 {
		case 0:
			names = append(names, fmt.Sprintf("random-safe-%03d-%02x", iteration, rng.Intn(256)))
		case 1:
			stem := stems[rng.Intn(len(stems))]
			names = append(names, fmt.Sprintf("engine-%s-rand-%03d", stem, iteration))
		case 2:
			stem := stems[rng.Intn(len(stems))]
			title := strings.ToUpper(stem[:1]) + stem[1:]
			names = append(names, fmt.Sprintf("engine%sRand%03d", title, iteration))
		default:
			names = append(names, fmt.Sprintf("context-window-%03d-tokens", iteration))
		}
	}
	return names
}

func TestEngineCatalogCoReaderUsesIntegerSizesAndPreservesLegacyValues(t *testing.T) {
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", ServedModelID: "Qwen-Model", Model: "Qwen-Model",
		Executable: "/opt/fixture/mlx_lm.server", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
		Knobs: &EngineCatalogKnobs{KVContextTokens: "unbounded", PrefillChunkTokens: "2048"},
	}
	data, err := MarshalEngineCatalog(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"qwen": entry}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"kv_context_size = 'unbounded'", "prefill_chunk_size = 2048"} {
		if !strings.Contains(text, want) {
			t.Fatalf("catalog writer omitted co-reader-safe knob %q:\n%s", want, text)
		}
	}
	unboundedPrefill := entry
	unboundedPrefill.Knobs = &EngineCatalogKnobs{KVContextTokens: "32768", PrefillChunkTokens: "unbounded"}
	if _, err := MarshalEngineCatalog(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"qwen": unboundedPrefill}}); !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.knobs.prefill_chunk_size") {
		t.Fatalf("legacy-invalid unbounded prefill was not rejected by the writer: %v", err)
	}
	unboundedWire, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"qwen": unboundedPrefill}})
	if err != nil {
		t.Fatalf("encode raw attack catalog with unbounded prefill: %v", err)
	}
	_, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, unboundedWire), "qwen")
	if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.knobs.prefill_chunk_size") {
		t.Fatalf("legacy-invalid unbounded prefill was admitted by the reader: state=%s err=%v", state, err)
	}
	for _, forbidden := range []string{"kv_context_tokens", "prefill_chunk_tokens"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("catalog writer retained incompatible legacy key %q:\n%s", forbidden, text)
		}
	}
	got, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), "qwen")
	if err != nil || state != EngineCatalogPresent || got.Knobs == nil || got.Knobs.KVContextTokens != "unbounded" || got.Knobs.PrefillChunkTokens != "2048" {
		t.Fatalf("schema v1 knob translation lost legacy values: state=%s entry=%#v err=%v", state, got, err)
	}
}

func TestEngineCatalogMissingServedIDRemainsPresentButNotRunnable(t *testing.T) {
	localWithoutServedID := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "model-id", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	llamaWithoutServedID := EngineCatalogEntry{
		Mode: "local", Engine: "llama-cpp", Executable: "/bin/echo",
		Argv: []string{"serve", "--alias", "legacy-id", "--host", "{host}", "--port", "{port}"},
	}
	sshWithoutServedID := EngineCatalogEntry{
		Mode: "ssh", Engine: "mlx-lm", SSHExecutable: "/usr/bin/ssh", SSHTarget: "model-host",
		RemoteExecutable: "/opt/model-harness", RemoteProfile: "qwen-tiny", RemoteHost: "127.0.0.1", RemotePort: 18012,
	}
	sshProfileNameAsID := sshWithoutServedID
	sshProfileNameAsID.ServedModelID = sshProfileNameAsID.RemoteProfile
	for name, entry := range map[string]EngineCatalogEntry{
		"mlx_lm_missing_id":             localWithoutServedID,
		"llama_missing_id_despite_argv": llamaWithoutServedID,
		"ssh_missing_id":                sshWithoutServedID,
	} {
		t.Run(name, func(t *testing.T) {
			data, err := MarshalEngineCatalog(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"candidate": entry}})
			if err != nil {
				t.Fatal(err)
			}
			got, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), "candidate")
			if state != EngineCatalogPresent || err != nil || got.ServedModelID != "" {
				t.Fatalf("entry without served_model_id was not preserved as catalog data: state=%s entry=%#v err=%v", state, got, err)
			}
			project, home := t.TempDir(), t.TempDir()
			resolved, err := resolveCatalogProfile(project, home, "/tmp", "candidate", got, "engines.toml")
			if err != nil || resolved.NotRunnableCode != "served_model_id_missing" {
				t.Fatalf("entry without served_model_id was not marked non-runnable: resolved=%#v err=%v", resolved, err)
			}
		})
	}
	t.Run("ssh profile name is not served id", func(t *testing.T) {
		data, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"candidate": sshProfileNameAsID}})
		if err != nil {
			t.Fatal(err)
		}
		_, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), "candidate")
		if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.served_model_id") {
			t.Fatalf("SSH profile-name alias was admitted as a served id: state=%s err=%v", state, err)
		}
	})

	for _, invalid := range []string{"a,b", "with spaces", "model=id", " leading-space", "trail/ ", "bad\x00id"} {
		name := invalid
		t.Run("invalid_"+strconv.Quote(name), func(t *testing.T) {
			entry := EngineCatalogEntry{
				Mode: "local", Engine: "llama-cpp", ServedModelID: name, Executable: "/bin/echo",
				Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
			}
			data, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"candidate": entry}})
			if err != nil {
				t.Fatal(err)
			}
			_, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), "candidate")
			if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.served_model_id") {
				t.Fatalf("invalid served model id %q was admitted: state=%s err=%v", name, state, err)
			}
		})
	}
}

func TestEngineCatalogRuntimeArgvReplacesDeclaredIdentityFlags(t *testing.T) {
	llama := EngineCatalogEntry{
		Mode: "local", Engine: "llama-cpp",
		Argv: []string{"serve", "-m", "legacy-model", "-a", "first,second", "--alias=other,alias", "--host", "{host}", "--port", "{port}"},
	}
	got := engineCatalogRuntimeArgv(llama, 50000, "served-model-id")
	if !containsCatalogFlagValue(got, "--alias", "served-model-id") || containsCatalogFlagValue(got, "-a", "first,second") || containsCatalogFlagValue(got, "--alias", "other,alias") {
		t.Fatalf("llama-server argv did not replace old alias with resolved id: %q", got)
	}
	if !containsCatalogFlagValue(got, "-m", "legacy-model") {
		t.Fatalf("llama-server argv lost the legacy -m model selection: %q", got)
	}

	swift := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-swift",
		Argv: []string{"serve", "--model-id", "old-id", "--model-id=stale-id", "--host", "{host}", "--port", "{port}"},
	}
	got = engineCatalogRuntimeArgv(swift, 50000, "served-model-id")
	if !containsCatalogFlagValue(got, "--model-id", "served-model-id") || containsCatalogFlagValue(got, "--model-id", "old-id") || containsCatalogFlagValue(got, "--model-id", "stale-id") || countCatalogFlag(got, "--model-id") != 1 {
		t.Fatalf("mlx-swift argv retained or repeated an old model id: %q", got)
	}
}

func countCatalogFlag(argv []string, flag string) int {
	count := 0
	for _, token := range argv {
		if token == flag || strings.HasPrefix(token, flag+"=") {
			count++
		}
	}
	return count
}

func containsCatalogFlagValue(argv []string, flag, value string) bool {
	for index, token := range argv {
		if token == flag && index+1 < len(argv) && argv[index+1] == value {
			return true
		}
		if token == flag+"="+value {
			return true
		}
	}
	return false
}

func TestEngineCatalogReportsSemanticErrorsOnlyForTheSelectedEntry(t *testing.T) {
	good := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", ServedModelID: "good-model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	bad := good
	bad.Engine = "future-engine"
	path := writeCatalogDocumentFixture(t, map[string]EngineCatalogEntry{"good": good, "bad": bad})
	if got, state, err := ReadEngineCatalogEntry(path, "good"); state != EngineCatalogPresent || err != nil || got.ServedModelID != good.ServedModelID {
		t.Fatalf("unselected semantic failure contaminated good entry: state=%s entry=%#v err=%v", state, got, err)
	}
	if _, state, err := ReadEngineCatalogEntry(path, "bad"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.engine") {
		t.Fatalf("selected semantic failure was not reported for its entry: state=%s err=%v", state, err)
	}
}

func TestEngineCatalogNameGrammarPreservesBoundariesAndRejectsCredentialKeys(t *testing.T) {
	for _, name := range []string{"qwen-local", "mlx_swift", "profile.01", "author", "passthrough"} {
		if !validEngineCatalogName(name) {
			t.Errorf("safe shared-reader name %q was rejected", name)
		}
	}
	for _, name := range []string{"api-key-engine", "context-window-tokens", "authorization-engine", "authtoken", "passphrase", "session-secret", "engine-token-case-000"} {
		if validEngineCatalogName(name) {
			t.Errorf("shared-reader credential-shaped name %q was admitted", name)
		}
	}
}

func TestGeneratedEngineCatalogKnobArgvContainsOnlyPositiveIntegers(t *testing.T) {
	values := []string{"", "unbounded", "0", "1", "2048", "-1", "many", strings.Repeat("9", 100)}
	engines := []string{"mlx-lm", "mlx-swift", "llama-cpp"}
	iterations := 0
	for _, engine := range engines {
		for _, kvValue := range values {
			for _, prefillValue := range values {
				iterations++
				argv := engineCatalogKnobArgv(engine, &EngineCatalogKnobs{
					KVContextTokens: kvValue, PrefillChunkTokens: prefillValue,
				})
				for index := 0; index < len(argv); index += 2 {
					if index+1 >= len(argv) {
						t.Fatalf("engine=%s values=(%q,%q) emitted an unpaired flag: %q", engine, kvValue, prefillValue, argv)
					}
					if argv[index] != "--ctx-size" && argv[index] != "--max-kv-size" && argv[index] != "--ubatch-size" && argv[index] != "--prefill-step-size" {
						t.Fatalf("engine=%s emitted unexpected numeric knob flag in %q", engine, argv)
					}
					number, err := strconv.Atoi(argv[index+1])
					if err != nil || number <= 0 {
						t.Fatalf("engine=%s values=(%q,%q) rendered non-positive-integer knob argv %q", engine, kvValue, prefillValue, argv)
					}
				}
			}
		}
	}
	if iterations != len(engines)*len(values)*len(values) {
		t.Fatalf("knob property covered %d cases, want %d", iterations, len(engines)*len(values)*len(values))
	}
}

func TestGeneratedLegacyModelArgvAcceptanceUsesOnlyLegacyExactLongModelRule(t *testing.T) {
	const seed int64 = 0x5eed2609
	const iterations = 256
	rng := rand.New(rand.NewSource(seed))
	for iteration := 0; iteration < iterations; iteration++ {
		name := fmt.Sprintf("model-argv-%03d", iteration)
		model := ""
		if iteration%2 == 0 {
			model = fmt.Sprintf("org/model-%03d", iteration)
		}
		entry := EngineCatalogEntry{
			Mode: "local", Engine: "llama-cpp", ServedModelID: fmt.Sprintf("served-%03d", iteration),
			Model: model, Executable: "/bin/echo", Argv: generatedLegacyModelArgv(rng, iteration, model == ""),
		}
		document := EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{name: entry}}
		data, err := MarshalEngineCatalog(document)
		if err != nil {
			t.Fatalf("seed=%#x iteration=%d legacy-valid model argv was refused: %v; argv=%q", seed, iteration, err, entry.Argv)
		}
		got, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), name)
		if err != nil || state != EngineCatalogPresent {
			t.Fatalf("seed=%#x iteration=%d reader refused legacy-valid model argv: state=%s err=%v argv=%q", seed, iteration, state, err, entry.Argv)
		}

		port := 51000 + iteration
		wantArgv := expectedCatalogArgvAfterLegacyAliasRemoval(entry, port)
		gotArgv := engineCatalogRuntimeArgv(got, port, got.ServedModelID)
		if !reflect.DeepEqual(gotArgv, wantArgv) {
			t.Fatalf("seed=%#x iteration=%d launch argv changed non-alias legacy tokens:\n got: %q\nwant: %q", seed, iteration, gotArgv, wantArgv)
		}
	}

}

func TestGeneratedLegacyModelFlagFormsRemainAcceptedWithModelField(t *testing.T) {
	const seed int64 = 0x260929
	const iterations = 128
	rng := rand.New(rand.NewSource(seed))
	for iteration := 0; iteration < iterations; iteration++ {
		entry := EngineCatalogEntry{
			Mode: "local", Engine: "llama-cpp", ServedModelID: fmt.Sprintf("served-%03d", iteration),
			Model: fmt.Sprintf("org/model-%03d", iteration), Executable: "/bin/echo",
			Argv: generatedLegacyModelArgv(rng, iteration, false),
		}
		if err := validateEngineCatalogEntry("profile-qwen", entry); err != nil {
			t.Fatalf("seed=%#x iteration=%d catalog refused a legacy argv with a separate model field: %v; argv=%q", seed, iteration, err, entry.Argv)
		}
		port := 51000 + iteration
		launched := engineCatalogRuntimeArgv(entry, port, entry.ServedModelID)
		want := expectedCatalogArgvAfterLegacyAliasRemoval(entry, port)
		if !reflect.DeepEqual(launched, want) {
			t.Fatalf("seed=%#x iteration=%d launcher changed non-alias legacy argv:\n got: %q\nwant: %q", seed, iteration, launched, want)
		}
	}
}

func generatedLegacyModelArgv(rng *rand.Rand, iteration int, noModelField bool) []string {
	modelValues := []string{
		"org/model,revision", "/models/model,variant.gguf", "hf://org/repo", "model-id",
	}
	value := modelValues[rng.Intn(len(modelValues))]
	argv := []string{"serve"}
	switch iteration % 8 {
	case 0:
		argv = append(argv, "-m", value, "-m", "second-model")
	case 1:
		argv = append(argv, "--model="+value, "-m=second-model")
	case 2:
		argv = append(argv, "-hf", "org/repository")
	case 3:
		if noModelField {
			argv = append(argv, "--model", value)
		} else {
			argv = append(argv, "--model="+value)
		}
	case 4:
		argv = append(argv, "-m", value, "--model="+modelValues[(iteration+1)%len(modelValues)])
	case 5:
		argv = append(argv, "-m="+value, "-hf", "org/second-repo")
	case 6:
		if noModelField {
			argv = append(argv, "--model", value, "-m", "second-model")
		} else {
			argv = append(argv, "-m", value, "-m", "second-model")
		}
	default:
		argv = append(argv, "--model="+value, "-m", "second-model", "--no-webui")
	}
	if noModelField && iteration%2 == 1 {
		argv = append(argv, "--alias", "legacy-first,legacy-second")
	} else {
		argv = append(argv, "-a=legacy-first,legacy-second")
	}
	return append(argv, "--host", "{host}", "--port", "{port}")
}

func expectedCatalogArgvAfterLegacyAliasRemoval(entry EngineCatalogEntry, port int) []string {
	argv := make([]string, 0, len(entry.Argv)+4)
	for index := 0; index < len(entry.Argv); index++ {
		token := entry.Argv[index]
		if token == "-a" || token == "--alias" {
			if index+1 < len(entry.Argv) {
				index++
			}
			continue
		}
		if strings.HasPrefix(token, "-a=") || strings.HasPrefix(token, "--alias=") {
			continue
		}
		switch token {
		case "{host}":
			token = engineCatalogDefaultHost
		case "{port}":
			token = strconv.Itoa(port)
		}
		argv = append(argv, token)
	}
	if entry.Model != "" {
		argv = append(argv, "--model", entry.Model)
	}
	return append(argv, "--alias", entry.ServedModelID)
}

func TestEngineCatalogLegacyKnobFixturesMatchModelHarnessAcceptance(t *testing.T) {
	fixture, err := os.ReadFile("testdata/legacy-model-harness-fixtures.toml")
	if err != nil {
		t.Fatal(err)
	}
	var legacy legacyFixtureDocument
	if err := toml.Unmarshal(fixture, &legacy); err != nil {
		t.Fatalf("decode real legacy acceptance fixtures: %v", err)
	}
	legacyResolveCases := map[string]struct {
		engine string
		argv   []string
	}{
		"qwen-default-engine": {
			engine: "mlx-lm",
			argv:   []string{"--max-kv-size", "76800", "--prefill-step-size", "2048", "--chat-template-args", `{"reasoning_effort": "medium"}`},
		},
		"mlx-lm-unbounded": {engine: "mlx-lm"},
		"mlx-swift-knobs": {
			engine: "mlx-swift",
			argv:   []string{"--max-kv-size", "32768", "--prefill-step-size", "2048", "--reasoning-effort", "xhigh"},
		},
		"llama-cpp-knobs": {
			engine: "llama-cpp",
			argv:   []string{"--ctx-size", "8192", "--ubatch-size", "2048", "--reasoning-effort", "low"},
		},
	}
	for name, expected := range legacyResolveCases {
		profile, ok := legacy.Profiles[name]
		if !ok {
			t.Fatalf("legacy fixture lacks the model-harness Resolve case %q", name)
		}
		entry := profile.catalogEntry(name)
		if entry.Engine != expected.engine {
			t.Errorf("profile %q engine=%q want legacy-resolved %q", name, entry.Engine, expected.engine)
		}
		if err := validateEngineCatalogEntry(name, entry); err != nil {
			t.Errorf("legacy model-harness accepted profile %q was refused by catalog v1: %v", name, err)
			continue
		}
		if got := engineCatalogKnobArgv(entry.Engine, entry.Knobs); !reflect.DeepEqual(got, expected.argv) {
			t.Errorf("profile %q translated argv=%q want legacy Resolve argv=%q", name, got, expected.argv)
		}
	}
}

func TestEngineCatalogReaderDistinguishesAbsentMalformedAndReadFailed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if _, state, err := ReadEngineCatalogEntry(missing, "qwen-local"); err != nil || state != EngineCatalogAbsent {
		t.Fatalf("absent catalog state=%s err=%v", state, err)
	}

	valid := "schema_version = 1\n[engines.other]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nserved_model_id = \"remote-model-id\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n"
	path := writeEngineCatalogFixture(t, []byte(valid))
	if _, state, err := ReadEngineCatalogEntry(path, "qwen-local"); err != nil || state != EngineCatalogAbsent {
		t.Fatalf("missing entry state=%s err=%v", state, err)
	}

	malformed := filepath.Join(t.TempDir(), "malformed.toml")
	if err := os.WriteFile(malformed, []byte("schema_version = 1\nnot = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state, err := ReadEngineCatalogEntry(malformed, "qwen-local"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.decode") {
		t.Fatalf("malformed catalog state=%s err=%v", state, err)
	}

	readFailed := writeEngineCatalogFixture(t, []byte(valid))
	t.Run("catalog metadata read failed", func(t *testing.T) {
		previous := lstatEngineCatalogFile
		lstatEngineCatalogFile = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { lstatEngineCatalogFile = previous })
		if _, state, err := ReadEngineCatalogEntry(readFailed, "qwen-local"); state != EngineCatalogReadFailed || !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.read") {
			t.Fatalf("metadata read failure state=%s err=%v", state, err)
		}
	})
	t.Run("catalog open failed", func(t *testing.T) {
		previous := openEngineCatalogFile
		openEngineCatalogFile = func(string) (*os.File, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { openEngineCatalogFile = previous })
		if _, state, err := ReadEngineCatalogEntry(readFailed, "qwen-local"); state != EngineCatalogReadFailed || !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.read") {
			t.Fatalf("open read failure state=%s err=%v", state, err)
		}
	})
	t.Run("catalog descriptor stat failed", func(t *testing.T) {
		previous := statEngineCatalogFile
		statEngineCatalogFile = func(*os.File) (os.FileInfo, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { statEngineCatalogFile = previous })
		if _, state, err := ReadEngineCatalogEntry(readFailed, "qwen-local"); state != EngineCatalogReadFailed || !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.stat") {
			t.Fatalf("descriptor stat failure state=%s err=%v", state, err)
		}
	})
	t.Run("catalog byte read failed", func(t *testing.T) {
		previous := readEngineCatalogFile
		readEngineCatalogFile = func(io.Reader) ([]byte, error) { return nil, os.ErrPermission }
		t.Cleanup(func() { readEngineCatalogFile = previous })
		if _, state, err := ReadEngineCatalogEntry(readFailed, "qwen-local"); state != EngineCatalogReadFailed || !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.read") {
			t.Fatalf("byte read failure state=%s err=%v", state, err)
		}
	})
}

func TestEngineCatalogPathReportsHomeFailuresAsReadFailed(t *testing.T) {
	t.Run("relative home", func(t *testing.T) {
		if _, err := engineCatalogPath("relative-home"); !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.home") {
			t.Fatalf("relative home state err=%v", err)
		}
	})
	t.Run("home lookup failed", func(t *testing.T) {
		previous := userEngineCatalogHomeDir
		userEngineCatalogHomeDir = func() (string, error) { return "", os.ErrPermission }
		t.Cleanup(func() { userEngineCatalogHomeDir = previous })
		if _, err := engineCatalogPath(""); !isEngineCatalogSite(err, EngineCatalogReadFailed, "catalog.home") || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("home lookup failure state err=%v", err)
		}
	})
}

func isEngineCatalogState(err error, want EngineCatalogState) bool {
	var catalogError *EngineCatalogError
	return errors.As(err, &catalogError) && catalogError.State == want
}

func TestEngineCatalogReaderRejectsUnknownKeysAndWrongTypes(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		state EngineCatalogState
		site  string
	}{
		{name: "missing engines table", body: "schema_version = 1\n", site: "catalog.engines"},
		{name: "engines table has wrong type", body: "schema_version = 1\nengines = true\n", site: "catalog.engines"},
		{name: "unknown top-level", body: "schema_version = 1\nunknown = true\n[engines.local]\nmode = \"local\"\n", site: "catalog.keys"},
		{name: "unsupported schema version", body: "schema_version = 2\n[engines.local]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n", site: "catalog.schema_version"},
		{name: "unknown entry member", body: "schema_version = 1\n[engines.local]\nmode = \"local\"\nnew_option = true\n", site: "catalog.keys"},
		{name: "unknown nested member", body: "schema_version = 1\n[engines.local]\nmode = \"local\"\nengine = \"mlx-lm\"\nexecutable = \"/bin/echo\"\nargv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n[engines.local.knobs]\nfuture_knob = \"x\"\n", site: "catalog.keys"},
		{name: "schema version has wrong type", body: "schema_version = \"1\"\n[engines.local]\nmode = \"local\"\n", site: "catalog.decode"},
		{name: "argv has wrong type", body: "schema_version = 1\n[engines.local]\nmode = \"local\"\nengine = \"mlx-lm\"\nexecutable = \"/bin/echo\"\nargv = \"serve\"\n", site: "catalog.decode"},
		{name: "legacy knob key is unknown", body: "schema_version = 1\n[engines.local]\nmode = \"local\"\nengine = \"mlx-lm\"\nexecutable = \"/bin/echo\"\nargv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n[engines.local.knobs]\nkv_context_tokens = 4096\n", site: "catalog.keys"},
		{name: "knob has wrong wire type", body: "schema_version = 1\n[engines.local]\nmode = \"local\"\nengine = \"mlx-lm\"\nexecutable = \"/bin/echo\"\nargv = [\"serve\", \"--host\", \"{host}\", \"--port\", \"{port}\"]\n[engines.local.knobs]\nkv_context_size = true\n", site: "catalog.knobs"},
		{name: "invalid unselected entry", body: "schema_version = 1\n[engines.bad]\nmode = \"local\"\nengine = \"future-engine\"\n[engines.good]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nserved_model_id = \"remote-model-id\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n", state: EngineCatalogAbsent},
		{name: "invalid unselected table key", body: "schema_version = 1\n[engines.\"../invalid\"]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n", site: "catalog.engine_name"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "engines.toml")
			if err := os.WriteFile(path, []byte(testCase.body), 0o600); err != nil {
				t.Fatal(err)
			}
			wantState := testCase.state
			if wantState == "" {
				wantState = EngineCatalogMalformed
			}
			_, state, err := ReadEngineCatalogEntry(path, "local")
			if state != wantState || (testCase.site != "" && !isEngineCatalogSite(err, EngineCatalogMalformed, testCase.site)) || (wantState == EngineCatalogAbsent && err != nil) {
				t.Fatalf("catalog returned the wrong state or refusal site: want=%s/%s state=%s err=%v", wantState, testCase.site, state, err)
			}
		})
	}
}

func TestEngineCatalogReaderRejectsEveryCaseVariantOfEverySchemaKey(t *testing.T) {
	tableTypes := map[string]reflect.Type{
		"document":            reflect.TypeOf(EngineCatalogDocument{}),
		"entry":               reflect.TypeOf(EngineCatalogEntry{}),
		"knobs":               reflect.TypeOf(EngineCatalogKnobs{}),
		"stress":              reflect.TypeOf(EngineCatalogStressPolicy{}),
		"supervision":         reflect.TypeOf(EngineCatalogSupervisionPolicy{}),
		"pinned_distribution": reflect.TypeOf(EngineCatalogPinnedDistribution{}),
	}
	for table, typ := range tableTypes {
		keys := catalogTestTOMLKeys(typ)
		for _, key := range keys {
			for _, variant := range catalogKeyCaseVariants(key) {
				name := table + "/" + key + "/" + variant
				t.Run(name, func(t *testing.T) {
					body := catalogCaseVariantDocument(table, key, variant)
					path := writeEngineCatalogFixture(t, []byte(body))
					_, state, err := ReadEngineCatalogEntry(path, "qwen")
					if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.keys") {
						t.Fatalf("case-variant schema key %q was not rejected by exact-key validation: state=%s err=%v\n%s", variant, state, err, body)
					}
				})
			}
		}
	}
	for _, table := range []string{"knobs", "stress", "supervision", "pinned_distribution"} {
		for _, variant := range catalogKeyCaseVariants(table) {
			t.Run("table-name/"+table+"/"+variant, func(t *testing.T) {
				body := catalogCaseVariantDocument("table-name", table, variant)
				path := writeEngineCatalogFixture(t, []byte(body))
				_, state, err := ReadEngineCatalogEntry(path, "qwen")
				if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.keys") {
					t.Fatalf("case-variant table name %q was not refused: state=%s err=%v", variant, state, err)
				}
			})
		}
	}
}

func catalogTestTOMLKeys(typ reflect.Type) []string {
	var keys []string
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("toml"), ",")[0]
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

func catalogKeyCaseVariants(key string) []string {
	upper := strings.ToUpper(key)
	parts := strings.Split(key, "_")
	for index, part := range parts {
		if part != "" {
			parts[index] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	title := strings.Join(parts, "_")
	mixedBytes := []byte(key)
	upperNext := true
	for index, character := range mixedBytes {
		if character == '_' {
			continue
		}
		if upperNext {
			if character >= 'a' && character <= 'z' {
				mixedBytes[index] = character - ('a' - 'A')
			}
		} else if character >= 'A' && character <= 'Z' {
			mixedBytes[index] = character + ('a' - 'A')
		}
		upperNext = !upperNext
	}
	variants := []string{upper, title, string(mixedBytes)}
	unique := make([]string, 0, len(variants))
	for _, variant := range variants {
		if variant != key && !slices.Contains(unique, variant) {
			unique = append(unique, variant)
		}
	}
	return unique
}

func catalogCaseVariantDocument(table, key, variant string) string {
	entry := catalogCaseVariantBaseEntry(table, key)
	encoded, err := marshalEngineCatalogDocument(EngineCatalogDocument{
		SchemaVersion: EngineCatalogSchemaVersion,
		Engines:       map[string]EngineCatalogEntry{"qwen": entry},
	})
	if err != nil {
		panic(err)
	}
	body := string(encoded)
	switch table {
	case "document":
		if key == "schema_version" {
			return strings.Replace(body, "schema_version = ", variant+" = ", 1)
		}
		return strings.Replace(body, "[engines.qwen]", "["+variant+".qwen]", 1)
	case "entry":
		if slices.Contains([]string{"knobs", "stress", "supervision", "pinned_distribution"}, key) {
			return strings.Replace(body, "[engines.qwen."+key+"]", "[engines.qwen."+variant+"]", 1)
		}
		return strings.Replace(body, "\n"+key+" = ", "\n"+variant+" = ", 1)
	case "knobs", "stress", "supervision", "pinned_distribution":
		return strings.Replace(body, "\n"+key+" = ", "\n"+variant+" = ", 1)
	case "table-name":
		return strings.Replace(body, "[engines.qwen."+key+"]", "[engines.qwen."+variant+"]", 1)
	default:
		panic("unknown engine catalog table " + table)
	}
}

func catalogCaseVariantBaseEntry(table, key string) EngineCatalogEntry {
	ssh := table == "entry" && slices.Contains([]string{
		"ssh_executable", "ssh_target", "remote_executable", "remote_config", "remote_profile", "remote_host", "remote_port",
	}, key)
	if ssh {
		return EngineCatalogEntry{
			Mode: "ssh", Engine: "mlx-lm", ServedModelID: "remote-model-id",
			SSHExecutable: "/usr/bin/ssh", SSHTarget: "model-host", RemoteExecutable: "/opt/model-harness",
			RemoteConfig: "/tmp/remote.toml", RemoteProfile: "qwen", RemoteHost: "127.0.0.1", RemotePort: 18012,
		}
	}
	return EngineCatalogEntry{
		Mode: "local", Engine: "llama-cpp", Model: "/models/qwen", ServedModelID: "qwen-served",
		Executable: "/bin/model-server", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
		Knobs: &EngineCatalogKnobs{
			KVContextTokens: "4096", PrefillChunkTokens: "256", ReasoningEffort: "medium", SpeculativeDecoding: "off",
		},
		Stress: &EngineCatalogStressPolicy{
			PromptTokens: 1024, MaxOutputTokens: 64, StartupTimeoutSeconds: 30, RequestTimeoutSeconds: 120,
			SampleIntervalMilliseconds: 250,
		},
		Supervision: &EngineCatalogSupervisionPolicy{
			FatalOutputSubstrings: []string{"fatal marker"}, RestartOnFailure: true, MaxRestarts: 2,
			RestartWindowSeconds: 60, RestartDelayMilliseconds: 1500,
		},
		PinnedDistribution: &EngineCatalogPinnedDistribution{
			Package: "mlx-lm", SitePackages: "/venv/lib/python3.12/site-packages",
			Commit: "0123456789abcdef0123456789abcdef01234567",
		},
	}
}

func TestEngineCatalogReaderTreatsUnrepresentableRequestedNameAsAbsent(t *testing.T) {
	path := writeEngineCatalogFixture(t, []byte("schema_version = 1\n[engines.remote]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nserved_model_id = \"remote-model-id\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n"))
	for _, name := range []string{"qwen:local", "local/qwen", "qwen 3.8", "_scratch", "qwen@8bit"} {
		if _, state, err := ReadEngineCatalogEntry(path, name); state != EngineCatalogAbsent || err != nil {
			t.Errorf("unrepresentable profile name %q did not remain catalog-absent: state=%s err=%v", name, state, err)
		}
	}
}

func TestEngineCatalogReaderRejectsUnsafeCatalogFileShape(t *testing.T) {
	valid := []byte("schema_version = 1\n[engines.remote]\nmode = \"ssh\"\nengine = \"mlx-lm\"\nserved_model_id = \"remote-model-id\"\nssh_target = \"host\"\nremote_executable = \"/bin/model-harness\"\nremote_profile = \"qwen\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n")
	base := t.TempDir()
	regular := filepath.Join(base, "regular.toml")
	if err := os.WriteFile(regular, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, state, err := ReadEngineCatalogEntry(regular, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.permissions") {
		t.Fatalf("world-readable catalog admitted: state=%s err=%v", state, err)
	}
	ownerUnreadable := filepath.Join(base, "owner-unreadable.toml")
	if err := os.WriteFile(ownerUnreadable, valid, 0o200); err != nil {
		t.Fatal(err)
	}
	if _, state, err := ReadEngineCatalogEntry(ownerUnreadable, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.permissions") {
		t.Fatalf("owner-unreadable catalog admitted: state=%s err=%v", state, err)
	}
	directory := filepath.Join(base, "catalog-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, state, err := ReadEngineCatalogEntry(directory, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.file") {
		t.Fatalf("directory catalog admitted: state=%s err=%v", state, err)
	}

	good := writeEngineCatalogFixture(t, valid)
	symlink := filepath.Join(base, "symlink.toml")
	if err := os.Symlink(good, symlink); err != nil {
		t.Fatal(err)
	}
	if _, state, err := ReadEngineCatalogEntry(symlink, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.file") {
		t.Fatalf("symlink catalog admitted: state=%s err=%v", state, err)
	}

	other := writeEngineCatalogFixture(t, valid)
	previousOpen := openEngineCatalogFile
	t.Cleanup(func() { openEngineCatalogFile = previousOpen })
	openEngineCatalogFile = func(string) (*os.File, error) { return os.Open(other) }
	if _, state, err := ReadEngineCatalogEntry(good, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.file") {
		t.Fatalf("catalog identity replacement admitted: state=%s err=%v", state, err)
	}
	openEngineCatalogFile = previousOpen

	large := filepath.Join(base, "large.toml")
	if err := os.WriteFile(large, append(valid, make([]byte, engineCatalogMaxFileBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	previousOpen = openEngineCatalogFile
	openCalled := false
	openEngineCatalogFile = func(string) (*os.File, error) {
		openCalled = true
		return nil, os.ErrPermission
	}
	_, state, err := ReadEngineCatalogEntry(large, "remote")
	openEngineCatalogFile = previousOpen
	if openCalled || state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.size") {
		t.Fatalf("oversized catalog was not refused before open: opened=%v state=%s err=%v", openCalled, state, err)
	}

	streamGrowth := writeEngineCatalogFixture(t, valid)
	previousRead := readEngineCatalogFile
	t.Cleanup(func() { readEngineCatalogFile = previousRead })
	streamBytes := append([]byte(valid), []byte(strings.Repeat("\n", engineCatalogMaxFileBytes+1-len(valid)))...)
	readEngineCatalogFile = func(io.Reader) ([]byte, error) { return streamBytes, nil }
	if _, state, err := ReadEngineCatalogEntry(streamGrowth, "remote"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "catalog.size") {
		t.Fatalf("catalog growth did not refuse at its byte limit: state=%s err=%v", state, err)
	}
}

func TestEngineCatalogSSHEntryKeepsEngineKindAndLegacyArgv(t *testing.T) {
	entry := EngineCatalogEntry{
		Mode: "ssh", Engine: "mlx-lm", SSHExecutable: "/usr/bin/ssh", SSHTarget: "model-host",
		RemoteExecutable: "/opt/model-harness", RemoteProfile: "qwen-tiny",
		RemoteHost: "127.0.0.1", RemotePort: 18012, ServedModelID: "fixture-remote-model-id",
	}
	data, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{"remote-qwen": entry}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, data), "remote-qwen")
	if err != nil || state != EngineCatalogPresent || parsed.Engine != "mlx-lm" || parsed.Mode != "ssh" {
		t.Fatalf("manager did not parse SSH engine kind: state=%s entry=%#v err=%v", state, parsed, err)
	}
	if got := engineCatalogSSHArgv(parsed, 50000); len(got) == 0 || got[len(got)-1] == "mlx-lm" {
		t.Fatalf("engine kind leaked into or replaced legacy SSH argv: %q", got)
	}

	withoutKind := []byte("schema_version = 1\n[engines.remote-qwen]\nmode = \"ssh\"\nssh_executable = \"/usr/bin/ssh\"\nssh_target = \"model-host\"\nremote_executable = \"/opt/model-harness\"\nremote_profile = \"qwen-tiny\"\nremote_host = \"127.0.0.1\"\nremote_port = 18012\n")
	if _, state, err := ReadEngineCatalogEntry(writeEngineCatalogFixture(t, withoutKind), "remote-qwen"); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.engine") {
		t.Fatalf("SSH catalog without required co-reader kind was accepted: state=%s err=%v", state, err)
	}
}

func isEngineCatalogSite(err error, state EngineCatalogState, site string) bool {
	var catalogError *EngineCatalogError
	return errors.As(err, &catalogError) && catalogError.State == state && catalogError.Site == site
}

func TestEngineCatalogValidationRefusesMalformedEntryMembers(t *testing.T) {
	baseLocal := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "model-id", ServedModelID: "model-id", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	local := func() EngineCatalogEntry {
		entry := baseLocal
		entry.Argv = append([]string(nil), baseLocal.Argv...)
		return entry
	}
	validStress := func() *EngineCatalogStressPolicy {
		return &EngineCatalogStressPolicy{PromptTokens: 1024, MaxOutputTokens: 1, StartupTimeoutSeconds: 1, RequestTimeoutSeconds: 1, SampleIntervalMilliseconds: 50}
	}
	validSupervision := func() *EngineCatalogSupervisionPolicy {
		return &EngineCatalogSupervisionPolicy{FatalOutputSubstrings: []string{"fatal"}, MaxRestarts: 1, RestartWindowSeconds: 1, RestartDelayMilliseconds: 0}
	}
	validPin := func() *EngineCatalogPinnedDistribution {
		return &EngineCatalogPinnedDistribution{Package: "mlx_lm", SitePackages: "/tmp/site-packages", Commit: "0123456789abcdef0123456789abcdef01234567"}
	}
	baseSSH := EngineCatalogEntry{
		Mode: "ssh", Engine: "mlx-lm", SSHExecutable: "/usr/bin/ssh", SSHTarget: "dedicated-mac",
		RemoteExecutable: "/usr/local/bin/model-harness", RemoteConfig: "/tmp/model-harness.toml",
		RemoteProfile: "qwen", RemoteHost: "127.0.0.1", RemotePort: 18012, ServedModelID: "remote-model-id",
	}
	tests := []struct {
		name   string
		entry  EngineCatalogEntry
		engine string
		site   string
	}{
		{name: "invalid engine name", entry: baseLocal, site: "entry.name", engine: "../invalid"},
		{name: "unknown local engine kind", entry: func() EngineCatalogEntry { e := local(); e.Engine = "future"; return e }(), site: "entry.engine"},
		{name: "missing executable", entry: func() EngineCatalogEntry { e := local(); e.Executable = ""; return e }(), site: "entry.executable"},
		{name: "relative executable", entry: func() EngineCatalogEntry { e := local(); e.Executable = "bin/server"; return e }(), site: "entry.executable"},
		{name: "executable with nul", entry: func() EngineCatalogEntry { e := local(); e.Executable = "/bin/server\x00bad"; return e }(), site: "entry.executable"},
		{name: "empty argv", entry: func() EngineCatalogEntry { e := local(); e.Argv = nil; return e }(), site: "entry.endpoint_placeholders"},
		{name: "empty argv token", entry: func() EngineCatalogEntry { e := local(); e.Argv[1] = ""; return e }(), site: "entry.argv"},
		{name: "argv token contains nul", entry: func() EngineCatalogEntry { e := local(); e.Argv[1] = "x\x00y"; return e }(), site: "entry.argv"},
		{name: "missing host placeholder", entry: func() EngineCatalogEntry {
			e := local()
			e.Argv = []string{"serve", "--host", "127.0.0.1", "--port", "{port}"}
			return e
		}(), site: "entry.endpoint_placeholders"},
		{name: "missing port placeholder", entry: func() EngineCatalogEntry {
			e := local()
			e.Argv = []string{"serve", "--host", "{host}", "--port", "18011"}
			return e
		}(), site: "entry.endpoint_placeholders"},
		{name: "duplicate host placeholder", entry: func() EngineCatalogEntry { e := local(); e.Argv = append(e.Argv, "{host}"); return e }(), site: "entry.endpoint_placeholders"},
		{name: "local SSH executable field", entry: func() EngineCatalogEntry { e := local(); e.SSHExecutable = "/usr/bin/ssh"; return e }(), site: "entry.locality"},
		{name: "local SSH target field", entry: func() EngineCatalogEntry { e := local(); e.SSHTarget = "host"; return e }(), site: "entry.locality"},
		{name: "local remote executable field", entry: func() EngineCatalogEntry { e := local(); e.RemoteExecutable = "/bin/harness"; return e }(), site: "entry.locality"},
		{name: "local remote config field", entry: func() EngineCatalogEntry { e := local(); e.RemoteConfig = "/tmp/config"; return e }(), site: "entry.locality"},
		{name: "local remote profile field", entry: func() EngineCatalogEntry { e := local(); e.RemoteProfile = "remote"; return e }(), site: "entry.locality"},
		{name: "local remote host field", entry: func() EngineCatalogEntry { e := local(); e.RemoteHost = engineCatalogDefaultHost; return e }(), site: "entry.locality"},
		{name: "local remote port field", entry: func() EngineCatalogEntry { e := local(); e.RemotePort = 18012; return e }(), site: "entry.locality"},
		{name: "unsupported mode", entry: func() EngineCatalogEntry { e := local(); e.Mode = "container"; return e }(), site: "entry.mode"},
		{name: "SSH missing engine kind", entry: func() EngineCatalogEntry { e := baseSSH; e.Engine = ""; return e }(), site: "entry.engine"},
		{name: "SSH model member", entry: func() EngineCatalogEntry { e := baseSSH; e.Model = "model"; return e }(), site: "entry.ssh_identity"},
		{name: "SSH knobs member", entry: func() EngineCatalogEntry {
			e := baseSSH
			e.Knobs = &EngineCatalogKnobs{KVContextTokens: "4096"}
			return e
		}(), site: "entry.ssh_identity"},
		{name: "SSH executable member", entry: func() EngineCatalogEntry { e := baseSSH; e.Executable = "/bin/echo"; return e }(), site: "entry.ssh_argv"},
		{name: "SSH argv member", entry: func() EngineCatalogEntry { e := baseSSH; e.Argv = []string{"run"}; return e }(), site: "entry.ssh_argv"},
		{name: "missing SSH target", entry: func() EngineCatalogEntry { e := baseSSH; e.SSHTarget = ""; return e }(), site: "entry.ssh_target"},
		{name: "invalid SSH target", entry: func() EngineCatalogEntry { e := baseSSH; e.SSHTarget = "host with spaces"; return e }(), site: "entry.ssh_target"},
		{name: "relative SSH executable", entry: func() EngineCatalogEntry { e := baseSSH; e.SSHExecutable = "ssh"; return e }(), site: "entry.ssh_executable"},
		{name: "SSH executable with nul", entry: func() EngineCatalogEntry { e := baseSSH; e.SSHExecutable = "/bin/ssh\x00x"; return e }(), site: "entry.ssh_executable"},
		{name: "missing remote executable", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteExecutable = ""; return e }(), site: "entry.remote_executable"},
		{name: "relative remote executable", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteExecutable = "model-harness"; return e }(), site: "entry.remote_executable"},
		{name: "remote executable with nul", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteExecutable = "/bin/harness\x00x"; return e }(), site: "entry.remote_executable"},
		{name: "relative remote config", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteConfig = "config.toml"; return e }(), site: "entry.remote_config"},
		{name: "missing remote profile", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteProfile = ""; return e }(), site: "entry.remote_profile"},
		{name: "invalid remote profile", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteProfile = "bad profile"; return e }(), site: "entry.remote_profile"},
		{name: "non-loopback remote host", entry: func() EngineCatalogEntry { e := baseSSH; e.RemoteHost = "0.0.0.0"; return e }(), site: "entry.remote_host"},
		{name: "remote port below range", entry: func() EngineCatalogEntry { e := baseSSH; e.RemotePort = 0; return e }(), site: "entry.remote_port"},
		{name: "remote port above range", entry: func() EngineCatalogEntry { e := baseSSH; e.RemotePort = 65536; return e }(), site: "entry.remote_port"},
		{name: "SSH stress policy", entry: func() EngineCatalogEntry { e := baseSSH; e.Stress = validStress(); return e }(), site: "entry.ssh_local_policy"},
		{name: "SSH supervision policy", entry: func() EngineCatalogEntry { e := baseSSH; e.Supervision = validSupervision(); return e }(), site: "entry.ssh_local_policy"},
		{name: "SSH pinned distribution", entry: func() EngineCatalogEntry { e := baseSSH; e.PinnedDistribution = validPin(); return e }(), site: "entry.ssh_local_policy"},
		{name: "KV tokens are not numeric", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{KVContextTokens: "many"}
			return e
		}(), site: "catalog.knobs"},
		{name: "KV tokens overflow positive integer", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{KVContextTokens: strings.Repeat("9", 100)}
			return e
		}(), site: "catalog.knobs"},
		{name: "KV tokens are not positive", entry: func() EngineCatalogEntry { e := local(); e.Knobs = &EngineCatalogKnobs{KVContextTokens: "0"}; return e }(), site: "entry.knobs.kv_context_size"},
		{name: "unbounded llama context", entry: func() EngineCatalogEntry {
			e := local()
			e.Engine = "llama-cpp"
			e.Knobs = &EngineCatalogKnobs{KVContextTokens: "unbounded"}
			return e
		}(), site: "entry.knobs.kv_context_size"},
		{name: "prefill tokens are not numeric", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{PrefillChunkTokens: "many"}
			return e
		}(), site: "catalog.knobs"},
		{name: "prefill tokens overflow positive integer", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{PrefillChunkTokens: strings.Repeat("9", 100)}
			return e
		}(), site: "catalog.knobs"},
		{name: "prefill tokens are not positive", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{PrefillChunkTokens: "0"}
			return e
		}(), site: "entry.knobs.prefill_chunk_size"},
		{name: "unsupported reasoning effort", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{ReasoningEffort: "high"}
			return e
		}(), site: "entry.knobs.reasoning_effort"},
		{name: "speculation with unsupported engine", entry: func() EngineCatalogEntry {
			e := local()
			e.Knobs = &EngineCatalogKnobs{SpeculativeDecoding: "ngram"}
			return e
		}(), site: "entry.knobs.speculative_decoding"},
		{name: "unsupported speculation mode", entry: func() EngineCatalogEntry {
			e := local()
			e.Engine = "llama-cpp"
			e.Knobs = &EngineCatalogKnobs{SpeculativeDecoding: "draft"}
			return e
		}(), site: "entry.knobs.speculative_decoding"},
		{name: "stress prompt below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.PromptTokens = 1023
			return e
		}(), site: "entry.stress.prompt_tokens"},
		{name: "stress prompt above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.PromptTokens = 1000001
			return e
		}(), site: "entry.stress.prompt_tokens"},
		{name: "stress output below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.MaxOutputTokens = 0
			return e
		}(), site: "entry.stress.max_output_tokens"},
		{name: "stress output above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.MaxOutputTokens = 4097
			return e
		}(), site: "entry.stress.max_output_tokens"},
		{name: "stress startup timeout below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.StartupTimeoutSeconds = 0
			return e
		}(), site: "entry.stress.startup_timeout_seconds"},
		{name: "stress startup timeout above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.StartupTimeoutSeconds = 3601
			return e
		}(), site: "entry.stress.startup_timeout_seconds"},
		{name: "stress request timeout below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.RequestTimeoutSeconds = 0
			return e
		}(), site: "entry.stress.request_timeout_seconds"},
		{name: "stress request timeout above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.RequestTimeoutSeconds = 86401
			return e
		}(), site: "entry.stress.request_timeout_seconds"},
		{name: "stress sample interval below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.SampleIntervalMilliseconds = 49
			return e
		}(), site: "entry.stress.sample_interval_milliseconds"},
		{name: "stress sample interval above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Stress = validStress()
			e.Stress.SampleIntervalMilliseconds = 10001
			return e
		}(), site: "entry.stress.sample_interval_milliseconds"},
		{name: "no fatal markers", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.FatalOutputSubstrings = nil
			return e
		}(), site: "entry.supervision.fatal_output_substrings"},
		{name: "too many fatal markers", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.FatalOutputSubstrings = make([]string, 17)
			for i := range e.Supervision.FatalOutputSubstrings {
				e.Supervision.FatalOutputSubstrings[i] = "fatal"
			}
			return e
		}(), site: "entry.supervision.fatal_output_substrings"},
		{name: "empty fatal marker", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.FatalOutputSubstrings = []string{""}
			return e
		}(), site: "entry.supervision.fatal_output_substrings"},
		{name: "long fatal marker", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.FatalOutputSubstrings = []string{strings.Repeat("x", 513)}
			return e
		}(), site: "entry.supervision.fatal_output_substrings"},
		{name: "fatal marker with nul", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.FatalOutputSubstrings = []string{"bad\x00marker"}
			return e
		}(), site: "entry.supervision.fatal_output_substrings"},
		{name: "restart count below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.MaxRestarts = 0
			return e
		}(), site: "entry.supervision.max_restarts"},
		{name: "restart count above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.MaxRestarts = 101
			return e
		}(), site: "entry.supervision.max_restarts"},
		{name: "restart window below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.RestartWindowSeconds = 0
			return e
		}(), site: "entry.supervision.restart_window_seconds"},
		{name: "restart window above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.RestartWindowSeconds = 86401
			return e
		}(), site: "entry.supervision.restart_window_seconds"},
		{name: "restart delay below range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.RestartDelayMilliseconds = -1
			return e
		}(), site: "entry.supervision.restart_delay_milliseconds"},
		{name: "restart delay above range", entry: func() EngineCatalogEntry {
			e := local()
			e.Supervision = validSupervision()
			e.Supervision.RestartDelayMilliseconds = 60001
			return e
		}(), site: "entry.supervision.restart_delay_milliseconds"},
		{name: "invalid pinned package", entry: func() EngineCatalogEntry {
			e := local()
			e.PinnedDistribution = validPin()
			e.PinnedDistribution.Package = "bad package"
			return e
		}(), site: "entry.pinned_distribution.package"},
		{name: "relative pinned site packages", entry: func() EngineCatalogEntry {
			e := local()
			e.PinnedDistribution = validPin()
			e.PinnedDistribution.SitePackages = "site-packages"
			return e
		}(), site: "entry.pinned_distribution.site_packages"},
		{name: "pinned site packages with nul", entry: func() EngineCatalogEntry {
			e := local()
			e.PinnedDistribution = validPin()
			e.PinnedDistribution.SitePackages = "/tmp/site\x00packages"
			return e
		}(), site: "entry.pinned_distribution.site_packages"},
		{name: "invalid pinned commit", entry: func() EngineCatalogEntry {
			e := local()
			e.PinnedDistribution = validPin()
			e.PinnedDistribution.Commit = strings.Repeat("A", 40)
			return e
		}(), site: "entry.pinned_distribution.commit"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			name := testCase.engine
			if name == "" {
				name = "engine"
			}
			if testCase.site == "entry.name" {
				name = "../invalid"
				testCase.site = "catalog.engine_name"
			}
			path, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: map[string]EngineCatalogEntry{name: testCase.entry}})
			if err != nil {
				t.Fatal(err)
			}
			catalogPath := writeEngineCatalogFixture(t, path)
			if _, state, err := ReadEngineCatalogEntry(catalogPath, name); state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, testCase.site) {
				t.Fatalf("malformed entry admitted or wrong refusal site: want=%s state=%s err=%v", testCase.site, state, err)
			}
		})
	}
}

func TestEngineCatalogReaderRefusesEmbeddedEndpointPlaceholder(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "host", value: "--url=http://{host}"},
		{name: "port", value: "--url=http://127.0.0.1:{port}"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			entry := EngineCatalogEntry{
				Mode: "local", Engine: "mlx-lm", Executable: "/bin/echo",
				Argv: []string{"serve", "--host", "{host}", "--port", "{port}", testCase.value},
			}
			path := writeCatalogDocumentFixture(t, map[string]EngineCatalogEntry{"embedded": entry})
			_, state, err := ReadEngineCatalogEntry(path, "embedded")
			if state != EngineCatalogMalformed || !isEngineCatalogSite(err, EngineCatalogMalformed, "entry.endpoint_placeholders") {
				t.Fatalf("embedded endpoint placeholder %q was expanded instead of refused: state=%s err=%v", testCase.value, state, err)
			}
		})
	}
}

func TestEngineCatalogAcceptsLegacySpeculativeDecodingOff(t *testing.T) {
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "llama-cpp", ServedModelID: "off-model-id", Executable: "/bin/echo",
		Argv:  []string{"serve", "--host", "{host}", "--port", "{port}"},
		Knobs: &EngineCatalogKnobs{SpeculativeDecoding: "off"},
	}
	path := writeCatalogDocumentFixture(t, map[string]EngineCatalogEntry{"llama-off": entry})
	got, state, err := ReadEngineCatalogEntry(path, "llama-off")
	if state != EngineCatalogPresent || err != nil {
		t.Fatalf("legacy-valid speculative_decoding=off was refused: state=%s err=%v", state, err)
	}
	if argv := engineCatalogKnobArgv(got.Engine, got.Knobs); len(argv) != 0 {
		t.Fatalf("speculative_decoding=off must emit no argv, got %q", argv)
	}
}

func TestEngineCatalogSSHArgvMatchesLegacyModelHarnessGolden(t *testing.T) {
	entry := EngineCatalogEntry{
		Mode: "ssh", Engine: "mlx-lm", SSHExecutable: "/usr/bin/ssh", SSHTarget: "dedicated-mac",
		RemoteExecutable: "/opt/model-harness", RemoteConfig: "/tmp/operator's config.toml",
		RemoteProfile: "qwen-tiny", RemoteHost: "127.0.0.1", RemotePort: 18012,
	}
	got := engineCatalogSSHArgv(entry, 50000)
	want := []string{
		"-tt", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-L", "127.0.0.1:50000:127.0.0.1:18012", "dedicated-mac",
		"'/opt/model-harness' 'run' 'qwen-tiny' '--config' '/tmp/operator'\"'\"'s config.toml' '--host' '127.0.0.1' '--port' '18012'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSH argv differs from model-harness config.go legacy plan:\n got: %#v\nwant: %#v", got, want)
	}
}

func writeEngineCatalogFixture(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engines.toml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCatalogDocumentFixture(t *testing.T, entries map[string]EngineCatalogEntry) string {
	t.Helper()
	data, err := marshalEngineCatalogDocument(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: entries})
	if err != nil {
		t.Fatalf("encode catalog fixture: %v", err)
	}
	return writeEngineCatalogFixture(t, data)
}
