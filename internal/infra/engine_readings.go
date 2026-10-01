package infra

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/skill-agents-management/pkg/inferenceengine"
	"github.com/relux-works/skill-agents-management/pkg/plugin"
)

// EngineReadings is the additive public status observation, independent of the
// status and socket versions. Values are raw inputs to the pinned validator.
type EngineReadings struct {
	ContractVersion string          `json:"contract_version"`
	Facts           []EngineReading `json:"facts"`
}

type EngineReading struct {
	Fact    inferenceengine.Fact             `json:"fact"`
	Outcome inferenceengine.Outcome          `json:"outcome"`
	Value   string                           `json:"value,omitempty"`
	Cause   inferenceengine.NotObservedCause `json:"cause,omitempty"`
	Reason  string                           `json:"reason,omitempty"`
	Source  string                           `json:"source"`
}

func profileEngineReadings(resolved sharedResolvedProfile) (EngineReadings, error) {
	values := map[inferenceengine.Fact]any{}
	absent := map[inferenceengine.Fact]bool{}
	degraded := map[inferenceengine.Fact]string{}
	entry := resolved.CatalogObservation
	if entry != nil {
		argv := resolved.Profile.Runtime.Argv
		for _, fact := range []inferenceengine.Fact{inferenceengine.FactContextArgv, inferenceengine.FactPrefillArgv} {
			absent[fact] = true
		}
		// The same catalog expansion supplies the actual process argv. Extract only
		// the capacity flags, preserving spelling and value; no engine defaults.
		for i := 0; i < len(argv); i++ {
			flag, value, inline := strings.Cut(argv[i], "=")
			if !inline && i+1 < len(argv) {
				value = argv[i+1]
			}
			spec, known := engineCheckFlags[flag]
			// Values of other options are data, never capacity options.
			if !known || spec.mlxOnly && entry.Engine == "llama-cpp" {
				continue
			}
			if spec.reading == "" {
				if spec.value && !inline && i+1 < len(argv) {
					i++
				}
				continue
			}
			fact := spec.reading
			tokens := []string{argv[i]}
			if !inline {
				tokens = append(tokens, value)
			}
			if previous, ok := values[fact].([]string); ok {
				tokens = append(previous, tokens...)
			}
			values[fact] = tokens
			absent[fact] = false
			if !inline && i+1 < len(argv) {
				i++
			}
		}
		if entry.Mode == "local" {
			values[inferenceengine.FactLocalExecutable] = resolved.Profile.Runtime.Executable
			values[inferenceengine.FactLocalArgv] = resolved.Profile.Runtime.Argv
			absent[inferenceengine.FactSSHForwarding] = true
		} else {
			absent[inferenceengine.FactLocalExecutable] = true
			absent[inferenceengine.FactLocalArgv] = true
			values[inferenceengine.FactSSHForwarding] = map[string]any{"host": entry.SSHTarget, "local_port": catalogEndpointPort(resolved.ProfileName, *entry), "remote_port": entry.RemotePort}
		}
		values[inferenceengine.FactStressPolicy] = map[string]any{"configured": false}
		if p := entry.Stress; p != nil {
			values[inferenceengine.FactStressPolicy] = map[string]any{"prompt_tokens": p.PromptTokens, "max_output_tokens": p.MaxOutputTokens, "startup_timeout_seconds": p.StartupTimeoutSeconds, "request_timeout_seconds": p.RequestTimeoutSeconds, "sample_interval_milliseconds": p.SampleIntervalMilliseconds}
		}
		values[inferenceengine.FactRestartSupervisionPolicy] = map[string]any{"configured": false}
		if p := entry.Supervision; p != nil {
			values[inferenceengine.FactRestartSupervisionPolicy] = map[string]any{"fatal_output_substrings": p.FatalOutputSubstrings, "restart_on_failure": p.RestartOnFailure, "max_restarts": p.MaxRestarts, "restart_window_seconds": p.RestartWindowSeconds, "restart_delay_milliseconds": p.RestartDelayMilliseconds}
		}
		if entry.Mode == "local" {
			degradeUnrepresentableLocalArgv(resolved, values, degraded)
		}
	}
	result := EngineReadings{ContractVersion: inferenceengine.ContractVersionV3}
	for _, definition := range inferenceengine.MeasuredFacts() {
		row := EngineReading{Fact: definition.Fact, Outcome: inferenceengine.OutcomeNotObserved, Cause: inferenceengine.NotObservedUnsupported, Reason: "no attested live evidence for this fact", Source: "unavailable"}
		if resolved.NotRunnableCode != "" {
			row.Reason = resolved.NotRunnableCode + ": " + resolved.NotRunnableReason
		}
		if entry == nil {
			row.Reason = "legacy profile has no engines.toml observation source"
		}
		if absent[definition.Fact] {
			row = EngineReading{Fact: definition.Fact, Outcome: inferenceengine.OutcomeObservedAbsent, Source: "engines.toml/v1"}
		}
		if detail, bad := degraded[definition.Fact]; bad {
			row = EngineReading{Fact: definition.Fact, Outcome: inferenceengine.OutcomeNotObserved, Cause: inferenceengine.NotObservedMalformed, Reason: detail, Source: "engines.toml/v1"}
		}
		if value, present := values[definition.Fact]; present {
			raw, err := json.Marshal(value)
			if err != nil {
				return EngineReadings{}, sharedRuntimeError("readings_malformed", err)
			}
			text := string(raw)
			if path, ok := value.(string); ok {
				text = path
			}
			row = EngineReading{Fact: definition.Fact, Outcome: inferenceengine.OutcomeObservedValue, Value: text, Source: "engines.toml/v1"}
		}
		result.Facts = append(result.Facts, row)
	}

	// This projection checks profile syntax only. Runtime facts are outside this
	// check, not claimed absent in the public report. The public NotObserved rows
	// remain unchanged and still refuse a consumer's full ValidateReadings call.
	// A degraded fact is probed with a schema-valid placeholder that is never
	// published, so this gate still checks every published value.
	if entry != nil {
		var profileShape []inferenceengine.Reading
		for _, row := range result.Facts {
			read := inferenceengine.ReadAbsent()
			if row.Outcome == inferenceengine.OutcomeObservedValue {
				read = inferenceengine.ReadValue(row.Value)
			}
			if _, bad := degraded[row.Fact]; bad {
				read = inferenceengine.ReadValue(engineReadingsDegradedArgvProbe)
			}
			profileShape = append(profileShape, inferenceengine.NewReading(row.Fact, read))
		}
		kind := inferenceengine.EngineKindNativeTransformer
		if entry.Engine == "llama-cpp" {
			kind = inferenceengine.EngineKindGGUFServer
		}
		if _, err := inferenceengine.ValidateReadings(plugin.ID(resolved.ProfileName), kind, profileShape, inferenceengine.ContractVersionV3); err != nil {
			return EngineReadings{}, sharedRuntimeError("readings_malformed", err)
		}
	}
	return result, nil
}

// Attestation performed a bounded HTTP model-discovery read against the
// kernel-attested process. Discovery proves liveness, not resident weights.
// The broker resource schema supplies inference busy only, not artifact-aware
// memory accounting, lifecycle transitions or a pressure consultation trace.
func observeAttestedEngineReadings(readings *EngineReadings, healthPath string, resources SharedRuntimeResourceStatus, now time.Time) error {
	if err := validateEngineResourceReadings(resources, now); err != nil {
		return err
	}
	for i := range readings.Facts {
		row := &readings.Facts[i]
		if row.Fact == inferenceengine.FactHealth {
			raw, _ := json.Marshal(map[string]any{"endpoint": healthPath, "healthy": true})
			*row = EngineReading{Fact: row.Fact, Outcome: inferenceengine.OutcomeObservedValue, Value: string(raw), Source: "attested-http-model-discovery"}
		}
		if row.Fact == inferenceengine.FactInferenceBusy && resources.Source == "provider-observed" {
			if resources.Inference.State == SharedRuntimeInferenceUnknown {
				row.Reason = resources.Inference.Reason
				row.Source = "provider-observed"
			} else {
				raw, _ := json.Marshal(map[string]bool{"busy": resources.Inference.State == SharedRuntimeInferenceBusy})
				*row = EngineReading{Fact: row.Fact, Outcome: inferenceengine.OutcomeObservedValue, Value: string(raw), Source: "provider-observed"}
			}
		}
	}
	return nil
}

// Validate the entire resource envelope before publishing even the health row.
// Enum and numeric checks apply to each branch, including disabled observation.
func validateEngineResourceReadings(resources SharedRuntimeResourceStatus, now time.Time) error {
	if resources.Schema != SharedRuntimeResourceStatusSchema {
		return sharedRuntimeError("readings_malformed", errors.New("resource observation schema mismatch"))
	}
	if resources.Source != "provider-observed" && resources.Source != "provider-observation-failed" && resources.Source != "broker-config" {
		return sharedRuntimeError("readings_malformed", errors.New("resource observation source invalid"))
	}
	if resources.Source == "provider-observation-failed" {
		return sharedRuntimeError("readings_read_failed", errors.New("provider resource observation failed"))
	}
	if (resources.State == SharedRuntimeResourceUnknown || resources.State == SharedRuntimeResourcePressured || resources.State == SharedRuntimeResourceDraining) && resources.Reason == "" {
		return sharedRuntimeError("readings_partial", errors.New("unavailable resource state lacks reason"))
	}
	if (resources.State == SharedRuntimeResourceHealthy || resources.State == SharedRuntimeResourceBusy) && resources.Reason != "" {
		return sharedRuntimeError("readings_malformed", errors.New("available resource state carries reason"))
	}

	if resources.LoadedModelMemory.State != SharedRuntimeMemoryHealthy && resources.LoadedModelMemory.State != SharedRuntimeMemoryPressured && resources.LoadedModelMemory.State != SharedRuntimeMemoryUnknown {
		return sharedRuntimeError("readings_malformed", errors.New("provider memory state invalid"))
	}
	if resources.LoadedModelMemory.State == SharedRuntimeMemoryUnknown {
		if resources.LoadedModelMemory.Bytes != nil || resources.LoadedModelMemory.Reason == "" {
			return sharedRuntimeError("readings_partial", errors.New("unknown memory has bytes or lacks reason"))
		}
	} else {
		if resources.LoadedModelMemory.Bytes == nil || *resources.LoadedModelMemory.Bytes == 0 || resources.LoadedModelMemory.Reason != "" {
			return sharedRuntimeError("readings_partial", errors.New("observed memory lacks positive bytes or carries reason"))
		}
	}
	if resources.Inference.State != SharedRuntimeInferenceBusy && resources.Inference.State != SharedRuntimeInferenceIdle && resources.Inference.State != SharedRuntimeInferenceUnknown {
		return sharedRuntimeError("readings_malformed", errors.New("provider inference state invalid"))
	}
	if resources.Inference.State == SharedRuntimeInferenceUnknown {
		if resources.Inference.Reason == "" {
			return sharedRuntimeError("readings_partial", errors.New("unknown provider inference lacks reason"))
		}
	} else if resources.Inference.Reason != "" {
		return sharedRuntimeError("readings_malformed", errors.New("observed inference carries reason"))
	}
	if resources.Policy.Mode != "provider" && resources.Policy.Mode != "disabled" && resources.Policy.Mode != "unknown" {
		return sharedRuntimeError("readings_malformed", errors.New("resource policy mode invalid"))
	}
	if resources.Policy.Mode == "provider" {
		if resources.Policy.EvictionGraceSeconds < 0 || int64(resources.Policy.EvictionGraceSeconds) > maxTimeDurationSeconds {
			return sharedRuntimeError("readings_malformed", errors.New("resource eviction grace outside duration range"))
		}
		if resources.Policy.PressureAction != "refuse-new-drain-idle" {
			return sharedRuntimeError("readings_malformed", errors.New("resource pressure action invalid"))
		}
		if resources.Policy.UnknownAction != "refuse-new" {
			return sharedRuntimeError("readings_malformed", errors.New("resource unknown action invalid"))
		}
		if resources.Policy.BusyAction != "observe" {
			return sharedRuntimeError("readings_malformed", errors.New("resource busy action invalid"))
		}
		if resources.LoadedModelMemory.RecoveryAtBytes <= 0 || resources.LoadedModelMemory.RecoveryAtBytes >= resources.LoadedModelMemory.PressureAtBytes {
			return sharedRuntimeError("readings_malformed", errors.New("memory thresholds invalid"))
		}
		if resources.LoadedModelMemory.State == SharedRuntimeMemoryHealthy && *resources.LoadedModelMemory.Bytes >= uint64(resources.LoadedModelMemory.PressureAtBytes) || resources.LoadedModelMemory.State == SharedRuntimeMemoryPressured && *resources.LoadedModelMemory.Bytes <= uint64(resources.LoadedModelMemory.RecoveryAtBytes) {
			return sharedRuntimeError("readings_malformed", errors.New("memory state contradicts pressure hysteresis"))
		}

	} else {
		if resources.Policy.EvictionGraceSeconds != 0 || resources.Policy.PressureAction != "" || resources.Policy.UnknownAction != "" || resources.Policy.BusyAction != "" || resources.LoadedModelMemory.PressureAtBytes != 0 || resources.LoadedModelMemory.RecoveryAtBytes != 0 {
			return sharedRuntimeError("readings_malformed", errors.New("inactive policy carries provider settings"))
		}
	}
	if resources.Source == "provider-observed" {
		if resources.Policy.Mode != "provider" {
			return sharedRuntimeError("readings_malformed", errors.New("observed provider lacks provider policy"))
		}
		if resources.ObservedAt == nil {
			return sharedRuntimeError("readings_partial", errors.New("provider observation lacks timestamp"))
		}
		age := now.Sub(*resources.ObservedAt)
		if age < 0 || age > 30*time.Second {
			return sharedRuntimeError("readings_stale", errors.New("provider observation timestamp outside freshness window"))
		}
	} else {
		if resources.ObservedAt != nil || resources.LoadedModelMemory.State != SharedRuntimeMemoryUnknown || resources.Inference.State != SharedRuntimeInferenceUnknown {
			return sharedRuntimeError("readings_malformed", errors.New("unobserved source carries observed facts"))
		}
	}
	// Match the broker's classification, including the draining override.
	state, admission := SharedRuntimeResourceHealthy, SharedRuntimeAdmissionAdmitted
	if resources.State == SharedRuntimeResourceDraining {
		state, admission = SharedRuntimeResourceDraining, SharedRuntimeAdmissionRefused
	} else if resources.Policy.Mode == "disabled" {
		state, admission = SharedRuntimeResourceUnknown, SharedRuntimeAdmissionNotEnforced
	} else if resources.LoadedModelMemory.State == SharedRuntimeMemoryPressured {
		state, admission = SharedRuntimeResourcePressured, SharedRuntimeAdmissionRefused
	} else if resources.LoadedModelMemory.State == SharedRuntimeMemoryUnknown || resources.Inference.State == SharedRuntimeInferenceUnknown {
		state, admission = SharedRuntimeResourceUnknown, SharedRuntimeAdmissionRefused
	} else if resources.Inference.State == SharedRuntimeInferenceBusy {
		state = SharedRuntimeResourceBusy
	}
	if resources.State != state || resources.Admission != admission {
		return sharedRuntimeError("readings_malformed", errors.New("resource classification contradicts facts"))
	}
	return nil
}

// engineReadingsDegradedArgvProbe stands in for a degraded argv fact in the
// profile-shape gate only. It is never published. A single non-empty token
// satisfies the pinned argv-tokens contract.
const engineReadingsDegradedArgvProbe = `["argv unrepresentable in closed schema"]`

// A catalog-validated entry can still resolve to a local argv outside the
// pinned closed schema: model-less mlx-swift and llama-cpp entries expand to
// a runtime argv carrying an empty --model-id or --alias token. The catalog
// forbids empty raw tokens, so an empty resolved token is provably an
// expansion artifact, never a configured value. It degrades to an explicit
// NotObserved/malformed row instead of failing the whole status report: the
// pinned validator refuses any set carrying a NotObserved row, so launch
// stays fail-closed while the additive status contract keeps working for
// designed NotRunnable states such as the legacy-import admission read.
// Entry-own values outside the schema (a non-clean executable, an extracted
// empty capacity value) keep the typed-refusal path through the gate below.
func degradeUnrepresentableLocalArgv(resolved sharedResolvedProfile, values map[inferenceengine.Fact]any, degraded map[inferenceengine.Fact]string) {
	entry := resolved.CatalogObservation
	tokens, ok := values[inferenceengine.FactLocalArgv].([]string)
	if !ok {
		return
	}
	runnable := "runnable"
	if resolved.NotRunnableCode != "" {
		runnable = "not-runnable " + resolved.NotRunnableCode + ": " + resolved.NotRunnableReason
	}
	if len(tokens) == 0 {
		degraded[inferenceengine.FactLocalArgv] = fmt.Sprintf("resolved local argv is empty (%s engine %q; engines.toml/v1)", entry.Engine, runnable)
		delete(values, inferenceengine.FactLocalArgv)
		return
	}
	for index, token := range tokens {
		if token == "" {
			degraded[inferenceengine.FactLocalArgv] = fmt.Sprintf("resolved local argv token %d empty (%s engine %q; engines.toml/v1)", index, entry.Engine, runnable)
			delete(values, inferenceengine.FactLocalArgv)
			return
		}
	}
}
