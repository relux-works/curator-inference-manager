//go:build !windows

package infra

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResolveSharedProfileUsesCatalogWithoutAgentsInfraPiConfig(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "Qwen3-local",
		Executable: "/bin/echo", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"qwen-local": entry})
	resolved, err := resolveSharedProfile(project, home, cache, "qwen-local")
	if err != nil {
		t.Fatalf("catalog resolution required legacy agents-infra config: %v", err)
	}
	if !resolved.CatalogEntry || resolved.CatalogMode != "local" || resolved.Profile.Model != entry.Model || resolved.Profile.Runtime.Executable != entry.Executable {
		t.Fatalf("catalog entry was not selected: %#v", resolved)
	}
}

func TestResolveSharedProfileUsesCatalogForSelectedPrimaryProfile(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "Model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"selected": entry})
	port := catalogEndpointPort("selected", entry)
	argv := []string{"serve", "--host", engineCatalogDefaultHost, "--port", strconv.Itoa(port), "--model", "Model"}
	writePiProjectConfig(t, project, validPiProfileWithArgv(t, "selected", "/bin/echo", port, argv, 5))

	resolved, err := resolveSharedProfile(project, home, cache, "")
	if err != nil {
		t.Fatalf("selected primary profile did not resolve through its agreeing catalog entry: %v", err)
	}
	if !resolved.CatalogEntry || resolved.ProfileName != "selected" || resolved.Profile.Model != entry.Model {
		t.Fatalf("selected primary profile resolved without the catalog entry: %#v", resolved)
	}
}

func TestResolveSharedProfileFallsBackToLegacyOnlyWhenEntryIsAbsent(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{
		"other": {Mode: "ssh", Engine: "mlx-lm", SSHTarget: "host", RemoteExecutable: "/bin/model-harness", RemoteProfile: "other", RemoteHost: engineCatalogDefaultHost, RemotePort: 18012},
	})
	writePiProjectConfig(t, project, sharedPiProfileTOML("legacy-only", "/bin/echo", 18011))
	resolved, err := resolveSharedProfile(project, home, cache, "legacy-only")
	if err != nil {
		t.Fatalf("absent catalog name did not use compatibility reader: %v", err)
	}
	if resolved.CatalogEntry || resolved.ProfileName != "legacy-only" || resolved.Profile.Model != "Model" {
		t.Fatalf("legacy compatibility result=%#v", resolved)
	}
}

func TestResolveSharedProfileAllowsLegacyNamesOutsideCatalogGrammarOnEverySelectionPath(t *testing.T) {
	profileNames := []string{"qwen:local", "local/qwen", "qwen 3.8", "_scratch", "qwen@8bit"}
	for index, name := range profileNames {
		for _, selection := range []struct {
			name string
			arg  string
		}{
			{name: "explicit_engine", arg: name},
			{name: "primary_session", arg: ""},
		} {
			t.Run(name+"/"+selection.name, func(t *testing.T) {
				project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
				writePiProjectConfig(t, project, sharedPiProfileTOML(name, "/bin/echo", 18011+index))
				resolved, err := resolveSharedProfile(project, home, cache, selection.arg)
				if err != nil {
					t.Fatalf("legacy profile was blocked by catalog name grammar: %v", err)
				}
				if resolved.CatalogEntry || resolved.ProfileName != name || resolved.Profile.Model != "Model" {
					t.Fatalf("legacy compatibility result=%#v", resolved)
				}
			})
		}
	}
}

func TestResolveSharedProfileDoesNotFallBackAfterCatalogReadFailure(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	writePiProjectConfig(t, project, sharedPiProfileTOML("same-name", "/bin/echo", 18011))
	path := filepath.Join(home, ".curator", EngineCatalogFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("schema_version = 1\n[engines.same-name]\nmode = \"ssh\""), 0o600); err != nil {
		t.Fatal(err)
	}
	previousOpen := openEngineCatalogFile
	openEngineCatalogFile = func(string) (*os.File, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { openEngineCatalogFile = previousOpen })
	_, err := resolveSharedProfile(project, home, cache, "same-name")
	var catalogError *EngineCatalogError
	if !errors.As(err, &catalogError) || catalogError.State != EngineCatalogReadFailed {
		t.Fatalf("catalog read failure fell back or lost its state: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesDisagreeingCatalogAndLegacyIdentity(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"pair": entry})
	port := catalogEndpointPort("pair", entry)
	argv := []string{"serve", "--host", engineCatalogDefaultHost, "--port", strconv.Itoa(port)}
	writePiProjectConfig(t, project, validPiProfileWithArgv(t, "pair", "/bin/echo", port, argv, 5))
	_, err := resolveSharedProfile(project, home, cache, "pair")
	var catalogError *EngineCatalogError
	if !errors.As(err, &catalogError) || catalogError.State != EngineCatalogConflict || catalogError.Site != "resolution.identity_argv" {
		t.Fatalf("disagreeing legacy/catalog pair admitted: %T %v", err, err)
	}
}

func TestResolveSharedProfileAppliesCatalogConflictToSelectedPrimaryProfile(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "catalog-model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"selected": entry})
	writePiProjectConfig(t, project, validPiProfileTOML("selected", "/bin/echo", 18011, false))
	_, err := resolveSharedProfile(project, home, cache, "")
	if !isEngineCatalogSite(err, EngineCatalogConflict, "resolution.identity_argv") {
		t.Fatalf("selected primary profile bypassed the disagreeing catalog entry: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesMalformedProjectConfiguration(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	writePiProjectConfig(t, project, "[agents\n")
	_, err := resolveSharedProfile(project, home, cache, "")
	if sharedRuntimeErrorCode(err) != "invalid_project_configuration" {
		t.Fatalf("malformed primary project configuration was not refused: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesMalformedLegacyProfileDuringCatalogConflictCheck(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{
		"broken": {Mode: "local", Engine: "mlx-lm", Model: "Model", Executable: "/bin/echo", Argv: []string{"serve", "--host", "{host}", "--port", "{port}"}},
	})
	writePiProjectConfig(t, project, `agents = "wrong-table-type"`)
	_, err := resolveSharedProfile(project, home, cache, "broken")
	if sharedRuntimeErrorCode(err) != "invalid_project_configuration" {
		t.Fatalf("malformed legacy compatibility profile was not refused: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesLegacyPiIdentityReadAndShapeFailures(t *testing.T) {
	runtimeArgv := `argv = ["serve", "--host", "127.0.0.1", "--port", "18011"]`
	cases := []struct {
		name        string
		body        string
		readErr     error
		wantMessage string
	}{
		{name: "compatibility read failed", readErr: os.ErrPermission, wantMessage: "read project config for compatibility check"},
		{name: "compatibility TOML malformed", body: "[agents\n", wantMessage: "parse project config for compatibility check"},
		{name: "agents table wrong type", body: `agents = "wrong-table-type"`, wantMessage: "expected table for agents"},
		{name: "Pi table wrong type", body: "[agents]\npi = \"wrong-table-type\"\n", wantMessage: "expected table for agents.pi"},
		{name: "profiles table wrong type", body: "[agents.pi]\nprofiles = \"wrong-table-type\"\n", wantMessage: "expected table for agents.pi.profiles"},
		{name: "profile table wrong type", body: "[agents.pi.profiles]\npair = 7\n", wantMessage: "agents.pi.profiles.pair must be a table"},
		{name: "model wrong type", body: "[agents.pi.profiles.pair]\nmodel = 7\n[agents.pi.profiles.pair.runtime]\nexecutable = \"/bin/echo\"\n" + runtimeArgv + "\n", wantMessage: "agents.pi.profiles.pair.model must be a string"},
		{name: "model empty", body: "[agents.pi.profiles.pair]\nmodel = \"\"\n[agents.pi.profiles.pair.runtime]\nexecutable = \"/bin/echo\"\n" + runtimeArgv + "\n", wantMessage: "agents.pi.profiles.pair.model must not be empty"},
		{name: "runtime table absent", body: "[agents.pi.profiles.pair]\nmodel = \"Model\"\n", wantMessage: "runtime table is absent"},
		{name: "runtime table wrong type", body: "[agents.pi.profiles.pair]\nmodel = \"Model\"\nruntime = \"wrong-table-type\"\n", wantMessage: "expected table for agents.pi.profiles.pair.runtime"},
		{name: "executable wrong type", body: "[agents.pi.profiles.pair]\nmodel = \"Model\"\n[agents.pi.profiles.pair.runtime]\nexecutable = 7\n" + runtimeArgv + "\n", wantMessage: "agents.pi.profiles.pair.runtime.executable must be a string"},
		{name: "argv wrong type", body: "[agents.pi.profiles.pair]\nmodel = \"Model\"\n[agents.pi.profiles.pair.runtime]\nexecutable = \"/bin/echo\"\nargv = \"wrong-array-type\"\n", wantMessage: "agents.pi.profiles.pair.runtime.argv must be an array"},
		{name: "argv element wrong type", body: "[agents.pi.profiles.pair]\nmodel = \"Model\"\n[agents.pi.profiles.pair.runtime]\nexecutable = \"/bin/echo\"\nargv = [7]\n", wantMessage: "agents.pi.profiles.pair.runtime.argv[0] must be a string"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
			writePiProjectConfig(t, project, sharedPiProfileTOML("pair", "/bin/echo", 18011))
			entry := EngineCatalogEntry{
				Mode: "local", Engine: "mlx-lm", Model: "Model", Executable: "/bin/echo",
				Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
			}
			writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"pair": entry})
			canonicalProject, err := CanonicalProjectDir(project)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(canonicalProject, ".agents", ".configs", projectConfigFileName)
			previous := readLegacyPiIdentityConfigFile
			called := false
			readLegacyPiIdentityConfigFile = func(path string) ([]byte, error) {
				if samePath(path, configPath) {
					called = true
					return []byte(testCase.body), testCase.readErr
				}
				return os.ReadFile(path)
			}
			t.Cleanup(func() { readLegacyPiIdentityConfigFile = previous })
			_, err = resolveSharedProfile(project, home, cache, "pair")
			if !called {
				t.Fatalf("production catalog resolution did not read the legacy Pi identity; err=%v", err)
			}
			if sharedRuntimeErrorCode(err) != "invalid_project_configuration" || !strings.Contains(err.Error(), testCase.wantMessage) {
				t.Fatalf("legacy identity failure code=%q want=invalid_project_configuration detail want %q, err=%v", sharedRuntimeErrorCode(err), testCase.wantMessage, err)
			}
		})
	}
}

func TestResolveSharedProfileRefusesMissingPrimarySelection(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	_, err := resolveSharedProfile(project, home, cache, "")
	if sharedRuntimeErrorCode(err) != "shared_runtime_not_configured" {
		t.Fatalf("missing primary selection was not refused: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesUnknownLegacyProfile(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	writePiProjectConfig(t, project, validPiProfileTOML("known", "/bin/echo", 18011, false))
	_, err := resolveSharedProfile(project, home, cache, "unknown")
	if sharedRuntimeErrorCode(err) != "unknown_pi_profile" {
		t.Fatalf("unknown compatibility profile was not refused: %T %v", err, err)
	}
}

func TestResolveSharedProfileRefusesLegacyProfileWithoutSharedRuntime(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "sharing table absent", body: validPiProfileTOML("not-shared", "/bin/echo", 18011, false)},
		{name: "sharing mode exclusive", body: strings.Replace(sharedPiProfileTOML("not-shared", "/bin/echo", 18011), `mode = "shared"`, `mode = "exclusive"`, 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
			writePiProjectConfig(t, project, testCase.body)
			_, err := resolveSharedProfile(project, home, cache, "not-shared")
			if sharedRuntimeErrorCode(err) != "shared_runtime_not_configured" {
				t.Fatalf("profile without shared runtime was not refused: %T %v", err, err)
			}
		})
	}
}

func TestResolveSharedProfileRefusesLegacyConfigurationReadAndShapeFailures(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		dir  bool
	}{
		{name: "configuration read failed", dir: true},
		{name: "agents table wrong type", body: "agents = true\n"},
		{name: "Pi primary profile wrong type", body: "[agents.pi.primary_session]\nprofile = 7\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
			configPath := filepath.Join(project, ".agents", ".configs", projectConfigFileName)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if testCase.dir {
				if err := os.Mkdir(configPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(configPath, []byte(testCase.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := resolveSharedProfile(project, home, cache, "profile")
			if sharedRuntimeErrorCode(err) != "invalid_project_configuration" {
				t.Fatalf("legacy configuration failure code=%q err=%v", sharedRuntimeErrorCode(err), err)
			}
		})
	}
}

func TestResolveSharedProfileRefusesPairDifferingOnlyByExecutable(t *testing.T) {
	testResolveCatalogPairMismatchOnly(t, func(entry *EngineCatalogEntry, _ int) {
		entry.Executable = "/bin/true"
	})
}

func TestResolveSharedProfileRefusesPairDifferingOnlyByArgv(t *testing.T) {
	testResolveCatalogPairMismatchOnly(t, func(entry *EngineCatalogEntry, _ int) {
		entry.Argv = append(entry.Argv, "--catalog-only")
	})
}

func TestResolveSharedProfileRefusesPrimaryPairDifferingOnlyByArgv(t *testing.T) {
	testResolveCatalogPairMismatchOnlyAtSelection(t, "", func(entry *EngineCatalogEntry, _ int) {
		entry.Argv = append(entry.Argv, "--catalog-only")
	})
}

func testResolveCatalogPairMismatchOnly(t *testing.T, change func(*EngineCatalogEntry, int)) {
	testResolveCatalogPairMismatchOnlyAtSelection(t, "pair", change)
}

func testResolveCatalogPairMismatchOnlyAtSelection(t *testing.T, selectedName string, change func(*EngineCatalogEntry, int)) {
	t.Helper()
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "Model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	change(&entry, 0)
	port := catalogEndpointPort("pair", entry)
	argv := []string{"serve", "--host", engineCatalogDefaultHost, "--port", strconv.Itoa(port), "--model", "Model"}
	writePiProjectConfig(t, project, validPiProfileWithArgv(t, "pair", "/bin/echo", port, argv, 5))
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"pair": entry})
	_, err := resolveSharedProfile(project, home, cache, selectedName)
	if !isEngineCatalogState(err, EngineCatalogConflict) {
		t.Fatalf("pair differing only in executable/argv was admitted: %T %v", err, err)
	}
}

func TestResolveSharedProfileAcceptsAgreeingCatalogAndLegacyIdentity(t *testing.T) {
	project, home, cache := t.TempDir(), t.TempDir(), shortEngineCatalogCache(t)
	entry := EngineCatalogEntry{
		Mode: "local", Engine: "mlx-lm", Model: "Model", Executable: "/bin/echo",
		Argv: []string{"serve", "--host", "{host}", "--port", "{port}"},
	}
	writeEngineCatalog(t, home, map[string]EngineCatalogEntry{"pair": entry})
	port := catalogEndpointPort("pair", entry)
	argv := []string{"serve", "--host", engineCatalogDefaultHost, "--port", strconv.Itoa(port), "--model", "Model"}
	writePiProjectConfig(t, project, validPiProfileWithArgv(t, "pair", "/bin/echo", port, argv, 5))
	resolved, err := resolveSharedProfile(project, home, cache, "pair")
	if err != nil {
		t.Fatalf("agreeing catalog and legacy identity conflicted: %v", err)
	}
	if !resolved.CatalogEntry || resolved.Profile.Model != entry.Model || !equalStrings(resolved.Profile.Runtime.Argv, argv) {
		t.Fatalf("catalog-first agreeing result=%#v", resolved)
	}
}

func TestReadPiProfilesRejectsMalformedAndUnreadableConfiguration(t *testing.T) {
	cases := []struct {
		name string
		body string
		dir  bool
	}{
		{name: "read failed", dir: true},
		{name: "malformed TOML", body: "[agents\n"},
		{name: "agents has wrong type", body: "agents = true\n"},
		{name: "Pi profile has wrong type", body: "[agents.pi.profiles.broken]\nmodel = 7\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			configPath := filepath.Join(project, ".agents", ".configs", projectConfigFileName)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if testCase.dir {
				if err := os.Mkdir(configPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(configPath, []byte(testCase.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readPiProfiles(project, home); sharedRuntimeErrorCode(err) != "invalid_project_configuration" {
				t.Fatalf("readPiProfiles accepted malformed configuration: %T %v", err, err)
			}
		})
	}
}

func shortEngineCatalogCache(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ecc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func writeEngineCatalog(t *testing.T, home string, entries map[string]EngineCatalogEntry) string {
	t.Helper()
	path := filepath.Join(home, ".curator", EngineCatalogFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	withTestServedModelIDs := make(map[string]EngineCatalogEntry, len(entries))
	for name, entry := range entries {
		if entry.ServedModelID == "" {
			if entry.Mode == "ssh" {
				entry.ServedModelID = "fixture-remote-served-model"
			} else if modelID, ok := engineCatalogServedModelID(entry); ok {
				entry.ServedModelID = modelID
			} else if entry.Model != "" {
				entry.ServedModelID = entry.Model
			} else {
				entry.ServedModelID = "fixture-served-model-" + name
			}
		}
		withTestServedModelIDs[name] = entry
	}
	data, err := MarshalEngineCatalog(EngineCatalogDocument{SchemaVersion: EngineCatalogSchemaVersion, Engines: withTestServedModelIDs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
