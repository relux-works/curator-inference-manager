# Unresolved Questions

## Curator-spec adoption

Decision 01 defines the manager-owned engine catalog v1 and supervision mapping. Curator-spec adoption remains pending in [issue #114](https://github.com/relux-works/curator-spec/issues/114) (Decision 0020). This external documentation decision does not block the curator-inference-manager implementation; the spec marks the contract as adopted by curator-engines v0.x pending that decision.


- TASK-260930-1hzscs: dev briefs 04–05 resolved unconditional and
  decoder-unreachable classification. Explicit source-bound undecided sites
  remain subject to review; the task outcome lists each evidence gap rather
  than waiving it. See [evidence contract](spec/refusal-evidence.md).

The brief-08 frozen singleton rule reclassifies 197 historical witnesses as explicit source-bound undecided gaps (including the former nil-map defensive witness). Their old executions remain historical; current evidence and every retired key are in the task rev8 results/Bounds ledger. Reviewer owns these bounds under dev brief 05. No production/API decision is pending.

## Configured runtime code identity

Broker/client authentication now uses kernel cdhash. Configured runtime FILE
expectations still use the existing path identity, explicitly documented in
[the protocol](spec/engines-protocol-v1.1.md#darwin-broker-code-identity).
A follow-up must define unsigned/interpreted runtime compatibility and exact
Mach-O CPU subtype selection, then cover launch, authorization, attestation
and reclaim together. The embedded-signature reader alone does not settle
that policy or close the runtime FILE race.

## Broker hello challenge

Open follow-up: add a broker-generated nonce challenge to hello in a protocol
revision. LOCAL_PEERTOKEN describes the current incarnation of the socket's
last PID; messages queued before authentication followed by exec into trusted
code are attributed to the trusted executable. The current policy accepts
this because the same-UID attacker can already run the genuine client with
arbitrary arguments. Post-authentication PID/pidversion rechecks do not close
this pre-authentication attribution gap. Specify challenge freshness, request
binding, replay rejection and compatibility before changing the wire schema.
See [the identity contract](spec/engines-protocol-v1.1.md#darwin-broker-code-identity).
