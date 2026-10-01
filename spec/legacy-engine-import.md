# Legacy engine and terminal-evidence import

`curator-engines import-legacy preview --config FILE --evidence-root DIR` reads an explicitly selected legacy model-harness/MLX TOML and lifecycle-envelope directory. Omit a source to leave that surface alone. `apply`, `resume`, and `rollback` use the same source selection; source disappearance never deletes imported values. `--home` and `--cache-root` support disposable validation homes. This is Process B import; it does not migrate Process A, catalogs of runtime/model registrations, authenticated homes, credentials, live processes, leases or sockets.

The importer uses the adopted engine catalog v1 validator and terminal summary reader. Source/target files are bounded to 1 MiB, owner-controlled, regular, single-link files. Read-only profile sources may be readable by others (for example a repository fixture at mode 0644), but group/other writes refuse. The HOME container may have ordinary mode 0755; group/other writes refuse. Manager destinations, transaction artifacts and lifecycle directories remain private. Existing destination catalog symlinks may resolve to a private regular catalog; apply replaces only the selected link, backs up its link text, and rollback restores that exact link without modifying its referent. Source symlinks refuse. Unknown profile fields and wrong types refuse. A successful absent source is neutral; an unreadable/malformed source is a refusal, never absence.

## Field inventory

Each legacy field is mapped exactly once below. No runtime/Pi client field is silently translated into an engine profile. New catalog-only fields are not accepted as legacy input.

| Legacy profile field | Destination / rule |
| --- | --- |
| profile table name | `engines.<name>`, unchanged; existing catalog name validation |
| `mode` | `mode`, unchanged |
| `engine` | `engine`, unchanged; omission retains the legacy `mlx-lm` default, including SSH metadata |
| `model` | `model`, unchanged |
| `executable` | `executable`, unchanged; console scripts remain non-runnable with `script_executable_unsupported` |
| `argv` | unchanged only after credential refusal; catalog rendering keeps whole-token endpoint substitution and its existing identity-option rules |
| `ssh_executable` | `ssh_executable`, unchanged |
| `ssh_target` | `ssh_target`, unchanged |
| `remote_executable` | `remote_executable`, unchanged |
| `remote_config` | `remote_config`, unchanged |
| `remote_profile` | `remote_profile`, unchanged; never guessed to be a served model ID |
| `remote_host` | `remote_host`, unchanged; adopted loopback validation |
| `remote_port` | `remote_port`, unchanged |
| `knobs.kv_context_tokens` | `knobs.kv_context_size`; positive strings normalize to integers, permitted `unbounded` remains a string |
| `knobs.prefill_chunk_tokens` | `knobs.prefill_chunk_size`; positive strings normalize to integers; unbounded refuses under the adopted validator |
| `knobs.reasoning_effort` | `knobs.reasoning_effort`, unchanged |
| `knobs.speculative_decoding` | `knobs.speculative_decoding`, unchanged |
| `stress.prompt_tokens` | `stress.prompt_tokens`, unchanged |
| `stress.max_output_tokens` | `stress.max_output_tokens`, unchanged |
| `stress.startup_timeout_seconds` | `stress.startup_timeout_seconds`, unchanged |
| `stress.request_timeout_seconds` | `stress.request_timeout_seconds`, unchanged |
| `stress.sample_interval_milliseconds` | `stress.sample_interval_milliseconds`, unchanged |
| `supervision.fatal_output_substrings` | same field; existing supervision mapping |
| `supervision.restart_on_failure` | same field; false means restart limit zero |
| `supervision.max_restarts` | same field; existing restart-limit mapping |
| `supervision.restart_window_seconds` | same field; existing stable-run reset mapping |
| `supervision.restart_delay_milliseconds` | same field; existing constant-backoff mapping |
| `pinned_distribution.package` | same field |
| `pinned_distribution.site_packages` | same field |
| `pinned_distribution.commit` | same field |

The importer derives the additional catalog `served_model_id` using the engine's identity convention. For llama-cpp it uses the first legacy alias (first comma-separated alias), then the model basename. For mlx-swift it uses the first `--model-id`, then the model basename. mlx-lm retains the complete non-path model/repository name; it never guesses a served identity from a local model path. An SSH profile's remote served ID is unknown until explicitly configured. Unknown identity imports without `served_model_id`, with a named non-runnable reason; status stays unknown. Executable availability is inspected without launching it. Engine kind is never inferred from executable spelling.

| Legacy lifecycle field | Destination / rule |
| --- | --- |
| `schema_version` | validated source envelope v1; summary `schema_version = 1` |
| `entry_id` | summary `session_identity_digest`; raw identity stays in the source |
| `created_at` | validated chronology only; retained read-only envelope |
| `closed_at` | summary `closed_at` |
| `committed_bytes` | validates complete log bytes; retained read-only envelope |
| `committed_records` | validates complete log records; retained read-only envelope |
| `directory_device` | validates source directory identity; retained read-only envelope |
| `directory_inode` | validates source directory identity; retained read-only envelope |
| `log_device` | validates source log identity; retained read-only envelope |
| `log_inode` | validates source log identity; retained read-only envelope |
| `active_device` | validates existing active-lock identity; retained read-only envelope |
| `active_inode` | validates existing active-lock identity; retained read-only envelope |
| event `timestamp` | validated chronology; retained read-only log |
| event `event` | validated terminal ordering; retained read-only log |
| event `fields.status` | summary `terminal_status`, only admitted terminal values |
| other event `fields` | retained read-only log, never copied into manager state |
| source directory path | summary `source_path_digest`, never a copied path |
| held flock, process/lease/socket or unknown state | refusal; no process intervention, transfer, relabeling or quarantine |

Summaries are stored in the private manager-owned `~/.curator/terminal-evidence.json`. `curator-engines legacy-evidence DIR` uses the same read-only reader. Duplicate lifecycle identities refuse before any import; no ordering rule chooses between competing records. There is no implicit retirement action.

## Transaction and recovery

Credential-bearing argv refuses in preview and every import/recovery attempt, without stripping or rewriting source tokens. Credential flag components (`api-key`/`api_key`, `token`, `secret`, `password`, `auth`/`authentication`/`authorization`, `bearer`, `credential`) match case-insensitively with dash/underscore boundaries, in separated and equals forms. Normal token-count options such as `--max-tokens` remain data. Known secret prefixes (`sk-`, `ghp_`, `xoxb-`/`xoxp-` and related Slack prefixes) and three-part base64url JWT-shaped values also refuse. The preview reports only the recognized flag component, never its value. This recognizes these declared shapes; it does not attest that arbitrary opaque strings are secret-free.

Every closed JSON object uses its production type's canonical tags as an exact, case-sensitive key set, before Go struct decoding. This includes journal/target/state objects, lifecycle envelopes/events and existing imported summary arrays. Duplicate keys and trailing bytes refuse recursively. Nonterminal event `fields` remain the existing open map and stay only in the read-only legacy log; terminal fields retain their explicit closed set. Each lifecycle directory inventories all children while holding its active lock: only `active.lock`, `record.json`, and `log.jsonl` are admitted, and their existing regular-file/owner/mode/symlink/link-count checks still apply. Unknown files, directories, sockets, FIFOs or symlinks refuse.

Runtime namespaces inventory every child. Only the opaque `BrokerLog` and `RuntimeLog` files declared by the manager layout are exempt from JSON validation. `RestartLedger` and `BrokerState` use the existing owner types and exact, case-sensitive JSON tags recursively, including nested pointer objects. Required/non-null members are derived from those types (optional `omitempty` fields may be absent; nullable pointers and history may be null). Invalid schema/identity, directory-key mismatch, negative counters, excessive history, zero timestamps and inconsistent/invalid failure chronology refuse as typed unknown state. A broker record has no terminal state in the owner protocol and always refuses admission, regardless of PID liveness; no PID is probed or signaled. An unknown sibling state file refuses rather than being treated as a log.

A restart ledger with a pending retry (including an overdue retry), half-open attempt, readiness timestamp without terminal evidence, future failure, future quarantine (including a failure-history window), a positive restart count without terminal evidence, or failure history without a terminal quarantine/manual-quarantine marker cannot establish inactivity and refuses as active or unclassifiable. A fresh ledger or expired terminal quarantine history may pass admission; its bytes stay read-only. Failure history age alone never proves cold. This admission check does not invent a restart scheduler or transfer historical ledger ownership.

Admission is revalidated before staging, before recovery, immediately before each target replacement and before committing. A late admission failure unwinds only the importer's digest-verified targets to their recorded before state; missing evidence or drift refuses the unwind rather than guessing. An already-active source/destination refuses without writes. This is snapshot revalidation, not a shared exclusion lock with legacy brokers: external non-cooperating state changes between the final observation and the atomic replacement remain a concurrency bound.

Only `engine-catalog` and `terminal-evidence` target IDs resolve to destination paths. The frozen `manager-legacy-import-journal/v1` grammar is checked before recovery. It rejects duplicate JSON members, trailing bytes, unknown members, missing required fields, oversized input, duplicate target IDs and inconsistent backup digests. Journal schema and conformance vectors are retained in `internal/infra/testdata/manager-journal-grammar-v1.json`.

Preview creates no lock, staging, backup, journal or temporary file, and launches nothing. Apply creates the private `.curator/.legacy-import/` transaction root and a nonblocking transaction lock. It stages only selected destination backups and after-images, syncs them, installs the prepared journal, and atomically replaces the targets. Journal/backup evidence contains no lifecycle events. Identical reapply returns before acquiring the lock or writing anything. Equal effective catalog entries preserve existing bytes; divergent entries refuse unless the entire current catalog still matches this importer's last committed output digest. Unrelated entries are retained. Importer-owned terminal summaries may be refreshed; identities absent from the new source remain retained. A differing unowned summary file refuses.

Recovery validates every target and artifact before replacement. A target must match its recorded before/after state exactly; a committed journal requires every target to match after. Missing/corrupt backups, missing/corrupt staging, source digest changes and target drift refuse. No cleanup follows an ambiguous refusal. Rollback restores file bytes and mode or link text; a newly created target is removed only while it still matches the recorded output. After durable rollback the current journal is removed; backups remain private evidence. A crash before staging leaves no transaction; repeat apply is safe. After the prepared journal or any replacement, resume converges to imported state or rollback restores original state.

Manager cache inspection uses the existing broker namespace and operator status reader. The read-only admission gate also inspects legacy `agents-infra/pi-runtimes` and `agents-infra/pi-rv` cache namespaces; the existing profile resolver calls it before ensure/lease/status/recovery, so an old active or unclassifiable record cannot acquire a fresh manager identity. Active records, held broker locks, sockets, nonempty leases and unclassifiable shapes refuse. Status read failure is unknown. Import never calls ensure, stop, signal, lease creation or quarantine.

## Compatibility commands

The shipped `scripts/qwen-infra` and `scripts/model-harness` shims call `curator-engines compat <alias> <command>`. A legacy `--config FILE` is accepted only when its explicitly selected profile matches that catalog entry; it never supplies a fallback. Every invocation explicitly selects an engine, using `--engine NAME` or model-harness's positional profile name. There is no spelling-based default or provider/profile fallback.

`print-config`/`render` use the catalog renderer; `diagnostics`/`doctor` use the broker status reader; `run` uses the existing process-lifetime lease; `stress` uses that same lease and submits a bounded chat request using the selected stress policy. Actual token usage is server evidence, never inferred from prompt words. `stop`, `quarantine` and `unquarantine` call the existing owner APIs. Unsupported aliases/commands refuse with migration guidance. An unavailable live engine is unknown.
