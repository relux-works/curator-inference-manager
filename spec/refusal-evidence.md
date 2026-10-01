# Typed refusal evidence contract

This test/tooling contract does not change manager behavior. A source walk
includes every regular non-test `.go` file regardless of build tags, skipping
`testdata/`, `vendor/` and dot directories. Symlinks and non-regular entries
refuse. Darwin, Linux and Windows package contexts resolve source expressions
through `go/types`. Standalone `package main` commands with exactly
`//go:build ignore` get a separate explicit-file `go list -deps -export`
context on each platform. They remain in the census; other uncovered build
contexts refuse.
Reflection and generics-instantiated constructors are out of contract, as
specified by TASK-260930-1hzscs dev brief 02.

A refusal factory has a direct returned repository-defined error literal (or
another such factory) in its single-return body, with exactly one error result. Factory references use
resolved function objects, including methods. Typed error literal producers
are recognized by their resolved named type's implementation of `error`.
Typed value references are inventoried as well as factories/literals. The
closed observation exception permits field/method selection, comparison to
nil, and the address of the target in standard `errors.As`. These inspect an
already received error instead of propagating/storing its value. All other
typed value references are classified by P1–P4. No data-flow tracing is performed.

The accepted propagation subset is:

- P1: a producer returned directly as an error, including a returned refusal
  composite, or an if-init with exactly one returned consumer in its body.
- P2: a channel send whose element implements `error`. `chan any` refuses.
- P3: assignment to the enclosing function's own named error result, including
  its direct deferred closure. A different function's result refuses.
- P4: an argument to a module function/method whose every error return is
  the incoming error parameter, resolved by object identity, or standard
  `errors.Join` including it. The call's error result must propagate under
  P1–P3. Transitive wrappers, function values, interface calls and `fmt.Errorf`
  are excluded. Any extra return of another error disqualifies a wrapper.

Local storage followed by return, field/global/map/slice storage, non-error
channel sends, and argument passing outside P4 refuse with `file:line`.
Arguments to recognized refusal factories are permitted. Declaration-level
producers are also checked.

Each site uses `file:function:ordinal`, with receiver-qualified method identity.
The ordinal counts producers in AST traversal order. Explicit test references
are read from `scripts/engine-catalog-sites.json`; missing/duplicate mappings or
absent named test declarations or changed canonical AST body hashes refuse.
References bind the exact test declaration and its `body_sha256`; deleting a
negative subtest or assertion changes that body hash and turns the guard red. Names overlapping a production function
are never evidence in the command's inventory path. Legacy test-selection
helpers remain solely for the original helper regressions.

Certificates declare `witnesses`: a selected rejected condition member and
an `admit` expression. **R4 is frozen as singleton equality witness** under
rework brief 08. For comparable values the exact shape is
`<guarded expression> == <constant literal>`: one equality, with a `go/types`
constant assignable to the guarded basic type (including named basic types).
The guarded expression must have identical formatted AST syntax and the same
base and every field `types.Object`. Only direct variable/selector expressions
qualify. Constants, sibling fields and unrelated inputs cannot lend identity.

Every other predicate shape refuses: `len`, indexing, slicing, method calls,
ranges, inequalities and conjunctions. Observation arguments are opaque when
selecting the guarded operand, so `len(value)` cannot certify `value` either.
`nil` is not a Go constant. Interface/pointer equality and length/error-text
witnesses from earlier revisions are now explicit undecided bounds, not kills.
This supersedes rev-2's request to retain length-domain predicates.

For non-comparable types the accepted deep-equality shape is a call resolved to
standard `reflect.DeepEqual` with the identical guarded expression and a fully
specified composite literal of its exact type. Structs specify every field;
arrays specify every slot; slices and maps specify all their literal members.
Members are constants or recursively complete composites. Partial structs,
keyed arrays/slices, pointers and implicit interface values refuse. A module
alias or lookalike DeepEqual cannot replace the standard function. The source
must already resolve that function; no import or production-source change is
inferred from a certificate.

A narrowing mutation retains the original gate and adds only that exception.
For an OR the generated exception also requires the other rejected groups to
be false; for an AND the original gate requires every conjunct. Unconditional
refusals follow the separate rule below. No witness means no generated evidence.
The compiler and behavioral suite must validate a generated candidate; static
shape acceptance alone does not prove that a witness exercises a real site.
Further conceivable witness weaknesses go to future hardening; review blocks
R4 only for a violation of this frozen rule in this repository or a regression.

Before mutation execution, the generator binds the primary and execution roots
by canonical AST hashes of all production-package Go-file sets and mapped test directories, including all test
files, test bodies, helper declarations and production sources, plus raw
`go.mod`/`go.sum` hashes. Only emulator build-directive and platform-filename
rewrites are normalized. No test body or helper is excluded: the generated
production overlay is the only rewritten input. Missing, extra, changed or
non-regular Go entries refuse kill evidence. The selected tests must pass an
unmutated baseline; baseline reuse requires the same content identity, root and
selected test patterns. A failure counts only if its name belongs to the
primary mapped test set. A pre-existing named failure is invalid, never killed.

A zero-exit behavioral run is a survivor. A downstream equivalence claim is
admitted only when both individual mutations survive and a joint mutation
fails a named mapped test. The claim is bounded to those executed witnesses;
it does not prove universal program equivalence. Survivors, compiler failures,
missing tests, absent joint evidence and unnamed failures block completeness.

## Unconditional refusals

A refusal without a guarding condition is classified as **unconditional:
AC1 + removal mutant + consumer narrowing** (dev brief 04). The currently implemented structural subset is a function with a single
direct return; conditional switch/select arms and returns following other
statements cannot borrow this classification. It retains its
site-identity certificate and a removal mutant evaluating the original value
and then returning nil, killed by a
named test. Narrowing belongs to conditional consumer sites, including platform
dispatch selecting an unsupported implementation. No constant wrappers or
invented input domains stand in for a rejected guarding-condition class.

## Repository certificates and review bounds

The repository census contains **482 sites**. The older 484-site note was a
reporting error: a fresh `typedPropagationSites` walk matches every identity
in the previous 482-site probe, with no added or removed identity. The source
walk remains independent of build tags. Each declared bound binds its exact
production file SHA256 and reason; a changed source, missing certificate,
stale identity or unknown bound kind refuses. Inventory JSON includes the
bounds next to their sites. A site with no proved mapping/witness remains
**bound: undecided**, never silently disappears or claims a mutation kill.
These bounds are review decisions under dev brief 05, not out-of-contract
inputs or completeness proofs. Site coverage is reported separately from
coverage of the five acceptance rows.

## Defensive-unreachable bound

Under dev brief 05 a surviving narrowing may be classified
**bound: defensive-unreachable** only when a named test actually runs green
and pins the dependency behavior preventing that member, and a joint
primary-plus-downstream mutation fails a mapped named test. The generator
executes both commands and records their actual statuses and logs. A
package pass with no named pin execution is refused. This classification is
separate from EQUIVALENT and does not borrow a downstream failure as proof
that the primary was exercised.

`internal/infra/engine_catalog.go:validateEngineCatalogExactKeys:2` checks
`rawDocument == nil` after successful `toml.Unmarshal` into `map[string]any`.
Pinned go-toml v2.4.3 initializes the generic map on every success. The named
`TestEngineCatalogDecoderInitializesMapAndRejectsMissingEngines` pins this
behavior through the actual dependency and validator. The primary nil-map
admission survives, while its joint mutation with the subsequent engines
assertion is killed by that test. The generator requires the executed proof;
a certificate alone cannot claim defensive-unreachable success. Under the
frozen brief-08 shape rule this former nil-map witness is now an undecided
bound: nil is not a Go constant or a fully specified composite. The dependency
pin and joint kill remain historical evidence; no current defensive proof is
claimed for an ungenerated witness.

For other survivors the original gate and exact admitted member remain in
the table. An explicitly source-bound undecided declaration records the
reason and evidence gap for review; an undeclared survivor still fails
`mutationEvidenceComplete`. No string-matched EQUIVALENT allowlist exists.

## Converge onto ef23c20

The four new files (`engine_check.go`, `engine_gguf.go`, `check.go`, and
`check-k0-evidence.go`) are walked and type-checked. The typed-error census
remains 482: `EngineCheckFailure` is a report record without an `Error`
method, and GGUF parser errors use standard `fmt.Errorf`, not a repository
refusal type. This is a type distinction, not an omission or an error
implementation inferred from its name. The separate K0 census/mutant table
covers those report refusals via CLI `run` → `CheckEngineFile` →
`CheckEngineEntry` / `parseEngineCheckArgs`; remaining report sites are
explicit in the converge Bounds table. Standalone type-context omission is
covered by `TestRefusalInventoryIncludesStandaloneIgnoreCommands`, including
an allowed direct refusal and a refused local-storage near miss.

## Review revision 1 rework

`TestRefusalWitnessCannotBindSiblingField` drives the public inventory command
with the local-mode/unknown-engine sibling-field case and tests nested selector
paths. `TestRefusalNarrowingPreservesOtherRejectedEngines` executes a singleton
engine mutation and separately proves that another unknown engine is still
refused. `TestReviewNarrowingMustKeepOtherRejectedEngines` retains the reviewer's
actual catalog regression under its original name.

`TestRefusalEmulationRejectsChangedMappedTestBody` drives the public mutant
command with an exact Linux projection, then inserts the reviewer's unrelated
pre-existing failure in the mapped test. That body mismatch refuses before
baseline/mutation execution. `TestRefusalMutationRejectsAlreadyFailingBoundBaseline`
rejects a failing baseline even when both roots have matching contents.
`TestReviewCommandJointProofCannotCloseUnprovedSurvivor` retains the reviewer's
public-command equivalence regression under its original name.

## Review revision 2 rework

`TestRefusalWitnessCannotCoarsenGuardedValue` drives the public inventory CLI
with the review's length/prefix cases for the catalog Engine guard. Its valid
control executes the singleton mutant, requires a named kill, and separately
checks that `foobar` still refuses. The selector/sibling and changed-test-root
regressions and the original reviewer tests retain their names.
`TestRefusalSingletonEqualityRequiresTypedConstantAndExactSyntax` attacks
constant types, observations, conjunctions and original length-domain borrowing.
`TestRefusalSingletonDeepEqualityRequiresFullySpecifiedLiteral` attacks partial
composites, keyed slices and mismatched aggregate types.

The strict inventory retains all 482 sites and named mappings. 197 formerly
accepted witnesses are individually source-bound as undecided under brief 08;
their previous mutant keys, results and exact rejected shapes are preserved in
the handoff ledger. Current executions cover only the frozen accepted subset.
A historical kill is never silently promoted to current singleton evidence.

## Legacy importer convergence onto 1ecc0aa

The legacy importer extends the same typed census and explicit certificate file;
it does not add a second inventory or witness grammar. `-files` selects execution
rows only after the full module census, certificate validation and mutant
derivation succeed. `-task` changes the safe outcome filename prefix. The
singleton shape/type/object checks, content-bound execution roots, baseline
admission, joint-equivalence and defensive-bound rules remain unchanged.

Importer certificates bind current production digests and named negative test
bodies. Previously accepted length/error-text/method/pointer witnesses are
recorded as source-bound undecided rows under the frozen grammar. A recorded
historical kill is not singleton evidence. Unconditional factory removal
remains a separate classification. Go timeouts, including a timeout after an
earlier assertion failure, are invalid kill evidence.

`TestLegacyRuntimeStateMutantsRetainPredicateAndNarrowOneClass` runs the module
census and certified generator, pins the original gate and mapped test-body
hashes, and requires both ledger-schema and half-open singleton witnesses.
`TestImportLegacyEngineStateLifecycleSchemaSingleton` drives all four public
import actions with the reader's schema witness and checks exact preservation
of source, manager config and cache state. Undecided rows and every executed
survivor are reported individually in the task's current mutation table.

On the converged importer candidate the module census is **629 sites**: the
upstream 482 identities remain, and 147 importer identities are added. All 147
new identities have named, body-bound negative tests. The whole module has 519
named certificates and 110 upstream sites with explicit undecided evidence gaps;
186 mutants are currently generated. These census counts account for sites;
they do not claim that every site or operand has a killed singleton witness.

Non-Darwin file-set projections retain the unprojected candidate privately under
`.temp/canonical-source`. The full-module importer census test binds that snapshot
to the executing tree using the existing content identity check before resolving
all three original platform contexts. Projected build tags cannot supply those
contexts themselves. `TestLegacyMutationCensusRejectsChangedCanonicalSnapshot`
refuses a changed test body or incomplete snapshot; no context or guard is skipped.
