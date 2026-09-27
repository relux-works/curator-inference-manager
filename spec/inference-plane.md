# curator-inference-manager: inference plane

Status: **DRAFT**, 2026-09-27. Not normative. This document collects what earlier drafts and decisions already say about the inference plane and adds no design of its own. The sources are the launch-profiles draft (`skill-project-management/.specs/drafts/launch-profiles.md`, cited as *LP*), the ecosystem roadmap (`wiki/roadmap/ecosystem-roadmap.md`, cited as *roadmap*), and the command-line shape the operator adopted on 2026-09-27 (`wiki/research/cli-shaping-review.md`, cited as *CLI*), which renames the commands and the engine entries of LP. §11 is the change log. Every statement cites its source; points the sources leave open are listed in §10. The decision record for this plane will be **Decision 0020** in curator-spec, which is not written yet (LP-D7, LP-D13, roadmap §3 M5).

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
- An engine entry carries the fields of today's `model-harness` `[profiles.<name>]`: `engine`, `model`, `executable`, `argv`, `knobs`, `mode local|ssh`, `stress`, `supervision`, `pinned_distribution` (LP-D7).
- A runtime binding references an engine entry by name. For example, the provisional `runtimes.toml` sketch has `engine = { plugin = "llama-cpp", name = "<name>" }`, where LP wrote `profile = "<name>"` (LP-D2, LP-D7; CLI §3.1 rule 3).
- The pi client half of today's profiles (`thinking`, `compat`, `compaction`) does not move here. It moves to the managed pi home and the `pi-native` plugin (LP-D7).
- Engine entries live in a 0600 machine file. Board text and digests carry runtime ids only, and run manifests carry nothing private: no paths, sockets, endpoints with credentials or PIDs (LP-D11).

## 4. Lifecycle

- These carry over unchanged from the residual broker: election, attested readiness, the states `serving → lingering → draining`, the restart ledger, quarantine and memory-pressure eviction (LP-D7).
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
  - Today the only primitive is llama.cpp `/slots/{id}?action=save|restore` behind `--slot-save-path`. `mlx_lm.server` has none, and vLLM offers prefix caching only.
  - Snapshot key: `(profile, weight digest, engine version, session id, prefix digest)`. The first four must match exactly and the fifth by prefix; anything else is reported as a cold prefill.
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

**2026-09-27.** Applied the command-line shape the operator adopted on 2026-09-27 (CLI):

- the binary is `curator-engines`, dispatched as `curator engines …`; the `curator inference` dispatcher is dropped (§1, §2);
- the catalog verbs `list`, `show`, `add`, `remove`, `check`, `bench` and `confirm` join the lifecycle verbs `status`, `ensure`, `release` and `stop` (§2);
- adding an engine is a widening entry confirmed with `curator engines confirm` (§2);
- first-party installation beside `curator`, or `curator provider install engines` (§2);
- "engine profile" becomes "engine entry", `--profile NAME` becomes `--engine NAME`, and a runtime binding's `engine.profile` becomes `engine.name` (§2, §3, §5–§8);
- `runtimes.toml` is `~/.curator/runtimes.toml` (§3).
