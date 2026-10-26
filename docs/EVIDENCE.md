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

## Offline raw input inventory

The [private raw input inventory](../tools/doc-check/RAW-EVIDENCE.md) adds an
offline capture/verify path for existing phase and user-run inputs. It requires
explicit file/reference closure and an independently held inventory pin. It
records original source/build/run declarations as unverified or unknown and
calculates no performance aggregate. File-byte capture does not qualify the
original run or change the legacy renderer.

The selected [inventory module](../tools/doc-check/raw-inventory.mjs),
[CLI](../tools/doc-check/raw-cli.mjs), [JSON validator](../tools/doc-check/raw-json.mjs)
and [owned fixture source](../tools/doc-check/raw-inventory.test.mjs) are retained
at Fleet `5bc02b3ed23d6471a777dc493c9422e8cd08d6f9` and checked against that Git
revision by the manifest. This is a selected source correspondence claim. Actual
local/hosted test execution and original benchmark acceptance are separate.

## Planned retention work

G02 still needs real historical raw-artifact recovery and independent
source/run/platform/command/completeness and benchmark-method acceptance. The
generic checker's typed benchmark fixtures and this tool's owned synthetic
fixtures do not fill that gap. Preserve incomplete/failed evidence and original
samples; no raw samples may be reconstructed from summary tables.

## Qualification limits

The default hosted documentation suite executes the portable checker's complete
filesystem profile. Its local restricted profile skips two native symlink cases;
those skips remain visible. Existing Go CI runs whenever runtime or build inputs
change; future documentation-only changes avoid that unrelated application build.
Source inspection, command execution, hosted checks, benchmark method acceptance,
operator recovery and production readiness are separate. Manual English/privacy
and semantic/provenance review remain required. No raw operator logs, private
paths, identities, accounts or credentials belong in public reports.

## Source revision aliases

Implementation source selectors use the current contribution history after
author metadata normalization. Each pair below has an identical complete Git
tree and identical selected source blobs. Original test results and benchmark
provenance remain bound to their recorded historical revisions. This source
correspondence does not establish a fresh runtime test, accepted governance,
deployment or current performance.

| Implementation claim | Historical revision | Current source alias |
| --- | --- | --- |
| `raw-input-inventory-implementation` | `5bc02b3ed23d6471a777dc493c9422e8cd08d6f9` | `c4e2a531626ced32c3759cfc3ab2fa1367cfed8b` |
