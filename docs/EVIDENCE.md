# Source and benchmark evidence

## Current source references

This integration pins runtime source Fleet `1c2c499d08e8ee7e7830d2561cf5cae356ede5c5` and combines it
with documentation source `b8725bc0653de8a3465c7a2805a055924cd70c37`.
The [manifest](../tools/doc-check/manifest.json) checks the selected
[phase writer](../fleet/fleet.go), [phase report](../fleet/report.go),
[user report](../fleet/users_report.go), [command](../cmd/zvm-fleet/run.go)
and the changed runtime pipeline modules against that immutable runtime source.
It retains exact bytes and SHA-256 for input parsing, arithmetic, context/time,
sampling, resource/readiness, JSONL, user aggregates/timing and report publication.
This is selected source correspondence, not source-authority authentication.

Core vectors remain a separate pinned input from Core
`29e6d9b30be94de3772c850c7a1e9811585a8833`. Hash equality does not execute or
authenticate the generator, qualify its full source context, or qualify every
declared submodule. No local submodule acquisition is needed for these claims.

The [portable checker provenance](../tools/doc-check/UPSTREAM.json) remains
unchanged. Its dedicated job checks all tracked Markdown, local links/anchors,
selected references and evidence records. Documentation tests and runtime Go
workflows have separate outcomes. A documentation pass executes no Fleet phase,
devnet, wallet, payout, project RPC or benchmark.

## Historical results

[FLEET.md](../FLEET.md#evidence-status) remains a historical summary with its
original numbers preserved. Its raw phase/run files, original platform and exact
source/run/configuration context remain unavailable in this source snapshot.
No original samples are reconstructed from those numbers. This is not an
exhaustive claim about operator storage, submodules, history or hosted artifacts.

At historical source `d00b61aa734b4cb8e7103114772c60c1ebcaacb5`, the legacy
renderer could reuse a saved summary when referenced raw input was unavailable
and render without a retained names profile. That description is historical;
it is not the current renderer contract. The current source adds confined raw
references, strict replay, exact operands and availability, observed readiness,
resource coverage, checked streams, user run/context/time checks, owned output
boundaries and checked report-pair publication. See the [guide](GUIDE.md) for
the selected behavior and its limits. These changes do not recover old inputs
or establish the historical table's reproducibility.

Before accepting a benchmark, retain phase/raw files, names/profile,
source/dependency/build/runtime/platform, chain/config/genesis, invocation,
attempt, actual exit and completeness. Retain byte hashes without exposing
private paths or identity. Missing, partial, failed and unknown observations
remain explicit. Apparent size, physical allocation, retained-row ratios and
causal cost are separate. Neither a successful render nor a passing fixture
suite proves complete runs, synchronized clocks, durable bytes or finality.

## Offline raw input inventory

The [private raw input inventory](../tools/doc-check/RAW-EVIDENCE.md) remains
an offline capture/verify tool with explicit reference closure and a separately
held inventory pin. Source/build/run declarations stay unverified or unknown;
the tool calculates no performance aggregate and authenticates no original run.
Its own code does not execute or modify a renderer.

Its [inventory module](../tools/doc-check/raw-inventory.mjs),
[CLI](../tools/doc-check/raw-cli.mjs), [JSON validator](../tools/doc-check/raw-json.mjs)
and [fixture source](../tools/doc-check/raw-inventory.test.mjs) remain pinned to
`5bc02b3ed23d6471a777dc493c9422e8cd08d6f9`. Their implementation claim and the
historical benchmark-unavailable record remain unchanged. Test execution and
original benchmark acceptance are independent of byte correspondence.

## Planned retention work

G02 still needs real historical raw-artifact recovery and independent
source/run/platform/command/completeness and benchmark-method acceptance.
Synthetic typed benchmark fixtures and raw inventory fixtures do not fill that
gap. Do not infer original samples from summaries. Broader runtime coverage,
checked-pair consumer adoption and actual operator acceptance remain open.

## Qualification limits

The default hosted documentation profile executes actual filesystem symlink
fixtures; restricted local profiles and their skips remain separate evidence.
The combined runtime workflow preserves build, vet, normal tests, counter fuzz,
Fleet/internal race and CLI package race. A documentation-only descendant can
reuse exact runtime input/workflow correspondence without claiming a new Go
execution; actual workflow/path-filter observations must remain explicit.

Documentation checks validate declared records and selected bytes, not every
natural-language statement or the authority of a hosted result. Manual English,
privacy, semantic and provenance review remain necessary. Keep raw operator
logs, private paths, real identity, accounts and credentials out of public reports.
Windows capture, actual services/chains, controlled performance, crash recovery,
complete source freshness and production acceptance remain unqualified.
