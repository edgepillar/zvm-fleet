# Private raw input inventory

`raw-cli.mjs` captures and verifies existing Fleet input files offline. It uses
Node.js built-ins and the strict JSON validator retained from the pinned Core
checker. The six portable files listed in [UPSTREAM.json](UPSTREAM.json) remain
unchanged. The [source evidence page](../../docs/EVIDENCE.md) separates this
byte-retention tool from benchmark and production qualification.

## Scope

The supported layout is Fleet `d00b61aa734b4cb8e7103114772c60c1ebcaacb5` with Node
`ae90b945b0ea95887c31cfb2f19e0b88504186d8`. These pins describe JSON layout, not
the source/build that produced an arbitrary supplied run. Original source,
dependency/build/runtime/platform, command, run/attempt, exit and completeness
declarations remain unverified or unknown. Hashing an input cannot authenticate
those declarations. The tool computes no throughput, latency, cost or storage
aggregate and launches no program, renderer, network request or benchmark.

A successful capture binds a stable selected set of readable file bytes. A
recorded error, missing snapshot, ended timestamp or readable JSON does not
establish a successful original measurement. An ended Node result can precede
its inclusion drain. Raw transaction-result files remain opaque streamed bytes;
their schema, populations and saved summaries are not validated or recomputed.
A nonempty file with an empty transaction population remains unqualified input.
An empty or missing referenced file is refused.

The inventory is private. It contains relative producer filenames and optional
declared run labels that may identify their source. It is not an anonymization
format. Raw files can contain accounts, transaction identifiers, endpoints and
operator diagnostics. Keep the inventory, its independent pin and all referenced
inputs under private owner-controlled retention. This command does not copy the
raw files into protected storage or prevent their later deletion.

## Capture

Use an existing stable private input snapshot and output directory:

```sh
node tools/doc-check/raw-cli.mjs capture \
  --root "$RUN_DATA" --mode phases \
  --output "$PRIVATE_OUTPUT/raw-inventory.json"
```

`--root`, `--mode` and `--output` are explicit. The mode is `phases` or `users`.
The output is created exclusively with owner-only permissions and synced with
its parent directory. Existing output is never replaced. A failed new write
leaves its actual partial output for review; it is not deleted. Standard output
contains a small operation summary, total input bytes/file count and the
inventory SHA-256, without the root, raw records, selectors or context values.
Record that pin when accepting the capture, separately from later producer data.

`phases` selects root `NN-name.json` phase files, requires `names.json`, follows
each canonical `runs/` reference, and includes `samples.jsonl` when present.
It refuses user-run metadata in that mode. `users` requires exactly one
`NN-users-label.json` phase, `names.json`, `users.json`, `records.jsonl` and
`samples.jsonl`; the label must match. Stream records must be complete LF-ended
JSON objects. They are syntax/structure checked without deriving transaction or
performance semantics. The mode cannot join multiple user phases to one possibly
overwritten record stream.

Selectors must use the supported ASCII producer names and exact case. References
are bound to phase/scenario/lane/wallet-count filenames. Duplicate selectors,
unknown phase fields, duplicate JSON members, missing references and orphan
files under `runs/` are refused. Unrelated root data, state, credentials and other
directories are not read or adopted. Reused output directories may need a
separately prepared private snapshot; the tool never removes orphan files or
renames input records to force a match.

The guarded filesystem profile requires regular files on the selected filesystem
and the current owner, with no symlink ancestors or hard links. File identities
are checked before opening, after streamed reads and at final reconciliation.
Selected directory identities and sets must remain stable. This detects ordinary
replacement/mutation; it is not an OS sandbox or an atomic snapshot against a
hostile concurrent writer. Local macOS and hosted Linux execution must be
qualified separately; required guard flags must be available on other platforms.

## Context declarations

Without `--context FILE`, source/build and run fields are explicitly unknown.
`--context` accepts a private JSON object with exactly this structure:

```json
{
  "authority": "unverified-declaration",
  "source": {"fleetRevision": null, "nodeRevision": null, "coreRevision": null},
  "build": {"artifactSha256": null, "dependenciesSha256": null, "runtime": null, "os": null, "architecture": null},
  "run": {"id": null, "commandSha256": null, "attempt": null, "exitCode": null, "completion": "unknown", "outcome": "unknown"}
}
```

Full source revisions and digests may replace nulls. Runtime/run identifiers use
bounded ASCII tokens; platform names use the supported OS/architecture values.
Commands are retained as a digest, not raw arguments. Completion is `unknown`,
`declared-complete` or `declared-incomplete`; outcome is `unknown`,
`declared-success` or `declared-failure`. A declared success requires declared
completion and exit zero, and still establishes no original outcome authority.
The capture records phase-error and missing-snapshot observations separately.

## Verify an independently pinned inventory

```sh
node tools/doc-check/raw-cli.mjs verify \
  --root "$RUN_DATA" --manifest "$PRIVATE_OUTPUT/raw-inventory.json" \
  --manifest-sha256 "$ACCEPTED_MANIFEST_SHA256"
```

The expected SHA-256 must come from the independently retained accepted capture.
Computing a new pin from a later producer's changed inventory does not recover
that trust boundary. Verification checks the pinned inventory bytes, strict
schema, full current selected set, references, digests and structural observations.
An inventory cannot label itself performance-qualified or raise consumer limits.
Only selected file-byte correspondence is returned; source/build authenticity,
original process completeness/outcome and performance remain unestablished.

## Bounds and tests

Defaults are 256 files, 8 MiB per metadata JSON file, 256 MiB per raw/stream file,
512 MiB total, 1 MiB per JSONL record and one million JSONL records per stream.
Use the explicit `--max-files`, `--max-phase-bytes`, `--max-raw-bytes`,
`--max-total-bytes`, `--max-jsonl-line-bytes` and `--max-jsonl-lines` flags to
choose a budget. Hard ceilings are 4,096 files, 16 MiB metadata, 4 GiB per raw
file, 8 GiB total, 1 MiB per line and ten million lines. Directory enumeration,
references, JSON depth and inventory output also have bounds. Consumers select
their own limits independently. Raw hashing uses a fixed 64 KiB buffer; no second
whole-record JSON encoding or raw-file copy is created.

Run the separate owned fixture suite:

```sh
node --test --test-reporter=tap --test-concurrency=1 tools/doc-check/raw-inventory.test.mjs
```

The restricted `ZVM_DOC_CHECK_STORAGE_PROFILE=regular-files-only` profile skips
three real filesystem alias cases, which the full hosted profile executes.
Simulated device/owner mismatch cases are labelled as simulations. Fixtures
contain synthetic input bytes and establish no product performance result.
Keep test execution, record retention, historical recovery and independent
benchmark method/source/run acceptance separate.
