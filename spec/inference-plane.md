# curator-inference-manager: inference plane

Status: **DRAFT**, 2026-09-27. Not normative. This document collects what earlier drafts and decisions already say about the inference plane and adds no design of its own, except for the adopted curator-engines contract below. The engine catalog schema v1 and supervision mapping in §3.1 and §4 are **adopted by curator-engines v0.x; Decision 0020 pending in curator-spec**. The sources are the launch-profiles draft (`skill-project-management/.specs/drafts/launch-profiles.md`, cited as *LP*), the ecosystem roadmap (`wiki/roadmap/ecosystem-roadmap.md`, cited as *roadmap*), the command-line shape the operator adopted on 2026-09-27 (`wiki/research/cli-shaping-review.md`, cited as *CLI*), which renames the commands and engine entries of LP, and orchestrator decision 01 for `TASK-260929-9amima` (cited as *Decision 01*). §11 is the change log. The decision record for the full plane will be **Decision 0020** in curator-spec, which is not written yet (LP-D7, LP-D13, roadmap §3 M5).

---

## 1. Place in the ecosystem

- Owners today: Curator owns context and managed homes; `curator-run` owns human interactive launches; the agents-management module (`skill-agents-management`) owns runtimes, models, launch plans and limits; ax owns session durability; `relux-agents-infra` is a deprecated residual that this work empties (LP §0).
- None of these owners can take engines: Curator's boundary excludes them, the launcher lists them as a non-goal, the module's contract keeps it out of running processes, and ax's scope leaves them out. So the residual's engine broker becomes a provider: a fifth plane, the **inference plane**. The provider is built from this repository, `curator-inference-manager`, and its binary is `curator-engines`, so people and tools reach it as `curator engines …` (LP-D7; CLI §3.3).
- Today the module's `pkg/inferenceengine` observation contract names `ExecutionOwner = agents-infra`, and its `localruntime` reads `agents-infra runtime status` (LP §1, module `v0.5.16` row).

## 2. The provider

| Item | Statement | Source |
| --- | --- | --- |
| Binary and dispatch | `curator-engines`, named after the noun people type: `curator engines …` dispatches to it, since Curator dispatches purely by executable name. The repository keeps its name. There is no `curator inference` dispatcher | CLI §2.6 item 8, §3.3 |
| Commands | the catalog: `curator engines list`, `show <engine>`, `add`, `remove`, `check`, `bench`, `confirm`; the engines themselves: `curator engines status`, `ensure`, `release`, `stop`, each with `[--engine NAME] [--json]` (formerly `curator-inference-manager status\|ensure\|release\|stop [--profile NAME]`) | LP-D7; CLI §3.3 |
| Confirmation | adding an engine is a widening entry, because an engine entry names an executable and its arguments: it takes effect only after `curator engines confirm`, which refuses to run in an agent session, and so does any other widening edit from an agent session (relux-works/curator-trust `spec/trust.md` §10; curator-playbook `spec/process-configuration.md` §2.3) | CLI §3.3 |
| Installation | a first-party provider ships beside `curator` in its signed release; otherwise `curator provider install engines` puts it on a trust root (curator-spec issue #100, curator issue #96) | CLI §4 |
| Discovery | installed on a trust root. Umbrella providers `curator-<name>` resolve from trust roots (install dir, `provider_directories`), never from `~/.local/bin`: that is Curator's user-bin shim directory, and a provider found there is refused (`subcommand_provider_untrusted`) | LP-D7; LP §1; LP-D16 |
| Curator coupling | knows nothing of Curator beyond discovery; Curator stays engine-blind | LP-D7 |
| Contracts consumed | the module's `pkg/inferenceengine` contract and the `localruntime` status shape; one executable name changes in `localruntime` | LP-D7 |

## 3. Engine entries and the catalog

- **Names.** LP calls an entry of this catalog an "engine profile". The word `profile` now names only the Curator environment profile, so this document says **engine entry**, and the command-line flag is `--engine` (CLI §2.4, §3.1 rule 3).
- Catalog file: `~/.curator/engines.toml`, table `[engines.<name>]` (LP-D2, LP-D7; LP §6 closed decision 1). It pairs with the spawn-plane catalog `~/.curator/runtimes.toml`, table `[runtimes.<id>]`; "the two names read as a pair" (LP-D2). The operator's files live together under `~/.curator/`, which moves LP-D2's `$XDG_CONFIG_HOME/agents-management/runtimes.toml` (curator-playbook `spec/process-configuration.md` §2.3; CLI §2.9 item 2).
- An engine entry carries the fields of today's `model-harness` `[profiles.<name>]`: `engine`, `model`, `executable`, `argv`, `knobs`, `mode = "local"|"ssh"`, locality-specific SSH fields, `stress`, `supervision`, and `pinned_distribution` (LP-D7; Decision 01). Schema v1 adds the optional `served_model_id`; it is required for an entry to be runnable.
- A runtime binding references an engine entry by name. For example, the provisional `runtimes.toml` sketch has `engine = { plugin = "llama-cpp", name = "<name>" }`, where LP wrote `profile = "<name>"` (LP-D2, LP-D7; CLI §3.1 rule 3).
- The pi client half of today's profiles (`thinking`, `compat`, `compaction`) does not move here. It moves to the managed pi home and the `pi-native` plugin (LP-D7).
- Engine entries live in a 0600 machine file. Board text and digests carry runtime ids only, and run manifests carry nothing private: no paths, sockets, endpoints with credentials or PIDs (LP-D11).

### 3.1 Adopted curator-engines schema v1

- `~/.curator/engines.toml` has `schema_version = 1` and one or more `[engines.<name>]` tables. TOML keys are case-sensitive: the reader checks the exact closed key set at the document, entry, and nested-table levels before decoding values. Each selected entry is a lossless field transcription of the legacy model-harness profile: `engine`, `model`, `executable`, `argv`, `knobs`, `mode`, `ssh_executable`, `ssh_target`, `remote_executable`, `remote_config`, `remote_profile`, `remote_host`, `remote_port`, `stress`, `supervision`, and `pinned_distribution` (Decision 01). The `served_model_id` field is optional catalog data, but an entry without it is non-runnable and reports `engine_availability = "unknown"` with reason `served_model_id_missing`.
- A local legacy profile that omits `engine` is transcribed with the explicit catalog value `engine = "mlx-lm"`, the legacy default. Engine kind is never inferred from the executable name. This matters for older profiles whose executable name happens to identify another engine.
- The legacy knob names `kv_context_tokens` and `prefill_chunk_tokens` are encoded as `knobs.kv_context_size` and `knobs.prefill_chunk_size`. Positive numeric legacy strings are written as TOML integers and read back as canonical base-10 strings, matching the legacy launch formatter; non-canonical spellings such as `+4096` or `04096` normalize to `4096`. `kv_context_tokens = "unbounded"` stays a string only for engines where legacy accepts it and emits no flag. Legacy rejects `prefill_chunk_tokens = "unbounded"`; v1 refuses it and the renderer never sends a non-integer knob value. The renamed keys keep the file readable by skill-agents-management `pkg/runtimecatalog` v0.5.30, whose credential-key scan rejects non-integer values under keys ending in `tokens`.
- Local entries use `engine = "mlx-lm"|"mlx-swift"|"llama-cpp"`, an absolute `executable`, optional `model`, and `argv` with exactly one whole-token `{host}` and `{port}`. Any embedded occurrence is malformed; resolution substitutes only whole tokens. Runnable entries explicitly declare `served_model_id` matching `[A-Za-z0-9][A-Za-z0-9._:/@+-]*`; commas, whitespace, and `=` are refused. This catalog reader does not derive the id. Legacy import work must write an id it can establish and leave the entry without an id when it cannot; launch never parses model flags to guess one. Readiness treats the declared id as a claim and requires it to be present in `/v1/models` before the engine is available.
- The `engine` entry-name grammar is `[A-Za-z0-9][A-Za-z0-9._-]*`. The compact name (lowercase with `.`, `_`, and `-` removed) may not contain these credential-key stems: `accesskey`, `apikey`, `authorization`, `bearer`, `clientsecret`, `cookie`, `credential`, `jwt`, `passphrase`, `passw`, `privatekey`, `privkey`, `pwd`, `secret`, `sessionid`, `sessionkey`, `sessionsecret`, `sessiontoken`, `signingkey`, and `token`. `pass` is refused only as a field token. `auth` is refused as a field token, as a compact suffix, or as a compact prefix when the remainder contains another credential stem. Names whose compact form contains `token` and ends in `tokens` are refused. Field-token detection splits on punctuation and lower-to-upper or digit-to-upper boundaries. These are the exact name checks used to keep entry names readable by skill-agents-management v0.5.30; the entry body also uses the closed schema key set. Syntax and document-structure errors (including an invalid name or a missing, empty, or untrimmed `engine` in any entry) refuse the document. A selected entry's semantic errors do not poison a different safe entry.
- SSH entries preserve the legacy SSH target, executable, config, profile, loopback host, and remote port fields, and include the required non-empty `engine` kind for the skill-agents-management co-reader; the kind is metadata and does not alter launch argv. SSH entries declare the remote server's `served_model_id` explicitly and do not declare local model, knobs, executable, argv, stress, supervision, or pinned-distribution fields. The SSH launch argv keeps the legacy order and options: `-tt`, `BatchMode=yes`, `ExitOnForwardFailure=yes`, `ServerAliveInterval=15`, `ServerAliveCountMax=3`, then `-L`, target, and the shell-quoted remote command.
- Catalog resolution runs for an explicitly supplied `--engine` and for the name selected by `agents.pi.primary_session.profile` when the flag is omitted. Legacy Pi identity/argv is read only to check a same-name pair; any disagreement refuses as `conflict`.
- Knob values preserve legacy acceptance. `speculative_decoding = "off"` is valid for `llama-cpp` and emits no argv; `ngram` and `mtp` retain their legacy translations. An omitted legacy engine uses the `mlx-lm` default before applying knob rules.
- The catalog is a private regular non-symlink file. Unknown keys, wrong types, unsupported schema versions, invalid values, unsafe file shape, or an invalid entry refuse as `malformed`. Missing file or selected name is `absent`. I/O and metadata failures are `read_failed`. A missing name may invoke legacy Pi-profile compatibility resolution; malformed or unreadable catalog data never does.
- When the same name exists in both catalogs, curator-engines compares model identity and executable/argv. A disagreement refuses as `conflict`; the two definitions are never merged. A catalog entry resolves first and does not require agents-infra's Pi configuration to exist.
- Catalog status reports `engine_availability = "unknown"` until a live runtime passes the manager's readiness attestation; a successful attestation reports `available`. A missing id reports `engine_availability_reason = "served_model_id_missing"`. A shebang script used directly as the executable reports `engine_availability_reason = "script_executable_unsupported"`; the caller may use a native interpreter with its module entry point. These states are separate from broker presence: a known cold broker remains `broker.state = "absent"`. Status does not infer runtime availability from whether a model is an absolute path, whether the executable currently exists, or whether the entry uses SSH.
- A local `llama-cpp` entry's launcher removes only legacy `-a`/`--alias` options (including equals forms) and their values, then appends `--alias <served_model_id>`. A local `mlx-swift` launcher removes every legacy `--model-id` option (including equals forms) and its value, then appends `--model-id <served_model_id>`. Both preserve every other legacy argv token, including model flags, `-hf`, repeated selections, and `=` forms. A local `mlx-lm` entry has no identity flag and passes its declared id to readiness without changing argv identity options. SSH entries require the remote served id explicitly and distinct from `remote_profile`.
- Readiness passes when the declared `served_model_id` is a member of the forwarded `/v1/models` response. Additional served ids are allowed for both local and SSH engines; readiness does not infer an id from an unrelated model, executable path, or remote profile name.
- `mlx-lm` reports cached Hugging Face repository IDs and, when started with a local model path, may also report that path's absolute form. Since an absolute path cannot satisfy the v1 served-id grammar, a path-based entry is runnable only when the declared id is independently present in `/v1/models`; catalog data never shortens or guesses a path-derived id.
- `stress.startup_timeout_seconds` is also used as the manager's readiness timeout. This schema revision preserves the legacy value, but does not keep the legacy stress-only timeout use separate from normal startup.
- `pinned_distribution` is transcribed and validated as catalog data. This revision does not verify the installed package's provenance at launch; that remains a separate runtime-check question.
- The manager may emit one sanitized terminal Pi evidence summary v1 with exactly these JSON keys: `schema_version`, `session_identity_digest`, `closed_at`, `terminal_status`, and `source_path_digest`. Digests are lowercase SHA-256 hex. The source lifecycle envelope and log remain in place and read-only; raw events and their arbitrary fields are never copied. A held `active.lock`, missing close timestamp, unknown terminal status, or otherwise unclassifiable envelope refuses.
- The summary reader is a producer surface for the out-of-scope importer; this leaf does not wire an import caller.

## 4. Lifecycle

- These carry over unchanged from the residual broker: election, attested readiness, the states `serving → lingering → draining`, the restart ledger, quarantine and memory-pressure eviction (LP-D7).
- **Adopted legacy supervision mapping.** `restart_on_failure = false` sets the reported generic restart limit to 0; `true` sets it to the legacy `max_restarts`. `restart_window_seconds` maps to the supervisor's stable-run reset interval. `restart_delay_milliseconds` maps to equal initial and maximum backoff, preserving a fixed delay. A matching `fatal_output_substrings` marker retains the legacy marker-specific restart budget even when generic restarts are disabled. Exceeding the applicable limit quarantines the engine. The legacy runner simply stopped retrying; the manager labels that stopped state as quarantine. A timed quarantine reaches the supervisor's half-open attempt after its hold interval; this leaf adds no quarantine-clearing CLI command (Decision 01).

- The committed typed-refusal guard walks from the module root and parses every non-test `.go` file regardless of build tags, skipping only `testdata/`, `vendor/`, and dot-directories. It refuses any symlink encountered under the walked tree. It inventories typed refusals as direct return expressions or returning if-inits; other shapes are out of scope, see TASK-260930-1hzscs.
- The window translation is intentionally not a sliding-window equivalent: a run lasting at least the window resets the manager's failure count, while a sequence of failures each just shorter than the window can accumulate to quarantine even after the oldest legacy failure would have aged out. Catalog quarantine uses the manager's 30-second hold, then admits a half-open attempt.
- New (LP-D7):
  - `ensure` acquires a lease, holds it for the caller's deadline or hands it to the launched harness, and releases into `lingering`;
  - `release`;
  - real load and unload observations;
  - host arbitration: a maximum number of resident engines; never drain an engine with active leases; refuse rather than evict a busy engine.
- Engine kinds become a real axis in the module: `llama-cpp`, `mlx-lm` and `mlx-swift`, with ollama and LM Studio later (LP-D7).
- The module describes the lifecycle as a value, `EngineLifecyclePlan`, and never starts processes (LP-D7; LP §5 "Never: … exec in the module"). Plans stay values; only the process owner starts processes (LP §5 invariants).
- Host arbitration is to be reconciled with task-board's work on scarce-resource declarations with operation queues and on run-owned host-resource accountability (LP-D7).

## 5. Availability facts

- Engine status enters the single availability block, keyed by `(runtime, managed home)`, as two facts (LP-D9, amended 2026-09-22; LP §6 closed decision 6):
  - `ready`: the engine is `serving` or `lingering`;
  - `ensurable`: the engine entry and its weights are present, the engine is not quarantined, and host arbitration would admit an `ensure`.
- A cold but ensurable engine keeps its runtime available, at the cost of a start. Only "not ensurable" removes the runtime: if a stopped engine removed it too, the engine could never be chosen and therefore never ensured (LP-D9).
- Leases are also among the block's engine inputs: "engine status and leases (via `curator-inference-manager`)" (LP-D9), that is, `curator engines status`.
- These rules are kept: operator-declared configuration decides admission; vendor evidence only advises order; a read failure is never counted as headroom (LP-D9; LP §5).

## 6. Callers

- task-board preflight calls `curator engines ensure` for engine-bound runtimes behind the abort fence; a dry run never ensures (LP-D7).
- `curator-run` calls `curator engines ensure` before `BuildLaunch` (LP-D7).
- Routing (`curator-model-router/spec/model-routing.md`, *MR*) may weigh an engine entry's capacity as compatibility and cost. But `curator engines ensure` is the process owner's preflight action, never the router's (MR R11). `engine_ensure_failed` is a distinct failure class in routing feedback (MR R12).
- Network profiles (`curator-network-profiles/spec/network-profiles.md`, *NP*):
  - Local engine endpoints are loopback and must appear in every profile's `bypass_hosts`.
  - SSH-mode engine entries connect through the engine provider's own channel.
  - Engine networking is outside the network-profiles epic. There, `curator-inference-manager` is a fourth process owner whose processes are engines (NP N2, N12).

## 7. Local models through the general scheme

- `local-qwen` is the first catalog entry, not a special path (LP-D8):
  - project half `{pi-native, local-models, model}`;
  - catalog entry with `transport: config-home` over the seeded `models.json`, a loopback endpoint, and `engine: {mlx-lm, profile}`;
  - the engine entry lives in `curator-inference-manager`'s catalog;
  - it launches through exec-mode `pi-native`, a new `LaunchModeExec` in the module;
  - its availability comes from engine status.
- The same engine under codex is a second catalog entry, with `transport: codex-provider` (LP-D8).
- This retires the legacy `pi` wrapper plugin, `agents-infra pi spawn`, `pi-infra`/`qwen-infra`, the loopback-only `base_url`, the plugin id `mlx`, and the three-pair `[agents.targets]` whitelist (LP-D8; roadmap §3 M5).

## 8. Remote engines and KV snapshots (later)

- `mode = "ssh"` engine entries, already present in `model-harness` as profiles, cover a remote llama.cpp box. The `profile-ssh-forwarding` fact then stops being emitted as absent (LP §3).
- KV snapshots (LP-D10; LP §4 Phase 5):
  - The `kv-snapshot` facts (`save`, `restore`, `validity`) belong to the module's engine contract; their execution belongs to `curator-inference-manager`.
  - Today the only primitive is llama.cpp `/slots/{id}?action=save|restore` behind `--slot-save-path`. `mlx_lm.server` has none, and vLLM offers prefix caching only. On a hybrid model (Qwen3.8) a restored slot cannot roll back, so a restore helps only a request that extends the saved prefix exactly, at the pinned engine build. An identical prompt or a divergent tail gets no reuse.
  - Snapshot key: `(engine entry, weight digest, engine version, session id, prefix digest)`. The first four must match exactly and the fifth by prefix; anything else is reported as a cold prefill. The key further carries the KV layout (K/V cache types, stream count, V transposition), the render digest (the chat template in effect plus the effort it renders), the KV session lineage and the transcript anchor, and the weight identity (a digest of the artifacts the attested instance actually loaded, adapters included), computed and enforced by the broker.
  - Snapshots are built only after measuring cold against restored prefill (LP-D10; roadmap §5).

## 9. Delivery

| Step | Where | Done when | Source |
| --- | --- | --- | --- |
| Decision 0020: inference plane (fifth plane, closed CLI/JSON contract, Curator engine-blind, security review because the provider runs processes) | curator-spec | adopted | LP-D7, LP-D13, roadmap §3 M5 |
| `curator-inference-manager`, binary `curator-engines`: engine entries with `add`/`confirm`, broker from the agents-infra residual, `ensure/release/status/stop`, `ready`/`ensurable`, host arbitration; module engine kinds; exec-mode `pi-native`; preflight `ensure` | curator-inference-manager, skill-agents-management, skill-project-management | local models run through the general scheme | roadmap §3 M5; LP §4 Phase 4 |
| Retire the legacy `pi` plugin, `agents-infra pi spawn`, `pi-infra`, `qwen-infra` | skill-agents-management, relux-agents-infra | nothing calls them | roadmap §3 M5 |
| KV snapshots | curator-inference-manager, skill-agents-management | built after the measurement | LP §4 Phase 5; LP-D10 |

- Milestone M5 follows M4, in which runtime bindings reach the launch (roadmap §2).
- After LP Phase 4, agents-infra holds only Codex/Claude configuration state and attachments (LP §4 Phase 4).
- Work on this repository is tracked on the curator board, with the cross-repository board-state commit path (LP §5).

## 10. Open points

1. Decision 0020 is not written. The closed CLI/JSON contract of `curator engines status|ensure|release|stop`, and of the catalog verbs, is defined there, including field names, exit codes and refusal codes (LP-D7, LP-D13; CLI §3.3).
2. Host arbitration still has to be reconciled with task-board's work on scarce-resource declarations with operation queues and on run-owned host-resource accountability (LP-D7).
3. ollama and LM Studio come later as engine kinds (LP-D7).
4. KV snapshots wait for the cold-versus-restored measurement (LP-D10).
5. Repository visibility: the roadmap proposes public (roadmap §6). This repository stays private until the operator decides.

## 11. Change log

**2026-10-02.** Erratum 1: on a hybrid model (Qwen3.8) a restored slot cannot roll back, so §8 states that a restore helps only a request extending the saved prefix exactly, at the pinned engine build.

**2026-10-02.** Erratum 2: the §8 snapshot key gains the KV layout, the render digest, the KV session lineage and transcript anchor, and the broker-computed weight identity.

**2026-10-01.** Erratum 8: the first member of the KV snapshot key in §8 is the engine entry, replacing the legacy profile.

**2026-09-29.** Adopted the curator-engines v0.x engine catalog schema v1, strict reader/resolution outcomes, legacy Pi compatibility rule, supervision mapping, and sanitized terminal evidence summary. Decision 0020 remains pending in curator-spec (Decision 01).

**2026-09-27.** Applied the command-line shape the operator adopted on 2026-09-27 (CLI):

- the binary is `curator-engines`, dispatched as `curator engines …`; the `curator inference` dispatcher is dropped (§1, §2);
- the catalog verbs `list`, `show`, `add`, `remove`, `check`, `bench` and `confirm` join the lifecycle verbs `status`, `ensure`, `release` and `stop` (§2);
- adding an engine is a widening entry confirmed with `curator engines confirm` (§2);
- first-party installation beside `curator`, or `curator provider install engines` (§2);
- "engine profile" becomes "engine entry", `--profile NAME` becomes `--engine NAME`, and a runtime binding's `engine.profile` becomes `engine.name` (§2, §3, §5–§8);
- `runtimes.toml` is `~/.curator/runtimes.toml` (§3).
