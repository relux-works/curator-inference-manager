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
| weight-artifact | ObservedValue from broker-verified weights with a representable clean absolute GGUF path for an attested instance; otherwise NotObserved |
| memory-accounting | NotObserved: resource byte counts do not prove artifact-aware accounting |
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
also fail observation). These failures preserve the rest of status and publish
all 17 facts as `not-observed`, with no values: `cause = "malformed"` for
`readings_malformed`/`readings_partial`, or `cause = "read-failure"` for
`readings_read_failed`/`readings_stale`. Each `reason` is the exact typed code
and `source = "unavailable"`. Profile projection failures use the same shape
and cannot be overwritten by later attested readings. V3 already supports this
shape; no envelope member or contract version changes. Consumers can decode
these rows and still refuse observation at their validator while reading broker
availability independently. No partial successful readings are published.
Existing socket/identity refusals continue to propagate unchanged.

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
value. Entry-own values outside the schema fail the readings set with the typed
`readings_malformed` reason while retaining status.

The entire resource-status envelope is validated before any attested reading is
published: required fields and closed JSON member sets (including nested memory,
inference and policy objects), no null or duplicate members, positive observed
memory bytes, explicit reasons for unknown facts, policy action spellings,
representable non-negative eviction grace, ordered positive memory thresholds,
pressure hysteresis consistency and resource/admission classification. Disabled
or unknown policies carry no provider settings. An unobserved source cannot
carry timestamps or observed facts. Invalid numeric/types/member shapes retain
the existing typed `protocol_violation` wire failure; semantic violations use
`readings_malformed` or `readings_partial` inside the failed facts. Status
propagates `protocol_violation` wire failures rather than wrapping them as an
unreachable broker.

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

## Exact weights identity (additive)

Schema v1 catalogs are private regular files, conventionally mode `0600`.
Readers require owner readability and refuse any group/other permission bits;
symlink catalogs are refused. `knobs.kv_context_size` is a positive TOML integer
for llama-cpp (for example `kv_context_size = 16384`, not `"16384"`). The existing
`"unbounded"` spelling remains available only for supporting non-llama engines.

An optional `[engines.NAME.weights]` table has exactly one required string
member, `id`. There is no version key. Its exact case-sensitive grammar is
`^gguf-sha256:[0-9a-f]{64}$`; values are never trimmed or normalized. Whitespace,
uppercase hex, incorrect length, missing/empty/non-string ids are malformed.
A nonempty id with another prefix is unsupported. Unknown members refuse
fail-closed. Catalog refusals retain `EngineCatalogError` JSON:

| Code | Site | Meaning |
| --- | --- | --- |
| `engine_catalog_malformed` | `catalog.weights` | Weights is not a table |
| `engine_catalog_malformed` | `catalog.weights.keys` | Unknown weights member, including version |
| `engine_catalog_malformed` | `entry.weights.id` | Missing or malformed id |
| `engine_catalog_unsupported` | `entry.weights.id.unsupported` | Unsupported id prefix |
| `engine_catalog_unsupported` | `entry.weights.scope` | Not local llama-cpp with a model path |

The id is SHA-256 over the complete bytes of one unsplit GGUF file, prefixed
with `gguf-sha256:`. No filename, size or manifest enters the digest. This
version supports local llama-cpp entries with a `model` file only; SSH/remote
entries, other engines, directories and metadata-declared split files cannot
certify weights. Absence of the table is valid and means unpinned. Legacy import never
invents an id. `MarshalEngineCatalog` preserves the declared id exactly.

`weights compute --config FILE --engine NAME [--json]` streams the resolved
file, prints the id (JSON: `id` and UTC `observed_at`), and never writes the
catalog or launches a process. Relative paths resolve against FILE's directory.
The existing bounded GGUF metadata decoder validates magic `GGUF`, version 2 or
3, and metadata through the same open descriptor and byte stream that feeds
SHA-256. Metadata must fit the existing 32 MiB reader bound; the remaining file
bytes are streamed into the same hash. Filename patterns do not determine scope.
These file refusals apply to compute, pinned check and the ensure gate:

| Code | Meaning |
| --- | --- |
| `engine_weights_not_gguf` | Invalid/truncated GGUF header or metadata, unsupported version, or metadata exceeding the reader bounds |
| `engine_weights_split_unsupported` | Metadata has `split.count > 1` or any `split.no`, including zero |

`check` uses the same resolution and verification; a missing model is unknown
without a weights failure. A mismatch adds `engine_weights_mismatch` to its
existing `failures` array, containing the recorded and actual ids in its
message. Weights catalog validation failures retain the catalog code and site
from the table above as `failures[].code` and optional `failures[].site` in check
JSON. Non-weights catalog failures retain the existing `engine_config_malformed`
(or `engine_config_read_failed`) behavior. Other existing check failures remain
independent.

`ensure` accepts optional `--expected-weights-id ID`, the operator pin from
runtimes.toml. An unpinned entry refuses a pin with `engine_weights_unpinned`.
Pinned launches require equality of pin (when supplied), catalog id and actual
file id. `engine_weights_mismatch` includes `details.expected_weights_id`,
`details.recorded_weights_id` and `details.actual_weights_id`; the expected
field is empty when no operator pin was supplied. Nothing is started on a
mismatch. Compute and launch also refuse missing files with
`engine_weights_file_missing`, unreadable files with `engine_weights_read_failed`,
and non-regular files with `engine_weights_not_regular`.

At hash time the broker records descriptor device/inode, size, mtime and ctime;
identity changes while opening/hashing or after readiness refuse with
`engine_weights_file_changed`. After a ready-time change it terminates and
reaps the process it started before returning the refusal, with no lease.
Startup weights refusals survive the broker-to-client boundary as their exact
typed code, rather than being converted into restart quarantine.

The verified id and UTC observation timestamp are bound to the running process
record and carried in attested hello/status replies as optional runtime
`weights: {"id":"gguf-sha256:...","observed_at":"..."}`. The weights id also
participates in the catalog profile digest. A shared ensure compares against
that instance's observation and never rehashes the pathname; a disagreeing pin
refuses without changing the running instance or granting a lease. An instance
missing certification for a pinned catalog refuses `engine_weights_unverified`.
Legacy broker records remain valid because this member is optional.
Whenever `runtime.weights` is present, both `id` and `observed_at` are required
and non-null; missing or null members refuse with typed `protocol_violation`.
This runtime object carries only a certified observation and has no
`verification` member. Its required members are conditional on the object
being present, as with the optional nested resource-pressure policy; they do
not depend on the separate top-level status verification state.

Status adds a top-level `weights` object without renaming existing fields or
changing `contract_version = 1` or `protocol_version = 7`:

```json
{"weights":{"id":"gguf-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","verification":"verified","observed_id":"gguf-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","observed_at":"2026-01-01T00:00:00Z"}}
```

`verification` is `unpinned` with no declared id, `unknown` with a declared id
but no attested running observation, `verified` when the instance observation
matches, or `mismatch` for a differing observation or a recorded startup
mismatch. `observed_id` and `observed_at` are emitted only for verified evidence.
Unattested/stale process records cannot produce verified status. A cold status
does not hash large weights. Check JSON adds the same weights shape using its
read-only observation. Launch verification is an observation at startup, not a
continuous filesystem watch, and does not claim the engine's resident-memory
contents. When the broker verified the instance weights, a successful readings
envelope publishes `weight-artifact` as `observed-value`, source
`broker-verified-weights`, with the existing closed-schema JSON string value
`{"format":"gguf","model_path":"/models/model.gguf"}`. No digest or timestamp
is added to this closed value; they remain in top-level `weights`. The path is
cleaned before publication and must be absolute with a `.gguf` extension
(case-insensitive), as required by `weight-artifact/v1`. A verified file with an
unrepresentable path (for example `.bin`) leaves this fact `not-observed` with
cause `unsupported` and an explanatory reason; top-level verification remains
`verified`. Unverified
records do not upgrade this fact. A readings-envelope failure still publishes
only NotObserved facts. This preserves the status self-decode guard and pinned
consumer decoders without changing contract versions.

## Status publication compatibility

The integer status contract remains v1. Before printing JSON, the producer
checks the eleven joint broker state/source pairs, requested engine identity,
required runtime key/profile digest/sharing members, nullable runtime
start-time shape and bounds, complete restart cohorts, and v3 readings
transcription shape (including typed non-observations), and rejects duplicate
or case-variant aliases of the top-level members the readings consumer reads.
A violation refuses with `status_contract_violation` on stderr and no status
JSON on stdout. The guard mirrors the consumers exactly: like them, it accepts
an omitted `max_leases`, an omitted or null `pid`, and `leases: null`.

`broker.reason` is an optional additive typed code. Record states outside the
consumer vocabulary (including a still-live startup `refused` record) project
to `unverified-stale/record-derived-unverified`, with the recorded startup
failure code when present or `broker_state_unrecognized` otherwise. This pair
claims no admissible live broker evidence; it need not imply process death.
Weights mismatch remains visible in `weights.verification`. Attested states
still pass strict socket/identity validation before publication. No new broker
pair or consumer schema change is required.

## Reasoning control

The closed knob table contains `kv_context_size`, `prefill_chunk_size`,
`reasoning_effort`, `reasoning`, and `speculative_decoding`. For local llama-cpp,
`reasoning = "on" | "off" | "auto"` renders exactly one `--reasoning VALUE` pair.
An omitted knob emits no reasoning flag and preserves engine defaults. Other
values (including explicit empty strings and non-strings) and other engines
refuse with `engine_catalog_malformed`, site `entry.knobs.reasoning`, in both
check and ensure. A knob plus literal `--reasoning` or `-rea` (including inline
`=VALUE`) refuses at `entry.knobs.reasoning.argv`. The check argv table accepts
separated `--reasoning`/`-rea` and `--reasoning-budget` values. Semantic value and
conflict validation applies only to the selected entry. The unsafe
`--chat-template-kwargs` option remains `argv_unsupported`.

## Verification cache trust

Cold pinned launches may reuse a digest under
`<cache-root>/weights-verification/`. Each atomic JSON entry is `0600` in a
`0700` directory, both owned by the current user. Its exact key is resolved
absolute path, device, inode, size, mtime in nanoseconds, and ctime in nanoseconds.
All fields must match. A miss revalidates GGUF metadata and hashes the entire
file before replacing the entry. Corrupt, unreadable, foreign-owned,
non-regular, symlink or incorrectly permissioned entries are ignored; directory
validation and write failures also fall back to hashing, never verification
success. Comparing the resulting digest with catalog and operator ids remains
fail-closed. `weights compute` and offline `check` deliberately keep full hashing.

The cache defends against accidental changes within filesystem metadata
resolution and against other users through owner, mode and no-symlink checks.
A same-owner `0600` entry with a matching key is authoritative by design, even
if its digest was forged to match the catalog/operator pin. This does not defend
against the same uid: any authentication secret would also be readable by that
uid, which can edit the catalog pin itself. No digest authentication is claimed.

An unprivileged user cannot set ctime directly; rewrites normally change the key
even when size and mtime are restored. HFS+ and other filesystems with 1-second
timestamps can miss a same-size rewrite within the same second. Nanosecond key
fields do not improve the filesystem's actual resolution. Privileged tampering,
filesystem rollback, inode reuse with identical metadata and unreliable metadata
are also outside this guarantee. Descriptor/path checks still run before
readiness succeeds, but do not prevent changes after the final check, prove
residency or provide continuous integrity monitoring.

Operators needing byte-exact verification on every ensure can pass
`ensure --no-weights-cache`. It bypasses cache reads and writes and hashes the
file for that ensure, including attachment to an existing broker; a cold broker
also hashes before launch. This may hash twice on a cold ensure. Default sharing
continues to use the running instance's observation without rehashing. Status
only reports that observation and does not verify bytes, so has no cache flag.
`weights compute` and offline `check` always hash. The catalog weights table
still accepts only `id`; `cache = false` is an unknown key and refuses fail-closed.

## Cache root socket budget

Before state creation, check, ensure and status validate the canonical cache
root against the 103-byte socket-path budget. Missing descendants are resolved
through their nearest existing ancestor. The maximum root length is derived
from the same `curator-engines/r1/<32 hex>/b.sock` path builder used by the broker;
it is currently 44 bytes, including all canonical root bytes. Maximum passes;
maximum plus one refuses `broker_rendezvous_path_too_long`, reporting actual
root/socket lengths and their limits. No state is created on this refusal.
The existing layout and lease rendezvous identity are unchanged. A shorter
versioned suffix could increase the budget, but would require coordinated
rendezvous migration for existing brokers and leases; it is not implemented.

## Build identification

`curator-engines version [--json]` reads `runtime/debug.ReadBuildInfo`. Text and
JSON expose module version, VCS revision, VCS time, VCS modified, and Go version.
The JSON keys are `module_version`, `vcs_revision`, `vcs_time`, `vcs_modified`,
and `go_version`; values are strings, with `unknown` for unavailable fields.
Scratch directories `.temp/` and `.codex/` are ignored by Git so their presence
does not change the build's VCS modified setting.

## Darwin broker code identity

Darwin broker/client authentication compares the 20-byte CodeDirectory hash
(cdhash) of the code the kernel actually runs. A pathname is only a locator:
`proc_pidpath`, `EvalSymlinks` and `stat` cannot bind a file to a running image.
Two copies with the same selected CodeDirectory match even when their device,
inode or installation path differs. Replacing a pathname after a peer starts
cannot substitute different running code for the authenticated image.

A connected Unix socket supplies `LOCAL_PEERTOKEN` as well as `LOCAL_PEERPID`
and `LOCAL_PEERCRED`. The audit token's PID must equal the kernel socket PID;
admission also requires the hello PID to equal that PID, and the client checks
the announced broker PID. `csops_audittoken` binds queries to the token's PID and pidversion,
refusing reuse or exec after that token was observed. Both peer and self use
status → cdhash → status: `CS_OPS_STATUS` (0) must return `CS_VALID` (0x1)
before and after `CS_OPS_CDHASH` (5). Every peer query receives the same token;
self uses `csops(getpid(), ...)`. Bracketing detects invalidation during hash
retrieval, without claiming an atomic process handle or future validity.
[`bsd/sys/codesign.h`](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/sys/codesign.h)
defines the operations and includes
[`kern/cs_blobs.h`](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/osfmk/kern/cs_blobs.h),
which defines `CS_VALID`. No cgo, entitlement or publisher trust is required. These semantics follow XNU
[`csops_internal`](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_proc.c)
and [`audit_token_t`](https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/mach/message.h).

Missing self or peer cdhash, an all-zero hash, invalid signature status, and syscall failures fail closed
with `broker_identity_unavailable`. A code mismatch is
`broker_executable_identity_mismatch`; token/socket/hello PID disagreement is
`broker_identity_mismatch`. Existing UID failures remain
`broker_peer_untrusted`. There is no unsigned-self compatibility mode: unsigned
Darwin amd64 executables must be ad-hoc signed before using this broker.
Go's normal Darwin arm64 linker output is already ad-hoc signed.

The broker saves the exact authenticated token's PID (word 5) and pidversion
(word 7). On the same connection it re-reads `LOCAL_PEERTOKEN` after each
subsequent request is decoded, before dispatch, and again at state-changing
boundaries (resource admission, lease reservation/publication, heartbeat,
release and stop). A changed PID or pidversion refuses with
`broker_identity_mismatch`; an unreadable/truncated token refuses with
`broker_identity_unavailable`. Either refusal closes the connection. Lease
cleanup on disconnect remains permitted. These rechecks bound observation
windows; they do not make check and effect atomic.

**Accepted pre-authentication limitation:** `LOCAL_PEERTOKEN` returns the
current incarnation of the socket's last PID, rather than the incarnation
that queued a message (XNU
[`uipc_usrreq.c`, `LOCAL_PEERTOKEN`](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/kern/uipc_usrreq.c)).
Messages queued before authentication by a process that then execs into the
trusted executable are attributed to that executable if authentication
observes the new incarnation. Saving and rechecking pidversion cannot detect
that earlier exec. This is accepted for now because the attacker has the same
UID and can already run the genuine client with arbitrary arguments. Closing
this attribution gap requires a broker challenge (nonce) in hello, binding
requests to a fresh authenticated exchange. That is a wire protocol change,
recorded as an [open follow-up](../UNRESOLVED_QUESTIONS.md#broker-hello-challenge);
this change preserves the existing protocol.

| Site | Previous executable check | Current decision |
| --- | --- | --- |
| `ownResolvedExecutableIdentity` (broker startup and broker/runtime launcher selection) | `os.Executable` → resolved file identity | Require self kernel cdhash; retain path/stat only for launch location and metadata |
| `sharedUnixPeerIdentity` (admission and client attestation) | Socket UID/PID only | Also require audit-token-bound peer cdhash equal to self |
| `processExecIdentity` | Kernel executable path → resolved device/inode | Require pidversion-bound kernel code equality; retain path/stat as metadata only |
| `sharedBrokerServer.attestClient` | Client device/inode equal to broker record | Socket code equality plus process incarnation/liveness and hello PID; no inode equality |
| `connectAndAttestSharedRuntimeWithOwnership` | Peer inode equal to self; announced file identity equal to self | Socket code equality and process recheck; announced file identity does not authenticate code |
| `sharedRuntimeBrokerCandidates` | Candidate device/inode equal to self | Kernel code equality and existing UID/liveness/argv filters; signed copies qualify |
| `stopRecordedSharedRuntimeWithDependencies` | Recorded broker device/inode equal to self | Kernel code equality before signaling, retaining UID/start-time/argv checks |
| Runtime FILE expectation in broker publication/client attestation | Configured executable stat, then file identity equal to runtime record | **Deferred:** retain existing path identity; this is not race-resistant running-code attestation |
| Runtime reclaim/authorization | Process start time, process group, expected path/argv and exec-plan digest | Retain existing contract; these do not authenticate a broker/client executable |

Non-socket operator inspection acquires the kernel pidversion through
`PROC_PIDUNIQIDENTIFIERINFO`, rechecks the observed process start time, then
uses `csops_audittoken`. It does not obtain a transferable process handle:
signaling still has the existing PID-based check-to-signal limit.

The pure-Go expected-FILE reader supports thin Mach-O and an unambiguous slice
of the requested CPU architecture. It reads `LC_CODE_SIGNATURE`, SuperBlob and
CodeDirectory bounds and hashes the complete selected directory. Structural
checks follow XNU
[`cs_validate_codedirectory`](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.1.9/bsd/kern/ubc_subr.c#L340):
scatter entries require a bounded sentinel and no page-count overflow; team
strings require a bounded NUL; enabled linkage data must fit. Versioned
codeLimit64, execSeg, runtime and linkage headers must be complete. Supported
page shifts are 12 or the host kernel page shift, as in XNU; numeric file
limits and segment fields are not offsets into the CodeDirectory. Selection
follows XNU [`cs_validate_csblob` and `hash_rank`](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/ubc_subr.c):
SHA-384, full SHA-256, truncated SHA-256, then SHA-1, truncating the digest to
20 bytes. Equal current-best rank, unknown hash/version, malformed bounds,
missing signature and missing/ambiguous architecture refuse. This reader is
not wired into runtime FILE checks in this change: arbitrary runtimes may be
unsigned or interpreted, and exact CPU subtype selection and the entire
launch/authorization/reclaim policy need a separate contract. It does not
validate page hashes or certify that a file is executable or publisher-trusted.

Cdhash stays internal. `FileIdentity`, hello/status records, lifecycle evidence,
status contract v1 and the frozen attestation gate names remain unchanged.
`broker_build` now records the already checked kernel code equality rather
than trusting announced file metadata; `broker_build_identity_mismatch` has no
remaining emission site. The status publication compatibility guard and
consumer decoders receive no new member. Serialized file metadata describes
a path observation, not the running image. Linux, Windows and other builds
retain their existing behavior through Darwin-only build tags.

Ad-hoc signatures prove code identity, not publisher identity. Same-code
copies are trusted equally, and a same-UID attacker able to cause the broker
to run attacker-controlled code, inject code, or inherit/pass an authenticated
socket is outside this comparison's guarantee. This closes the pathname
replacement race for broker/client authentication; it does not claim to close
the explicitly deferred runtime FILE checks or PID-based signaling races.
