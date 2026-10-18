# `curator-engines` broker protocol v1

Status: **normative extraction contract**. Source: `relux-agents-infra` `origin/main` at `d7d727880a65183836362248a689eeaf163ae1e1`, `tools/agents-infra/internal/infra/pi_shared*.go` and their tests. The CLI name and spelling follow the adopted shape in curator-inference-manager PR #7.

This document names the extracted broker surface v1. The existing private socket field `protocol_version` remains **7** and the status payload `contract_version` remains **1**. These numbers are preserved wire values, not this document's version. The broker remains the host-arbitration owner. The session host does not own runtime admission, leases, release, or cleanup in this contract.

## 1. Commands

The public binary is `curator-engines`. Its lifecycle commands are:

```text
curator-engines status  [--engine NAME] [--config FILE] [--cache-root DIR] [--json]
curator-engines ensure  [--engine NAME] [--config FILE] [--cache-root DIR] [--json]
curator-engines release [--engine NAME] [--config FILE] [--cache-root DIR] --lease-token TOKEN [--json]
curator-engines stop    [--engine NAME] [--config FILE] [--cache-root DIR] [--json] [--force] [--timeout SECONDS]
```

`--engine` selects the existing managed profile entry. If it is omitted, the selected project entry is used. Project engine configuration remains at `.agents/.configs/project-config.toml`; the extraction does not move model files, model configuration, or engine executable configuration. `status --json` returns the v7 status object. `ensure --json` writes the granted lease record, including the token, before holding the lease for the lifetime of the caller process. SIGINT, SIGTERM, process exit, and connection loss release that process's lease. A caller can use the emitted token with `release` from another process. `stop` refuses active leases unless `--force` is supplied.

Catalog entries use zero linger: releasing the last lease starts shutdown
immediately. Release acknowledgement does not wait for broker shutdown.
During shutdown, a connected peer may close before replying to hello; this is
the typed `shared_runtime_unavailable` refusal (including EOF or connection
reset), not proof of broker absence. `--force` does not bypass a failed
post-connection attestation. After shutdown completes and the socket is absent,
`stop --force --json` succeeds with `state: "absent"`, zero/omitted PIDs and both
termination flags false. Without `--force`, absent-broker stop refuses with
`shared_runtime_broker_unreachable_use_force`. A reachable broker with active
leases refuses non-forced stop with `shared_runtime_leases_active` and retains
those leases.

`--config FILE` selects an absolute catalog path using the same private catalog
permissions and strict reader as the default `$HOME/.curator/engines.toml`.
A relative config path refuses with `engine_catalog_read_failed` (site
`catalog.selection`); a missing explicitly selected catalog or entry also
refuses there, without legacy fallback. `--cache-root DIR` selects an existing
absolute state/cache directory; relative paths refuse with
`shared_runtime_state_path_invalid`. It controls broker state, leases and the
rendezvous socket. Omitting either flag preserves that path's existing default.
The client passes the selection to the broker and the broker passes it to the
runtime launcher. Canonical rendezvous socket paths longer than 103 bytes refuse
with `broker_rendezvous_path_too_long`, naming the actual length and limit.
No alternate socket location is chosen.

The catalog location is not itself part of runtime or engine identity. Pinned
relative model paths resolve against the selected catalog directory, so relocating
such a catalog can legitimately change the expanded argv and runtime identity.
Darwin executable attestation obtains the inspected PID's absolute executable
vnode path via `PROC_PIDPATHINFO`, independently of the invocation spelling in
`kern.procargs2` or argv[0]; the same device/inode equality checks still apply.

`release` uses a bearer `lease_token`; a visible `lease_id` is not authority to release. The existing owner-connection release message by `lease_id` remains for orderly cleanup by the process that owns that connection. Tokens are distinct from lease IDs, compared as bearer values, and omitted from status lease rows. A valid external release removes that lease and notifies its owner connection with `released` so a waiting `ensure` exits cleanly.

CLI argument errors exit 2. For broker process results, exit 75 remains `broker_election_lost`, 76 remains `runtime_listener_occupied`, and 77 remains `shared_runtime_quarantined`; ordinary successful execution exits 0 and other errors retain exit 1.

## 2. Socket framing and identity

The broker listens on a same-UID Unix-domain socket. Each frame is one JSON object terminated by LF, with a maximum total frame size of 65,536 bytes. Unknown JSON fields, malformed frames, and trailing JSON are rejected with `protocol_violation`. Peer UID, PID, executable identity, process start time, runtime key, profile digest, engine identity, runtime executable, runtime process, endpoint, and model readiness are attested before a client may use the broker.

The hello request keeps its current fields: `type`, `protocol_version`, `client_pid`, `client_exec`, `runtime_key`, `profile_digest`, `project_key`, `profile_key`, `run_id`, and `sharing_configured`. V1 adds `engine_identity`. The `hello_ok` response keeps `type`, `protocol_version`, `runtime_key`, `profile_digest`, `sharing_effective`, `broker`, and `runtime`, and adds `engine_identity`.

`engine_identity` is an object with:

| Field | Meaning |
| --- | --- |
| `name` | Selected engine entry name |
| `key` | Existing SHA-256 runtime key used for broker arbitration and shared state |

The key is the physical arbitration identity. Two names that resolve to the same runtime configuration continue to share the same broker and state root.

## 3. Wire messages and fields

The existing wire envelope retains all of these JSON field names and omission rules. Fields with `omitempty` are absent when empty or zero; the remaining fields retain their existing zero-value encoding. Message types are `hello`, `hello_ok`, `acquire`, `lease`, `heartbeat`, `heartbeat_ok`, `release`, `released`, `status`, `stopping`, `refused`, `shutting_down`, `lease_revoked`, and `runtime_exited`.

| JSON field | Existing meaning |
| --- | --- |
| `type` | Message type |
| `code`, `reason`, `mismatch_field` | Refusal or termination details |
| `protocol_version` | Existing socket protocol version, 7 |
| `client_pid`, `client_exec` | Client process attestation |
| `runtime_key`, `profile_digest` | Existing runtime and configuration identities |
| `project_key`, `profile_key`, `run_id` | Client state identity |
| `sharing_configured`, `sharing_effective` | Configured and broker-fixed sharing policy |
| `broker`, `runtime` | Attested broker and engine-process records |
| `lease`, `leases`, `lease_id`, `lease_count` | Lease grant, status rows, identifier and count |
| `client_key`, `requested_at` | Acquisition identity and timestamp |
| `force`, `timeout_seconds` | Stop request options |
| `state`, `stage` | Broker lifecycle state |
| `effective_max_leases`, `configured_max_leases` | Lease-limit refusal details |
| `broker_pid`, `broker_start_time` | Broker identity refusal details |
| `resources` | Resource admission and status facts |

`engine_identity` and `lease_token` are the only new fields on the wire envelope. `lease_token` is used on a token-based `release`; it is not emitted in `status.leases`. The nested `broker`, `runtime`, `lease`, `leases`, sharing, and resource objects retain the exact JSON field names and value shapes in the cited v7 source.

The public status JSON keeps the existing direct fields `contract_version`, `runtime_key`, `profile_digest`, `endpoint`, `restart_count`, `restart_not_before`, `quarantined_until`, `last_readiness_match`, `manual_quarantine`, `half_open`, `failure_history`, `resources`, `sharing`, `broker`, `runtime`, `leases`, `attestation`, and `paths`. V1 adds `engine_identity`. Its status contract version remains 1. Lease status rows retain all prior fields but omit `lease_token`.

## 4. Lease record

The lease record retains the existing fields `lease_id`, `run_id`, `client_key`, `client_pid`, `client_start_time`, `runtime_key`, `profile_digest`, `endpoint`, `runtime_pid`, `granted_at`, and `last_heartbeat_at`. V1 adds:

| JSON field | Meaning |
| --- | --- |
| `lease_token` | Independent random bearer credential returned only to the grantee and accepted by external release |
| `lease_lifetime` | `client_process`; the lease exists while the attested owner connection/process remains live |
| `engine_identity` | Engine name and physical runtime key |

The broker arbitrates lease capacity before granting a lease. A refusal never evicts a busy engine or invalidates another client's lease. Release removes only the lease authorized by that connection or token. Broker-observed client exit, crash, disconnect, cancel, or forced shutdown reclaims the corresponding lease. The runtime is drained according to the configured linger and shutdown policy after its last lease is gone.

## 5. State ownership and roots

The extracted broker exclusively owns the new state namespace:

```text
<OS user cache>/curator-engines/shared-runtimes-v1/<runtime-key>/
<OS user cache>/curator-engines/r1/<runtime-key-prefix>/b.sock
```

The legacy agents-infra broker retains its old namespace under `agents-infra/pi-runtimes` and `agents-infra/pi-rv`. The old and new brokers therefore do not open, lock, or publish into the same state or rendezvous root. The cache base directory and engine/model configuration locations remain the ones already selected by the host environment and project configuration.

## 6. Exit and refusal vocabulary

The extracted implementation preserves the source's exit mapping: election lost 75, occupied runtime listener 76, quarantined runtime 77, ordinary errors 1. The public CLI uses 2 only for malformed command arguments.

The following source `code` values are preserved; their existing circumstances and response details are not redefined by this extraction:

```text
broker_build_identity_mismatch
broker_election_lost
broker_executable_identity_mismatch
broker_identity_mismatch
broker_identity_unavailable
broker_not_session_leader
broker_peer_untrusted
broker_protocol_version_mismatch
broker_rendezvous_bind_conflict
broker_rendezvous_path_too_long
broker_start_failed
broker_start_timeout
broker_stop_identity_mismatch
broker_terminated
broker_unreachable
invalid_project_configuration
invalid_runtime_stop_timeout
pi_deadline_exceeded
pi_execution_identity_changed
pi_execution_environment_invalid
pi_execution_environment_malformed
pi_profile_busy
pi_start_failed
protocol_violation
runtime_authorization_mismatch
runtime_executable_invalid
runtime_exited_early
runtime_identity_mismatch
runtime_launch_identity_mismatch
runtime_launch_identity_unresolvable
runtime_launch_unauthorized
runtime_listener_check_failed
runtime_listener_occupied
runtime_model_unavailable
runtime_readiness_invalid
runtime_readiness_timeout
runtime_shutdown_timeout
runtime_start_failed
shared_runtime_active
shared_runtime_broker_unreachable_use_force
shared_runtime_endpoint_mismatch
shared_runtime_identity_mismatch
shared_runtime_lease_limit
shared_runtime_leases_active
shared_runtime_not_configured
shared_runtime_orphan_unidentifiable
shared_runtime_owner_unidentifiable
shared_runtime_peer_start_timeout
shared_runtime_platform_unsupported
shared_runtime_profile_mismatch
shared_runtime_quarantined
shared_runtime_resource_policy_mismatch
shared_runtime_resource_pressure
shared_runtime_resource_unknown
shared_runtime_shutting_down
shared_runtime_state_path_invalid
shared_runtime_state_unreadable
shared_runtime_status_undecodable
shared_runtime_status_unsupported_contract_version
shared_runtime_unavailable
unknown_pi_profile
```

## 7. V1 change notes

- Extracted the versioned `pi_shared` broker and lifecycle as the v1 implementation; kept socket `protocol_version = 7`, status `contract_version = 1`, existing refusal codes, exit codes, and existing JSON names.
- Added `engine_identity` to hello, hello response, broker state, status, and lease records so the requested entry and arbitration key are explicit.
- Added independent `lease_token` and `lease_lifetime = "client_process"` lease fields. Added token-authorized external `release`; status omits the bearer token.
- Moved only the broker's ownership namespace to `curator-engines`; old agents-infra broker state remains disjoint.
- Broker arbitration remains the owner for busy-engine protection, lease-based release, and cleanup on cancel and crash. Session-host ownership is outside this contract.

## 8. Decision 0020 link

Curator-spec issue [#104](https://github.com/relux-works/curator-spec/issues/104) is the decision/ratification location. The implementation PR must link this published spec from issue #104 after the PR exists; implementation does not wait for ratification. This worktree handoff has not published a PR, so that link is a downstream publication step.
