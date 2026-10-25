# Source and benchmark evidence

## Current source references

This review pins Fleet `d00b61aa734b4cb8e7103114772c60c1ebcaacb5`. The selected
[phase writer](../fleet/fleet.go), [phase report](../fleet/report.go),
[user report](../fleet/users_report.go) and [command](../cmd/zvm-fleet/run.go)
are byte-pinned in the [manifest](../tools/doc-check/manifest.json), and checked
against that Git revision. The selected Core vectors are supplied separately
from Core `29e6d9b30be94de3772c850c7a1e9811585a8833`. Its full source context and every declared submodule
remain separate from this documentation qualification; no submodule was fetched
for the local Markdown checks.

The [portable checker provenance](../tools/doc-check/UPSTREAM.json) pins the
already qualified Core implementation. A dedicated documentation job checks all
tracked Markdown, local links/anchors and selected references. A documentation
pass executes no fleet phase, devnet, wallet, payout, RPC or benchmark.

## Historical results

[FLEET.md](../FLEET.md#evidence-status) is a retained historical summary. Its
original numeric content is preserved. No raw `.json`, `.jsonl` or `.ndjson`
benchmark artifact exists in the selected top-level source inventory. This is
not a claim about operator storage, submodule contents, all repository history
or hosted artifacts; those sources have not been exhaustively searched.

The inspected phase format includes before/after snapshots, samples, run file
references and errors. The legacy renderer can retain a saved summary when a
referenced raw run is unreadable, invalid or empty; it can also render a run
without a retained names profile. A rendered table or a successful re-render
therefore does not establish complete raw-run provenance. This source review
does not execute or qualify the renderer, change its runtime behavior, or
reconstruct any original samples.

Before treating a result as reproducible, retain the complete phase and referenced
run files, names/profile, source/dependency/build/runtime/platform and exact
chain/config/genesis context; retain the actual invocation, attempt, exit and
completeness. Hash the retained raw inputs without exposing local paths or
identity. Missing profiles, partial/error phases, absent run records and unknown
completion remain explicit. Preserve the distinction between apparent and
allocated storage and between a saved summary and recalculation from raw records.

## Planned retention work

G02 still needs source-bound raw-manifest retention and independently qualified
reproduction or explicit missing-evidence handling for real Fleet outputs. The
generic checker can validate complete typed benchmark fixtures; those synthetic
fixtures are not Fleet performance runs and do not fill the historical gap.
Do not generate raw samples from summary tables or silently label incomplete
input as a successful benchmark. Any future reporter/writer change needs its own
source contract, bounded offline tests and dependency/integration qualification.

## Qualification limits

The default hosted documentation suite executes the portable checker's complete
filesystem profile. Its local restricted profile skips two native symlink cases;
those skips remain visible. Existing Go CI runs whenever runtime or build inputs
change; future documentation-only changes avoid that unrelated application build.
Source inspection, command execution, hosted checks, benchmark method acceptance,
operator recovery and production readiness are separate. Manual English/privacy
and semantic/provenance review remain required. No raw operator logs, private
paths, identities, accounts or credentials belong in public reports.
