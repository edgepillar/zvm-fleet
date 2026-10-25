# Source-bound documentation checks

This is an offline Node.js 22+ tool. It parses Markdown with
[Marked](https://marked.js.org/), HTML with
[parse5](https://github.com/inikulin/parse5) and heading slugs with
[github-slugger](https://github.com/Flet/github-slugger). Exact package versions
and registry integrity hashes are in [package-lock.json](package-lock.json).
HTML is parsed as data; no page, script, vector generator or benchmark executes.

## Run

From the repository root:

```sh
npm ci --prefix tools/doc-check --ignore-scripts --no-audit --no-fund
npm test --prefix tools/doc-check
node tools/doc-check/cli.mjs --input published-vectors=vectors.json
```

Supply the selected Core `vectors.json` separately from the immutable source and
digest in [manifest.json](manifest.json). The workflow performs this bounded
acquisition first. The checker makes no network calls and never probes remote
link availability. Content equality does not independently authenticate source
authority, establish semantic conformance or run the vector generator.

Use `--root DIRECTORY` for another stable Git checkout and `--manifest RELATIVE`
for its manifest. Each declared external input needs one `--input ID=FILE`.
Duplicate or undeclared inputs fail. The report is JSON on stdout; exit zero
means its selected checks passed. Diagnostics contain codes and repository
relative files, without raw content, process errors or absolute paths.

## Markdown and path profile

Every tracked `.md` file must appear in `documents`. New Markdown cannot silently
fall outside coverage. The parser handles normal, reference, image and raw HTML
links, code fences, entities and duplicate heading slugs. Local paths are case
sensitive, confined to tracked regular files and checked without following
symlinks. Root-relative paths, directory README links, query strings and UTF-8
percent encoded fragments are supported. Code-file fragments must be bounded
GitHub line ranges. Other non-Markdown fragments fail explicitly.

HTML `href`, common media `src` and video `poster` attributes are checked. `srcset`
is refused. External HTTP(S), protocol-relative and mail links are counted without
fetching; unsupported schemes fail. The rendering profile is the pinned Marked
GFM profile, not every GitHub extension or the GitHub HTML sanitizer. Undefined
reference syntax that CommonMark treats as literal text is not a parsed link.
Headings and explicit HTML `id`/anchor `name` values provide local anchors.

Inputs have explicit bounds: 8,192 inventory entries, 512 Markdown documents,
1 MiB per selected file, 32 MiB total selected local input, 10,000 links and
100,000 HTML nodes per document, 64 JSON nesting levels, 256 references/claims,
64 benchmark manifests, 64 runs and 4,096 samples per run. Parsing operates on a
stable checkout; it is not an OS sandbox for concurrent hostile writers.

The test API accepts separate bare Git metadata for owned fixtures. A restricted
storage environment can set `ZVM_DOC_CHECK_STORAGE_PROFILE=regular-files-only`;
this skips two actual filesystem symlink cases and still tests the Git symlink
index mode. Such a run is partial local evidence. The default CI profile executes
both filesystem cases; it must not inherit the restricted environment setting.

## Manifest and evidence profile

The schema rejects unknown fields, duplicate identifiers and duplicate JSON
members. Every selected reference declares kind, format, exact bytes and SHA-256.
It names either a tracked local path or an external repository, full 40-character
revision and path. JSON references can require RFC 6901 pointers; Markdown
references can require headings. `comparisons` can require byte equality between
two selected inputs, for example duplicated encoding vectors.

Each claim names a document section, full source revision, selected input IDs
and nonempty limitations. Status is one of:

- `implementation`: selected local source/spec/vector bytes also checked against
  their declared full source revision in Git, without an execution claim.
- `plan`: proposed work, without a result record.
- `historical`: a retained local/hosted result at its stated source.
- `unavailable`: missing evidence, without a successful result.

A historical `result` reference must be JSON with `schema: 1`, `kind` (`local`
or `hosted`), `sourceRevision`, `complete: true`, `exitCode: 0`, `status: passed`,
`scope` and nonempty `limitations`. Checking it is record validation, not new
execution or CI artifact authentication. Unknown, skipped or incomplete output
cannot become a success. Committed results remain historical; fresh workflow
execution is recorded by the workflow system and its separately retained output.

The selected implementation revisions must be present in the local Git object
database. The workflow fetches history for that check. A shallow checkout without
the declared source object is unavailable; it does not qualify by assuming that
a matching revision string is enough.

A benchmark is `plan`, `unavailable` or `historical`, with a document section,
source revision and limitations. Only `historical` carries a `manifest` reference.
That JSON retains source revision, suite, OS/architecture/runtime, command, run
IDs/raw reference IDs, metrics and limits. Each raw JSON reference must be local
and pinned; it repeats source, suite, platform, command, run ID, actual successful
exit/completeness and raw metric/unit/value samples. Counts, units and
min/max/mean/median aggregates are recomputed. Duplicate runs, missing raw files,
mixed sources, incomplete output and fabricated aggregates fail. The tests include
small owned examples; those examples are not product performance results.

This checks declared evidence, not all natural-language claims. Add/update claim
records when changing source or result statements. Review benchmark methods,
platform representativeness, semantics and provenance independently. Existing
license/attribution notices are preserved; generated reports do not establish
anonymity. The [repository evidence page](../../docs/EVIDENCE.md) states this adoption's
scope and remaining roadmap work.

The unchanged portable code and test provenance is recorded in [UPSTREAM.json](UPSTREAM.json). This adoption changes inventory, evidence declarations and documentation only.
