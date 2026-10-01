# `curator-engines` broker protocol v1.1

This additive revision incorporates [v1](engines-protocol-v1.md). Every existing
status field, omission rule, `contract_version = 1` and socket
`protocol_version = 7` is unchanged. No new socket or broker is introduced.

`status --json` adds `readings`:

```json
{"contract_version":"observed-process/v3","facts":[
  {"fact":"context-argv","outcome":"observed-value","value":"[\"--max-kv-size\",\"8192\"]","source":"engines.toml/v1"},
  {"fact":"readiness","outcome":"not-observed","cause":"unsupported","reason":"no attested live evidence for this fact","source":"unavailable"}
]}
```

The abbreviated example is not a complete readings set. Production always emits
all 17 facts, in `inferenceengine.MeasuredFacts()` order from the normal required
module `github.com/relux-works/skill-agents-management v0.5.39`. Its
`ValidateReadings(..., ContractVersionV3)` is the normative closed per-fact
schema. V3 selects `stress-policy/v2` and `restart-policy/v2`; other value
contracts are identical to observed-process/v2. The manager copies no schema.

`value` is the raw validator input string: JSON for structured values, an
absolute path for executable, or a field path for reasoning. `observed-absent`
means an explicitly absent profile setting, with neither value nor failure.
`not-observed` carries `cause` and a non-empty `reason`; it is a refusal at the
consumer's validator, never healthy conformance. `source` states provenance.

| Facts | Production evidence |
| --- | --- |
| context-argv, prefill-argv | Exact configured capacity flags in the catalog-expanded process argv; unset flags are observed-absent, never engine defaults |
| profile-local-executable, profile-local-argv | Expanded local catalog profile; observed-absent in SSH mode; an unrepresentable expansion artifact degrades local-argv to not-observed/malformed |
| profile-ssh-forwarding | Catalog SSH target and exact local/remote ports; observed-absent in local mode |
| profile-stress-policy, profile-restart-supervision-policy | Exact catalog workload/supervision fields, preserving zero restart delay; absent sections are `{"configured":false}` |
| health | Bounded HTTP model-discovery success against the kernel-attested runtime; this proves liveness only |
| inference-busy | Timestamped broker `provider-observed` inference state, including explicit provider unknown with reason |
| readiness, load-state, unload-state | NotObserved: current discovery/resource evidence does not establish resident weights or transitions |
| weight-artifact, memory-accounting | NotObserved: catalog model identifiers and resource byte counts do not prove artifact shape or artifact-aware accounting |
| memory-pressure-sequence | NotObserved: resource pressure state is not an ordered load/unload/busy consultation trace |
| speculative-decoding, reasoning-stream-field | NotObserved: argv settings do not establish runtime capabilities or streamed field observations |

Catalog fields are retained from the same validated read used to resolve the
runtime, avoiding a second read racing the process profile. Legacy profiles
without a catalog source explicitly report unknown profile facts. Without a
live attested engine all runtime facts are NotObserved, even if a persisted
broker record says serving. Disabled resource observation is unsupported, not
idle. A failed provider read is `readings_read_failed`. A wrong resource schema, unknown source
or resource/memory/inference/admission/policy enum is `readings_malformed`; a missing timestamp or unexplained
unknown inference is `readings_partial`. Provider timestamps outside the
inclusive zero-to-30-second age window are `readings_stale` (future timestamps
also refuse). These typed failures return no status/readings set. No partial
observations are published and no successful values are derived from failed
reads. Existing socket/identity refusals continue to propagate unchanged.

The live conformance test is read-only against the retained local engine. If
any required observation is unavailable it skips with the exact unknown
reason. Schema-only controls validate profile values using explicit absent
runtime placeholders; they do not claim an engine round trip or transitions.

Before publishing catalog profile values, the manager also runs a profile-only
syntax projection through the pinned validator. This catches catalog-accepted
absolute paths that are not clean without rewriting them. Runtime slots in that
private syntax check are absent placeholders; public NotObserved rows remain
unchanged. It attests no runtime facts and cannot authorize a healthy launch.
Native engine argv arity/numeric semantics beyond `argv-tokens/v1` are outside
this observation schema; the status transcribes configured tokens exactly.

A model-less mlx-swift or llama-cpp entry expands to a runtime argv carrying
an empty `--model-id` or `--alias` token. The catalog forbids empty raw
tokens, so an empty resolved token is provably an expansion artifact, never a
configured value. That profile-local-argv degrades to an explicit
not-observed/malformed row with its token index and runnability reason instead
of failing the status report; the pinned validator refuses any set carrying a
NotObserved row, so launch stays fail-closed while status keeps working for
designed NotRunnable states such as the legacy-import admission read. Degraded
facts are probed with an unpublished schema-valid placeholder in the
profile-only syntax check above, so that gate still checks every published
value. Entry-own values outside the schema keep the typed `readings_malformed`
refusal.

The entire resource-status envelope is validated before any attested reading is
published: required fields and closed JSON member sets (including nested memory,
inference and policy objects), no null or duplicate members, positive observed
memory bytes, explicit reasons for unknown facts, policy action spellings,
representable non-negative eviction grace, ordered positive memory thresholds,
pressure hysteresis consistency and resource/admission classification. Disabled
or unknown policies carry no provider settings. An unobserved source cannot
carry timestamps or observed facts. Invalid numeric/types/member shapes retain
the existing typed `protocol_violation` wire failure; semantic violations use
`readings_malformed` or `readings_partial`. Status propagates those typed wire
failures rather than wrapping them as an unreachable broker.

Capacity extraction uses the same option table as `check`: llama-cpp
`--ctx-size` / `-c` and `--ubatch-size` / `-ub`, plus MLX `--max-kv-size` and
`--prefill-step-size`. Configured spellings and values are retained verbatim,
including inline values. A token consumed as another option's value cannot
supply a capacity observation. This does not broaden `check`'s argv subset.

Every evidence envelope on the readings path is decoded through one shared
strict decoder with exact-case closed member sets: a key whose spelling
differs only in case is an unknown member, and duplicates refuse on the exact
raw key set. This closes the case-folding hole in `encoding/json` struct
decoding, which `DisallowUnknownFields` does not catch. The closed sets cover
the wire frame, the resource observation and every nested object (broker and
runtime records, start time, executable identity, exec shapes, sharing policy
and pressure policy, engine identity, lease records and lease rows).

Absent, null and zero are three distinct states on the wire. The shared
decoder enforces required-member presence and rejects null for every required
member of every nested object, before Go struct decoding can launder omission
into a valid zero. The required set of each object mirrors its owner struct's
non-omitempty JSON members and is pinned against the owner tags; legitimately
optional (omitempty) members stay absent-capable. Only process argv arrays
accept an explicit null, because the owner type declares them as
non-omitempty slices. Explicit zeros (linger, restart limit and backoffs,
eviction grace) stay valid. The whole-array permission never extends to
array elements: a null element inside any wire array refuses, because
`encoding/json` would otherwise erase it into a zero value.

Before any reading is built, the hello and status replies are validated
against required members, closed enums and ranges, and the attested hello
identities; one malformed member is a typed `protocol_violation` refusal for
the whole set, never a partial set. The closed wire domains are: status state
`serving`/`draining`; status stage `composed`; runtime stage `authorizing`/
`running` (the `inherited-unreclaimed` marker is pre-listen only and never
served). Status broker and runtime records must carry the attested PID, start
time, endpoint and executable identity; a zero or mismatched PID is refused.
Sharing ranges and content name the producer's own bounds, so the reader can
never be narrower or wider than the writer: duration maxima and the linger
fit (linger plus runtime shutdown plus handoff grace within a time.Duration)
come from the configuration bounds, and fatal marker content (1-16 entries,
non-empty, at most 512 bytes, NUL-free) and millisecond backoff caps come
from the catalog bounds. A present marker list obeys the full 1..16
producer cardinality: omission is legal, a present empty list refuses. The
wire accepts the union of TOML-derived and catalog-derived sharing: catalog
entries legitimately use zero restart limit and backoff seconds for
"no restart" / "no backoff" and omit fatal markers and millisecond fields
without a supervision section. A transport failure after a peer is
established (a hello write or read failure, a hello timeout, a malformed
frame) is a typed observation failure; only a pre-peer failure (no socket,
connection refused) falls back to the absent-broker record status. Provider pressure
policy keeps its closed actions, ordered thresholds and observation-path
shape. Lease rows keep `client_process` lifetime, `held`/`held(stale)`
state, the resolved engine/runtime/profile/endpoint identities and the
attested runtime PID; lease bearer tokens never appear on the wire. Lease
ages and broker timestamps are informational observations without a closed
range and are not cross-checked. Broker PGID/SID/UID, runtime CWD, exec-plan
digest and pre/post-exec shapes are broker-echoed diagnostics bound by the
PID/start/executable/endpoint identity, not independently cross-checked.
