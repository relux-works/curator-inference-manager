//go:build darwin

package infra

import (
	"errors"
	"strings"
	"time"
)

// This file validates every evidence envelope consumed on the readings path
// before any reading is built: the hello_ok and status wire replies and every
// object they nest (broker and runtime records, sharing policy, leases and
// the resource observation). One malformed member is a typed refusal for the
// whole readings set, never a partial set. Key closure (exact case, no
// duplicates or unknown members) runs through the shared strict decoder in
// readSharedWireMessage; the functions below enforce required members, closed
// enums and ranges, and identity consistency against the attested hello
// values. Callers check pointer presence before delegating here.
//
// Closed wire domains (from the broker implementation, the only producer):
//   - status state: serving, draining (server.state while serving)
//   - status stage: composed (record.Stage after composition, before listen)
//   - runtime stage: authorizing, running (serving-phase lifecycle; the
//     inherited-unreclaimed marker is pre-listen only and never served)

// validateSharedWireMessageKeys enforces the exact-case closed member set of a
// raw wire frame, with required-member presence and null rejection. Absent and
// null scalars would otherwise decode to valid Go zero values before any
// semantic range check runs. It returns a plain error; readSharedWireMessage
// wraps it in the existing typed protocol_violation refusal.
func validateSharedWireMessageKeys(frame []byte) error {
	return validateStrictJSON(frame, sharedWireStrictSpec(), true)
}

// sharedWireStrictSpec is the single declaration of every readings wire
// envelope: the allowed member set plus the required members of each nested
// object. required mirrors the owner struct's non-omitempty JSON members, in
// struct order. The generated required-member matrix test walks this
// declaration, so a new owner member without a required entry fails the
// owner-tag pin test instead of silently admitting omission.
func sharedWireStrictSpec() *strictJSONSpec {
	startTime := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"seconds": nil, "microseconds": nil,
	}, required: []string{"seconds", "microseconds"}}
	fileIdentity := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"dev": nil, "ino": nil, "size": nil, "mode": nil,
	}, required: []string{"dev", "ino", "size", "mode"}}
	execShape := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"exec_path": nil, "argv": nil,
	}, required: []string{"exec_path", "argv"}, nullable: []string{"argv"}}
	engineIdentity := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"name": nil, "key": nil,
	}, required: []string{"name", "key"}}
	resourcePressure := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"observation_path": nil, "observation_timeout_milliseconds": nil,
		"pressure_threshold_bytes": nil, "recovery_threshold_bytes": nil,
		"eviction_grace_seconds": nil, "pressure_action": nil, "unknown_action": nil, "busy_action": nil,
	}, required: []string{
		"observation_path", "observation_timeout_milliseconds",
		"pressure_threshold_bytes", "recovery_threshold_bytes",
		"eviction_grace_seconds", "pressure_action", "unknown_action", "busy_action",
	}}
	// The millisecond backoffs, the nested pressure policy and the fatal
	// markers are the owner struct's omitempty members: a TOML profile omits
	// them, a catalog profile carries them, and the semantic validator checks
	// ranges and content when they are present. Everything else is required.
	sharing := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"mode": nil, "linger_seconds": nil, "max_leases": nil,
		"max_segment_bytes": nil, "max_segments": nil, "heartbeat_interval_seconds": nil,
		"lease_stale_seconds": nil, "broker_start_timeout_seconds": nil, "restart_limit": nil,
		"restart_initial_backoff_seconds": nil, "restart_max_backoff_seconds": nil,
		"restart_initial_backoff_milliseconds": nil, "restart_max_backoff_milliseconds": nil,
		"stable_run_seconds": nil, "quarantine_seconds": nil, "resource_pressure_mode": nil,
		"resource_pressure": resourcePressure, "fatal_output_substrings": nil,
	}, required: []string{
		"mode", "linger_seconds", "max_leases",
		"max_segment_bytes", "max_segments", "heartbeat_interval_seconds",
		"lease_stale_seconds", "broker_start_timeout_seconds", "restart_limit",
		"restart_initial_backoff_seconds", "restart_max_backoff_seconds",
		"stable_run_seconds", "quarantine_seconds", "resource_pressure_mode",
	}}
	broker := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"pid": nil, "pgid": nil, "sid": nil, "start_time": startTime, "uid": nil,
		"exec_path": nil, "argv": nil, "executable_identity": fileIdentity,
	}, required: []string{
		"pid", "pgid", "sid", "start_time", "uid",
		"exec_path", "argv", "executable_identity",
	}, nullable: []string{"argv"}}
	weights := &strictJSONSpec{object: map[string]*strictJSONSpec{"id": nil, "observed_at": nil}, required: []string{"id", "observed_at"}}
	runtime := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"pid": nil, "pgid": nil, "start_time": startTime, "uid": nil,
		"pre_exec": execShape, "post_exec": execShape, "cwd": nil, "endpoint": nil,
		"executable_identity": fileIdentity, "exec_plan_digest": nil, "stage": nil, "weights": weights,
	}, required: []string{
		"pid", "pgid", "start_time", "uid",
		"pre_exec", "post_exec", "cwd", "endpoint",
		"executable_identity", "exec_plan_digest", "stage",
	}}
	// The Bearer [REDACTED] and run id are the owner record's omitempty members:
	// Bearer [REDACTED] never appear on the wire and the run id is echo-only.
	lease := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"lease_id": nil, "lease_token": nil, "lease_lifetime": nil, "engine_identity": engineIdentity,
		"run_id": nil, "client_key": nil, "client_pid": nil, "client_start_time": startTime,
		"runtime_key": nil, "profile_digest": nil, "endpoint": nil, "runtime_pid": nil,
		"granted_at": nil, "last_heartbeat_at": nil,
	}, required: []string{
		"lease_id", "lease_lifetime", "engine_identity",
		"client_key", "client_pid", "client_start_time",
		"runtime_key", "profile_digest", "endpoint", "runtime_pid",
		"granted_at", "last_heartbeat_at",
	}}
	// SharedLeaseStatus embeds the lease record, so its members are flattened
	// beside the status wrapper members.
	leaseStatus := &strictJSONSpec{object: map[string]*strictJSONSpec{
		"lease_id": nil, "lease_token": nil, "lease_lifetime": nil, "engine_identity": engineIdentity,
		"run_id": nil, "client_key": nil, "client_pid": nil, "client_start_time": startTime,
		"runtime_key": nil, "profile_digest": nil, "endpoint": nil, "runtime_pid": nil,
		"granted_at": nil, "last_heartbeat_at": nil,
		"state": nil, "age": nil, "heartbeat_age": nil,
	}, required: []string{
		"lease_id", "lease_lifetime", "engine_identity",
		"client_key", "client_pid", "client_start_time",
		"runtime_key", "profile_digest", "endpoint", "runtime_pid",
		"granted_at", "last_heartbeat_at",
		"state", "age", "heartbeat_age",
	}}
	// The frame type is the only required top-level member: every other
	// member is message-type-specific and omitempty, with presence enforced
	// by each message handler.
	return &strictJSONSpec{object: map[string]*strictJSONSpec{
		"type": nil, "code": nil, "reason": nil, "mismatch_field": nil,
		"protocol_version": nil, "client_pid": nil, "client_exec": fileIdentity,
		"runtime_key": nil, "profile_digest": nil, "engine_identity": engineIdentity,
		"project_key": nil, "profile_key": nil, "run_id": nil,
		"sharing_configured": sharing, "sharing_effective": sharing,
		"broker": broker, "runtime": runtime, "lease": lease,
		"leases":   &strictJSONSpec{elements: leaseStatus},
		"lease_id": nil, "lease_token": nil, "lease_count": nil,
		"client_key": nil, "requested_at": nil, "force": nil, "timeout_seconds": nil,
		"state": nil, "stage": nil,
		"effective_max_leases": nil, "configured_max_leases": nil,
		"broker_pid": nil, "broker_start_time": startTime,
		"resources": engineResourceStrictSpec(),
	}, required: []string{"type"}}
}

// validateAttestedHelloEnvelope validates the hello_ok members that kernel
// attestation does not cover: the runtime stage enum and every admitted
// sharing object. Presence is checked by the caller.
func validateAttestedHelloEnvelope(message sharedWireMessage, runtime PiRuntime) error {
	if err := validateWireWeights(message.Runtime.Weights); err != nil {
		return err
	}
	if message.Runtime.Stage != "authorizing" && message.Runtime.Stage != "running" {
		return sharedRuntimeError("protocol_violation", errors.New("broker hello runtime stage invalid"))
	}
	return validateWireSharingCarriers(message, runtime)
}

// validateAttestedStatusEnvelope validates a status reply against the hello
// attestation before any member is published or read. Presence is checked by
// the caller.
func validateAttestedStatusEnvelope(message sharedWireMessage, attested sharedAttestedRuntime, resolved sharedResolvedProfile) error {
	if message.State != "serving" && message.State != "draining" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status state invalid"))
	}
	if message.Stage != "composed" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status stage invalid"))
	}
	if err := validateWireBrokerIdentity(*message.Broker, attested.broker); err != nil {
		return err
	}
	if err := validateWireRuntimeRecord(*message.Runtime, attested.runtime); err != nil {
		return err
	}
	if err := validateWireSharingCarriers(message, resolved.Profile.Runtime); err != nil {
		return err
	}
	for _, lease := range message.Leases {
		if err := validateWireLease(lease, attested.runtime.PID, resolved); err != nil {
			return err
		}
	}
	return nil
}

// validateWireSharingCarriers runs the producer-sourced semantic validator
// over every sharing object a hello or status reply admits. Effective sharing
// is required (callers check presence before delegating here); configured
// sharing is optional, but when a reply carries it the strict declaration
// admits it as a sharing object, so it obeys the same producer bounds.
// Routing both carriers through this one helper keeps any carrier of the
// shared declaration from skipping validation.
func validateWireSharingCarriers(message sharedWireMessage, runtime PiRuntime) error {
	if message.ConfiguredSharing != nil {
		if err := validateWireSharing(*message.ConfiguredSharing, runtime); err != nil {
			return err
		}
	}
	return validateWireSharing(*message.EffectiveSharing, runtime)
}

// validateWireBrokerIdentity ties the status broker record back to the
// attested hello identity. PID, start time and executable are the attested
// triple; the remaining record fields are informational.
func validateWireBrokerIdentity(broker, attested SharedBrokerIdentity) error {
	if broker.PID != attested.PID {
		return sharedRuntimeError("protocol_violation", errors.New("broker status broker identity differs from attested hello"))
	}
	if broker.StartTime != attested.StartTime {
		return sharedRuntimeError("protocol_violation", errors.New("broker status broker start time differs from attested hello"))
	}
	if broker.ExecutableIdentity != attested.ExecutableIdentity {
		return sharedRuntimeError("protocol_violation", errors.New("broker status broker executable differs from attested hello"))
	}
	return nil
}

// validateWireRuntimeRecord ties the status runtime record back to the
// attested hello identity and its closed stage enum.
func validateWireRuntimeRecord(record, attested SharedRuntimeProcessRecord) error {
	if err := validateWireWeights(record.Weights); err != nil {
		return err
	}
	if (record.Weights == nil) != (attested.Weights == nil) || (record.Weights != nil && *record.Weights != *attested.Weights) {
		return sharedRuntimeError("protocol_violation", errors.New("broker status weights differ from attested instance"))
	}
	if record.PID != attested.PID {
		return sharedRuntimeError("protocol_violation", errors.New("broker status runtime identity differs from attested hello"))
	}
	if record.StartTime != attested.StartTime {
		return sharedRuntimeError("protocol_violation", errors.New("broker status runtime start time differs from attested hello"))
	}
	if record.Endpoint != attested.Endpoint {
		return sharedRuntimeError("protocol_violation", errors.New("broker status runtime endpoint differs from attested hello"))
	}
	if record.ExecutableIdentity != attested.ExecutableIdentity {
		return sharedRuntimeError("protocol_violation", errors.New("broker status runtime executable differs from attested hello"))
	}
	if record.Stage != "authorizing" && record.Stage != "running" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status runtime stage invalid"))
	}
	return nil
}

// validateWireSharing enforces the closed sharing schema on hello and status
// replies. The broker only forwards validated configuration, so any violation
// is hostile evidence. Every range and content check names the producer's own
// bound (the pi_config duration constants and helpers, the catalog marker and
// delay constants), so the reader can never be narrower or wider than the
// writer. The wire accepts the union of TOML-derived and catalog-derived
// sharing: catalog entries legitimately use zero restart limit and backoff
// seconds for "no restart" / "no backoff" (the millisecond fields carry
// sub-second delays) and omit fatal markers and millisecond fields when the
// entry has no supervision section. runtime is the attested resolved profile's
// runtime; its digest already matched, so its startup/shutdown values are the
// same ones the producer bound was computed from.
func validateWireSharing(sharing PiRuntimeSharing, runtime PiRuntime) error {
	if sharing.Mode != "exclusive" && sharing.Mode != "shared" {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing mode invalid"))
	}
	if sharing.LingerSeconds < 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing linger seconds outside range"))
	}
	if sharing.MaxLeases <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing max leases outside range"))
	}
	if sharing.MaxSegmentBytes <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing max segment bytes outside range"))
	}
	if sharing.MaxSegments <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing max segments outside range"))
	}
	if sharing.HeartbeatIntervalSeconds <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing heartbeat interval outside range"))
	}
	// Non-positive lease staleness is refused by the heartbeat gates: either
	// the heartbeat ordering check fires, or the heartbeat itself is
	// non-positive. A separate gate would be entailed and unkillable.
	if sharing.BrokerStartTimeoutSeconds <= 0 || int64(sharing.BrokerStartTimeoutSeconds) > maxTimeDurationSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing broker start timeout outside range"))
	}
	// Catalog-derived sharing uses zero restart limit and backoff seconds for
	// "no restart" and "no backoff" (the millisecond fields carry sub-second
	// delays), while TOML configuration requires positive values. The wire
	// admits zero because some legitimate broker forwards it.
	if sharing.RestartLimit < 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart limit outside range"))
	}
	if sharing.RestartInitialBackoffSeconds < 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart backoff outside range"))
	}
	// Negative max backoff is refused by the initial-backoff and ordering
	// gates below. A separate gate would be entailed and unkillable.
	if sharing.RestartInitialBackoffMilliseconds < 0 || sharing.RestartInitialBackoffMilliseconds > engineCatalogRestartDelayMaxMilliseconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart backoff outside range"))
	}
	if sharing.RestartMaxBackoffMilliseconds < 0 || sharing.RestartMaxBackoffMilliseconds > engineCatalogRestartDelayMaxMilliseconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart backoff outside range"))
	}
	if sharing.StableRunSeconds <= 0 || int64(sharing.StableRunSeconds) > maxTimeDurationSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing stable run seconds outside range"))
	}
	if sharing.QuarantineSeconds <= 0 || int64(sharing.QuarantineSeconds) > maxTimeDurationSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing quarantine seconds outside range"))
	}
	if sharing.HeartbeatIntervalSeconds >= sharing.LeaseStaleSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing heartbeat must be less than lease stale seconds"))
	}
	if sharing.RestartMaxBackoffSeconds < sharing.RestartInitialBackoffSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart backoff unordered"))
	}
	if sharing.ResourcePressureMode != "disabled" && sharing.ResourcePressureMode != "provider" {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing resource pressure mode invalid"))
	}
	if sharing.ResourcePressureMode == "provider" && sharing.ResourcePressure == nil {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing provider pressure lacks policy"))
	}
	if sharing.ResourcePressureMode == "disabled" && sharing.ResourcePressure != nil {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing disabled pressure carries policy"))
	}
	if sharing.ResourcePressure != nil {
		if err := validateWireResourcePressure(*sharing.ResourcePressure); err != nil {
			return err
		}
	}
	// Producer upper bounds below. The duration maxima name the pi_config
	// constants; the restart and marker bounds name the catalog constants.
	// Heartbeat, initial-backoff and max-backoff maxima need no separate
	// gate: a heartbeat above the maximum trips the heartbeat/stale ordering
	// or the stale upper below, and a backoff above the maximum trips the
	// backoff ordering or the backoff/timeout gate below. Separate gates
	// would be entailed and unkillable.
	if int64(sharing.LeaseStaleSeconds) > maxSharedRuntimeLeaseStaleSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing lease stale seconds outside range"))
	}
	// The producer requires the broker start timeout to cover runtime startup
	// plus shutdown plus its slop; catalog entries meet it exactly.
	// Non-positive values stay with the gate above, which keeps its
	// singleton witness; this gate covers the short-but-positive range.
	if sharing.BrokerStartTimeoutSeconds > 0 && int64(sharing.BrokerStartTimeoutSeconds) < int64(runtime.StartupTimeoutSeconds)+int64(runtime.ShutdownTimeoutSeconds)+sharedRuntimeBrokerStartSlopSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing broker start timeout outside range"))
	}
	// The producer requires restart backoff below the broker start timeout;
	// every catalog delay maps below every catalog timeout, so the union
	// admits no exception here either. Non-positive timeouts stay with the
	// timeout gates above, which keep their singleton witness; this gate
	// covers the ordered positive range.
	if sharing.BrokerStartTimeoutSeconds > 0 && sharing.RestartMaxBackoffSeconds >= sharing.BrokerStartTimeoutSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing restart backoff exceeds broker start timeout"))
	}
	// Linger plus runtime shutdown plus the handoff grace must fit a
	// time.Duration. This exact producer bound subsumes the loose duration
	// maximum, so no separate loose gate exists.
	if int64(sharing.LingerSeconds) > piSharingMaximumLingerSeconds(runtime.ShutdownTimeoutSeconds) {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing linger seconds outside range"))
	}
	// Fatal markers ride the wire only for supervised catalog entries; a TOML
	// profile omits them. A present list must satisfy the catalog content
	// rule, named through the same constants.
	if len(sharing.FatalOutputSubstrings) > engineCatalogFatalMarkersMaxCount {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing fatal marker count outside range"))
	}
	for _, marker := range sharing.FatalOutputSubstrings {
		if marker == "" || len(marker) > engineCatalogFatalMarkerMaxBytes || strings.IndexByte(marker, 0) >= 0 {
			return sharedRuntimeError("protocol_violation", errors.New("broker sharing fatal marker invalid"))
		}
	}
	// A present list also obeys the producer's minimum cardinality: the
	// catalog requires 1..16 entries when the supervision section exists,
	// and every producer omits the member otherwise (omitempty), so a
	// present empty list is hostile evidence. Absence decodes to nil and
	// stays legal; explicit null already refuses in the strict pre-pass.
	// This gate trails the maximum and element gates so the census ordinals
	// above keep their identities.
	if sharing.FatalOutputSubstrings != nil && len(sharing.FatalOutputSubstrings) < engineCatalogFatalMarkersMinCount {
		return sharedRuntimeError("protocol_violation", errors.New("broker sharing fatal marker count outside range"))
	}
	return nil
}

// validateWireResourcePressure enforces the closed provider pressure policy
// nested in hello and status sharing replies. Bounds name the producer's own
// constants and helpers.
func validateWireResourcePressure(policy PiRuntimeResourcePressure) error {
	if policy.ObservationPath == "" {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure observation path invalid"))
	}
	if policy.ObservationTimeoutMilliseconds <= 0 || policy.ObservationTimeoutMilliseconds > sharedRuntimeResourceObservationMaxTimeoutMilliseconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure observation timeout outside range"))
	}
	// Non-positive pressure is refused by the recovery gates below: with
	// recovery > 0 the ordering check fires, otherwise the recovery check
	// fires. A separate pressure-positivity gate would be entailed and its
	// narrowing mutant unkillable, so it is deliberately absent.
	if policy.RecoveryThresholdBytes <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure threshold outside range"))
	}
	if policy.RecoveryThresholdBytes >= policy.PressureThresholdBytes {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure thresholds unordered"))
	}
	if policy.EvictionGraceSeconds < 0 || int64(policy.EvictionGraceSeconds) > maxTimeDurationSeconds {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure eviction grace outside range"))
	}
	if policy.PressureAction != "refuse-new-drain-idle" {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure action invalid"))
	}
	if policy.UnknownAction != "refuse-new" {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure unknown action invalid"))
	}
	if policy.BusyAction != "observe" {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure busy action invalid"))
	}
	// The producer requires a clean absolute HTTP path without query or
	// fragment. Empty stays with the gate above, which keeps its singleton
	// witness; this gate covers non-empty malformed shapes only.
	if policy.ObservationPath != "" && !piObservationPathValid(policy.ObservationPath) {
		return sharedRuntimeError("protocol_violation", errors.New("broker resource pressure observation path invalid"))
	}
	return nil
}

// validateWireLease enforces required members, closed enums and identity
// consistency on one status lease row. Lease bearer tokens never appear on
// the wire; ages and timestamps are informational broker observations.
func validateWireLease(lease SharedLeaseStatus, runtimePID int, resolved sharedResolvedProfile) error {
	if lease.LeaseID == "" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease identity invalid"))
	}
	if lease.LeaseToken != "" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease exposes a bearer token"))
	}
	if lease.LeaseLifetime != "client_process" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease lifetime invalid"))
	}
	if lease.EngineIdentity.Key != resolved.EngineIdentity.Key {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease engine differs"))
	}
	if lease.RuntimeKey != resolved.RuntimeKey {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease runtime key differs"))
	}
	if lease.ProfileDigest != resolved.ProfileDigest {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease profile digest differs"))
	}
	if lease.Endpoint != resolved.Profile.BaseURL {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease endpoint differs"))
	}
	if lease.RuntimePID != runtimePID {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease runtime differs from attested hello"))
	}
	if lease.ClientPID <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease client identity invalid"))
	}
	if lease.ClientStartTime.Seconds <= 0 {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease client start time invalid"))
	}
	if lease.State != "held" && lease.State != "held(stale)" {
		return sharedRuntimeError("protocol_violation", errors.New("broker status lease state invalid"))
	}
	return nil
}

func validateWireWeights(weights *EngineWeightsObservation) error {
	if weights != nil && (!engineWeightsIDPattern.MatchString(weights.ID) || weights.ObservedAt.IsZero() || weights.ObservedAt.After(time.Now())) {
		return sharedRuntimeError("protocol_violation", errors.New("invalid weights observation"))
	}
	return nil
}
