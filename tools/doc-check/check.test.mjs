import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkRepository, parseMarkdown, privacyCodes } from './check.mjs';

const revision = '1'.repeat(40);
const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const linkFixtureOptions = { skip: process.env.ZVM_DOC_CHECK_STORAGE_PROFILE === 'regular-files-only'
  ? 'Owned storage profile refuses symbolic links; execute this native filesystem case in hosted CI.' : false };
function fixture(t) {
  const container = mkdtempSync(path.join(tmpdir(), 'zvm-doc-check-'));
  const root = path.join(container, 'tree'); mkdirSync(root);
  const gitDirectory = path.join(container, 'metadata');
  t.after(() => rmSync(container, { recursive: true, force: true }));
  function write(name, value) {
    const filename = path.join(root, name); mkdirSync(path.dirname(filename), { recursive: true });
    writeFileSync(filename, typeof value === 'string' || Buffer.isBuffer(value) ? value : JSON.stringify(value, null, 2) + '\n');
  }
  function git(...args) {
    const context = args[0] === 'init' ? [] : [`--git-dir=${gitDirectory}`, `--work-tree=${root}`];
    return execFileSync('git', [...context, '-c', 'core.hooksPath=/dev/null', '-c', 'commit.gpgsign=false', '-c', 'core.createObject=rename', ...args],
      { cwd: root, env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_AUTHOR_NAME: 'Fixture', GIT_AUTHOR_EMAIL: 'fixture@invalid.example',
        GIT_COMMITTER_NAME: 'Fixture', GIT_COMMITTER_EMAIL: 'fixture@invalid.example',
        GIT_AUTHOR_DATE: '2000-01-01T00:00:00 +0000', GIT_COMMITTER_DATE: '2000-01-01T00:00:00 +0000' },
        stdio: ['ignore', 'pipe', 'pipe'] });
  }
  git('init', '--bare', '-q', '--object-format=sha1', '--template=', gitDirectory);
  write('README.md', '# Example\n\n## Evidence\n\n## Performance\n');
  const manifest = { schema: 1, documents: ['README.md'], references: [], comparisons: [], claims: [], benchmarks: [] };
  function save() { write('tools/doc-check/manifest.json', manifest); }
  save(); git('add', '.'); git('commit', '-qm', 'Fixture source');
  function reference(id, name, kind = 'vector', format = 'json') {
    const bytes = readFileSync(path.join(root, name));
    const spec = { id, kind, path: name, format, sha256: digest(bytes), bytes: bytes.length };
    manifest.references.push(spec); return spec;
  }
  function check({ stage = true, inputs = new Map() } = {}) {
    save(); if (stage) git('add', '-A'); return checkRepository({ root, inputs, gitDirectory });
  }
  return { root, gitDirectory, write, git, reference, manifest, save, check };
}
function passed(report) { assert.equal(report.status, 'passed', JSON.stringify(report.issues)); }
function refused(report, code) {
  assert.equal(report.status, 'failed');
  if (code) assert.ok(report.issues.some(issue => issue.code === code), JSON.stringify(report.issues));
}
function claim(status = 'implementation') {
  return { id: 'selected-encoding', document: 'README.md', anchor: 'evidence', status, sourceRevision: revision,
    inputs: ['vector'], limitations: ['Selected fixture bytes only; no runtime or activation claim.'] };
}
function benchmark(f) {
  const platform = { os: 'fixture', architecture: 'fixture', runtime: 'fixture' };
  const command = ['fixture', '--measure'];
  for (const [i, samples] of [[1, [1, 3]], [2, [5, 7]]]) {
    f.write(`runs/raw-${i}.json`, { schema: 1, sourceRevision: revision, suite: 'fixture', platform, command,
      run: `run-${i}`, exitCode: 0, complete: true,
      samples: samples.map(value => ({ metric: 'duration', unit: 'ms', value })) });
    f.reference(`raw-${i}`, `runs/raw-${i}.json`, 'benchmark-raw');
  }
  const rawManifest = { schema: 1, sourceRevision: revision, suite: 'fixture', platform, command,
    runs: [{ id: 'run-1', raw: 'raw-1' }, { id: 'run-2', raw: 'raw-2' }],
    metrics: [{ name: 'duration', unit: 'ms', aggregation: 'median', sampleCount: 4, value: 4 }],
    limitations: ['Synthetic parser fixture; not a product benchmark.'] };
  function save() {
    f.write('runs/manifest.json', rawManifest);
    f.manifest.references = f.manifest.references.filter(ref => ref.id !== 'raw-manifest');
    f.reference('raw-manifest', 'runs/manifest.json', 'benchmark-manifest');
  }
  save();
  f.manifest.benchmarks.push({ id: 'fixture-history', document: 'README.md', anchor: 'performance',
    status: 'historical', sourceRevision: revision, manifest: 'raw-manifest', limitations: ['Historical owned fixture only.'] });
  return { rawManifest, save };
}

test('plain tracked Markdown passes without execution or remote availability claims', t => {
  const f = fixture(t); const report = f.check(); passed(report);
  assert.equal(report.offlineOnly, true); assert.equal(report.networkRequests, 0);
  assert.equal(report.remoteAvailabilityChecked, false); assert.equal(report.markdownFiles, 1);
  assert.match(report.selectedInputSha256, /^[a-f0-9]{64}$/);
});
test('mature Markdown profile handles inline markup, entities and duplicate slugs', () => {
  const doc = parseMarkdown('# Hello *World* &amp; `Code`\n# Foo\n# Foo\n# Foo-1\n# Foo\n');
  assert.deepEqual([...doc.anchors], ['hello-world--code', 'foo', 'foo-1', 'foo-1-1', 'foo-2']);
});
test('Unicode headings remain available without ASCII transliteration', () => {
  assert.ok(parseMarkdown('# \u65e5\u672c\u8a9e\n').anchors.has('\u65e5\u672c\u8a9e'));
});
test('fenced and inline code are not interpreted as links', t => {
  const f = fixture(t); f.write('README.md', '# Example\n`[fake](missing.md)`\n\n```md\n[fake](missing.md)\n```\n');
  const report = f.check(); passed(report); assert.equal(report.localLinks, 0);
});
test('reference links, nested labels, query strings and percent encoded spaces resolve', t => {
  const f = fixture(t);
  f.write('README.md', '# Example\n[**nested** label][ref]\n\n[ref]: <docs/With space.md?view=1#part> "title"\n');
  f.write('docs/With space.md', '# Part\n'); f.manifest.documents.push('docs/With space.md');
  const report = f.check(); passed(report); assert.equal(report.localLinks, 1);
});
test('raw HTML links, decoded entities and named anchors are checked', t => {
  const f = fixture(t); f.write('README.md', '# Example\n<a id="target"></a>\n<a href="#target">yes</a>\n<img src="image.svg?x=1&amp;y=2">\n');
  f.write('image.svg', '<svg/>'); passed(f.check());
});
test('HTML comments do not create phantom links', t => {
  const f = fixture(t); f.write('README.md', '# Example\n<!-- <a href="absent">hidden</a> -->\n'); passed(f.check());
});
test('directory, repository-root and root-relative links resolve', t => {
  const f = fixture(t); f.write('README.md', '# Example\n[x](docs/) [y](/docs/README.md#part)\n');
  f.write('docs/README.md', '# Part\n[root](../#example)\n'); f.manifest.documents.push('docs/README.md'); passed(f.check());
});
test('directory links without README permit only a path without fragment', t => {
  const f = fixture(t); f.write('src/file.txt', 'source'); f.write('README.md', '# Example\n[source](src/)\n'); passed(f.check());
  f.write('README.md', '# Example\n[source](src/#missing)\n'); refused(f.check(), 'MISSING_LOCAL_TARGET');
});
test('line ranges on source files are bounded by actual line count', t => {
  const f = fixture(t); f.write('source.txt', 'one\ntwo\n');
  f.write('README.md', '# Example\n[lines](source.txt#L1-L2)\n'); passed(f.check());
  f.write('README.md', '# Example\n[lines](source.txt#L2-L3)\n'); refused(f.check(), 'LINE_ANCHOR_OUT_OF_RANGE');
});
for (const [name, href, code] of [
  ['missing file', 'missing.md', 'MISSING_LOCAL_TARGET'],
  ['missing heading', '#missing', 'MISSING_LOCAL_ANCHOR'],
  ['case mismatch', 'readme.md', 'MISSING_LOCAL_TARGET'],
  ['repository escape', '../outside.md', 'LINK_OUTSIDE_REPOSITORY'],
  ['encoded separator', 'docs%2Fitem.md', 'ENCODED_SEPARATOR'],
  ['encoded traversal', '%2e%2e/outside.md', 'LINK_OUTSIDE_REPOSITORY'],
  ['unsupported URI', 'javascript:alert(1)', 'UNSUPPORTED_LINK_SCHEME'],
  ['invalid source anchor', 'source.txt#counter', 'UNSUPPORTED_FILE_ANCHOR'],
  ['reversed line range', 'source.txt#L2-L1', 'LINE_ANCHOR_OUT_OF_RANGE'],
]) test(name + ' fails closed', t => {
  const f = fixture(t); f.write('source.txt', 'one\ntwo\n'); f.write('README.md', `# Example\n[link](<${href}>)\n`); refused(f.check(), code);
});
test('invalid percent encoding is refused with sanitized diagnostics', t => {
  const f = fixture(t); f.write('README.md', '# Example\n[bad](file%ZZ.md)\n'); refused(f.check());
});
test('remote links are counted without network access or availability claims', t => {
  const f = fixture(t); f.write('README.md', '# Example\n[remote](https://example.invalid/not-available#no-heading)\n');
  const report = f.check(); passed(report); assert.equal(report.remoteLinksNotFetched, 1); assert.equal(report.networkRequests, 0);
});
test('unsupported HTML srcset is explicit instead of silently unverified', t => {
  const f = fixture(t); f.write('README.md', '# Example\n<img srcset="one.png 1x, two.png 2x">\n'); refused(f.check(), 'UNSUPPORTED_SRCSET');
});
test('new tracked Markdown must be covered by the manifest', t => {
  const f = fixture(t); f.write('extra.md', '# Extra\n'); refused(f.check(), 'DOCUMENT_COVERAGE_MISMATCH');
});
test('untracked files cannot satisfy a local link', t => {
  const f = fixture(t); f.write('README.md', '# Example\n[x](untracked.txt)\n'); f.git('add', 'README.md');
  f.write('untracked.txt', 'not tracked'); refused(f.check({ stage: false }), 'MISSING_LOCAL_TARGET');
});
test('tracked symlink targets are refused', linkFixtureOptions, t => {
  const f = fixture(t); f.write('source.txt', 'source'); symlinkSync('source.txt', path.join(f.root, 'link.txt'));
  f.write('README.md', '# Example\n[x](link.txt)\n'); refused(f.check(), 'LINK_TO_NONREGULAR_FILE');
});
test('a symbolic-link Git index mode is refused without requiring a filesystem link', t => {
  const f = fixture(t); f.write('link.txt', 'source.txt'); f.write('README.md', '# Example\n[x](link.txt)\n');
  f.save(); f.git('add', '.');
  const blob = f.git('hash-object', '-w', 'link.txt').toString().trim();
  f.git('update-index', '--cacheinfo', `120000,${blob},link.txt`);
  refused(f.check({ stage: false }), 'LINK_TO_NONREGULAR_FILE');
});
test('replaced directory symlink is refused before following its child', linkFixtureOptions, t => {
  const f = fixture(t); f.write('docs/item.txt', 'source'); f.write('README.md', '# Example\n[x](docs/item.txt)\n');
  f.git('add', '.'); rmSync(path.join(f.root, 'docs'), { recursive: true }); mkdirSync(path.join(f.root, 'replacement'));
  f.write('replacement/item.txt', 'source'); symlinkSync('replacement', path.join(f.root, 'docs'));
  refused(f.check({ stage: false }), 'SYMLINK_REFUSED');
});
test('oversized Markdown is refused before parsing', t => {
  const f = fixture(t); f.write('README.md', 'x'.repeat(1024 * 1024 + 1)); refused(f.check(), 'UNSAFE_OR_OVERSIZED_FILE');
});
test('invalid UTF-8 Markdown is not normalized into success', t => {
  const f = fixture(t); f.write('README.md', Buffer.from([0xc0, 0xaf])); refused(f.check());
});
test('selected JSON vector bytes and escaped JSON pointers are pinned', t => {
  const f = fixture(t); f.write('vectors.json', { 'a/b': { '~key': [0] }, version: 1 });
  f.reference('vector', 'vectors.json').pointers = ['/a~1b/~0key/0', '/version']; passed(f.check());
});
for (const [name, change, code] of [
  ['changed bytes', ref => { ref.sha256 = '0'.repeat(64); }, 'REFERENCE_DIGEST_MISMATCH'],
  ['wrong length', ref => { ref.bytes++; }, 'REFERENCE_DIGEST_MISMATCH'],
  ['missing pointer', ref => { ref.pointers = ['/absent']; }, 'MISSING_JSON_POINTER'],
  ['invalid pointer escape', ref => { ref.pointers = ['/a~2']; }, 'INVALID_JSON_POINTER'],
  ['unknown reference field', ref => { ref.optional = true; }, 'INVALID_FIELDS'],
]) test(name + ' in selected vectors is refused', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); const ref = f.reference('vector', 'vectors.json'); change(ref); refused(f.check(), code);
});
test('duplicate JSON object members, including escaped keys, are refused', t => {
  const f = fixture(t); f.write('vectors.json', '{"nested":{"version":1,"vers\\u0069on":2}}');
  f.reference('vector', 'vectors.json'); refused(f.check(), 'DUPLICATE_JSON_KEY');
});
test('duplicate manifest fields are refused rather than overwritten', t => {
  const f = fixture(t); f.save(); const filename = 'tools/doc-check/manifest.json';
  f.write(filename, readFileSync(path.join(f.root, filename), 'utf8').replace('"schema": 1', '"schema": 2, "schema": 1'));
  f.git('add', '.'); refused(checkRepository({ root: f.root, gitDirectory: f.gitDirectory }), 'DUPLICATE_JSON_KEY');
});
test('JSON nesting is bounded', t => {
  const f = fixture(t); f.write('vectors.json', '['.repeat(66) + '0' + ']'.repeat(66)); f.reference('vector', 'vectors.json'); refused(f.check(), 'JSON_DEPTH_LIMIT');
});
test('byte comparison binds duplicate selected vector copies', t => {
  const f = fixture(t); f.write('a.json', { version: 1 }); f.write('b.json', { version: 1 });
  f.reference('vector-a', 'a.json'); f.reference('vector-b', 'b.json');
  f.manifest.comparisons.push({ left: 'vector-a', right: 'vector-b', mode: 'bytes' }); passed(f.check());
  f.write('b.json', { version: 2 }); f.manifest.references.pop(); f.reference('vector-b', 'b.json'); refused(f.check(), 'REFERENCE_COMPARISON_MISMATCH');
});
test('external spec content, full source revision and selected heading are required', t => {
  const f = fixture(t); const data = Buffer.from('# Pinned spec\n\n## Vectors\n'); const input = path.join(f.root, 'supplied-spec.txt'); writeFileSync(input, data);
  f.manifest.references.push({ id: 'spec', kind: 'spec', external: { repository: 'example/spec', revision, path: 'SPEC.md' },
    sha256: digest(data), bytes: data.length, format: 'markdown', anchors: ['vectors'] });
  refused(f.check(), 'EXTERNAL_INPUT_REQUIRED');
  const inputs = new Map([['spec', input]]); const report = f.check({ inputs }); passed(report);
  assert.equal(report.references[0].sourceAuthorityAuthenticated, false);
  f.manifest.references[0].external.revision = 'main'; refused(f.check({ inputs }), 'UNPINNED_EXTERNAL_SOURCE');
});
test('external spec digest and anchor mismatches are refused', t => {
  const f = fixture(t); const data = Buffer.from('# Spec\n'); f.write('input.txt', data);
  const spec = { id: 'spec', kind: 'spec', external: { repository: 'example/spec', revision, path: 'SPEC.md' },
    sha256: digest(data), bytes: data.length, format: 'markdown', anchors: ['missing'] };
  f.manifest.references.push(spec); const inputs = new Map([['spec', path.join(f.root, 'input.txt')]]);
  refused(f.check({ inputs }), 'MISSING_SPEC_ANCHOR'); spec.anchors = ['spec']; spec.sha256 = '0'.repeat(64);
  refused(f.check({ inputs }), 'REFERENCE_DIGEST_MISMATCH');
});
test('undeclared external inputs do not silently disappear', t => {
  const f = fixture(t); refused(f.check({ inputs: new Map([['unexpected', path.join(f.root, 'README.md')]]) }), 'UNDECLARED_EXTERNAL_INPUT');
});
test('implementation claims require selected inputs, location and limits', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json');
  f.git('add', '.'); f.git('commit', '-qm', 'Selected fixture input');
  const item = claim(); item.sourceRevision = f.git('rev-parse', 'HEAD').toString().trim(); f.manifest.claims.push(item);
  passed(f.check()); f.manifest.claims[0].inputs = []; refused(f.check(), 'IMPLEMENTATION_INPUT_REQUIRED');
});
test('an implementation source revision must contain the selected current bytes', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json');
  f.git('add', '.'); f.git('commit', '-qm', 'Selected fixture input');
  const item = claim(); item.sourceRevision = f.git('rev-parse', 'HEAD').toString().trim(); f.manifest.claims.push(item);
  f.write('vectors.json', { version: 2 }); f.manifest.references = []; f.reference('vector', 'vectors.json');
  refused(f.check(), 'IMPLEMENTATION_SOURCE_MISMATCH');
});
for (const value of [null, true, 17]) test('non-string reference ID is refused: ' + String(value), t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json').id = value;
  refused(f.check(), 'INVALID_OR_DUPLICATE_ID');
});
test('present but null optional policy fields cannot silently disable checks', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); const ref = f.reference('vector', 'vectors.json');
  ref.pointers = null; refused(f.check(), 'INVALID_LIST'); delete ref.pointers;
  ref.external = null; refused(f.check(), 'AMBIGUOUS_REFERENCE');
});
test('benchmark platform field order has no semantic meaning', t => {
  const f = fixture(t); benchmark(f); const name = 'runs/raw-1.json'; const raw = JSON.parse(readFileSync(path.join(f.root, name), 'utf8'));
  raw.platform = { runtime: raw.platform.runtime, architecture: raw.platform.architecture, os: raw.platform.os };
  f.write(name, raw); f.manifest.references = f.manifest.references.filter(ref => ref.id !== 'raw-1'); f.reference('raw-1', name, 'benchmark-raw');
  passed(f.check());
});
test('plans cannot carry a successful result', t => {
  const f = fixture(t); const item = claim('plan'); item.inputs = []; item.result = 'result'; f.manifest.claims.push(item);
  refused(f.check(), 'NONRESULT_CLAIM_HAS_RESULT');
});
test('retained local and hosted results remain historical with exact source pins', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json');
  const item = claim('historical'); item.result = 'result'; f.manifest.claims.push(item);
  for (const kind of ['local', 'hosted']) {
    f.write('result.json', { schema: 1, kind, sourceRevision: revision, complete: true, exitCode: 0,
      status: 'passed', scope: 'Owned fixture only.', limitations: ['A retained record is not current execution.'] });
    f.manifest.references = f.manifest.references.filter(ref => ref.id !== 'result'); f.reference('result', 'result.json', 'result');
    const report = f.check(); passed(report); assert.equal(report.claims[0].status, 'historical'); assert.equal(report.claims[0].executionPerformed, false);
  }
});
for (const [name, field, value] of [['wrong source', 'sourceRevision', '2'.repeat(40)], ['incomplete', 'complete', false],
  ['nonzero exit', 'exitCode', 1], ['skipped', 'status', 'skipped'], ['unknown kind', 'kind', 'current']]) {
  test(name + ' cannot qualify a historical result', t => {
    const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json');
    f.write('result.json', { schema: 1, kind: 'local', sourceRevision: revision, complete: true, exitCode: 0,
      status: 'passed', scope: 'Owned fixture only.', limitations: ['Not production evidence.'], [field]: value });
    f.reference('result', 'result.json', 'result'); const item = claim('historical'); item.result = 'result'; f.manifest.claims.push(item); refused(f.check());
  });
}
test('current execution and production claim status cannot be invented', t => {
  const f = fixture(t); f.write('vectors.json', { version: 1 }); f.reference('vector', 'vectors.json');
  f.manifest.claims.push(claim('current')); refused(f.check(), 'INVALID_CLAIM_STATUS');
  f.manifest.claims[0].status = 'production'; refused(f.check(), 'INVALID_CLAIM_STATUS');
});
test('empty evidence limits and missing section anchors are refused', t => {
  const f = fixture(t); const item = claim('plan'); item.inputs = []; f.manifest.claims.push(item);
  item.limitations = []; refused(f.check(), 'INVALID_LIST'); item.limitations = ['Owned fixture only.']; item.anchor = 'absent'; refused(f.check(), 'MISSING_EVIDENCE_LOCATION');
});
test('benchmark manifests retain raw samples and recompute median, mean, min and max', t => {
  const f = fixture(t); const b = benchmark(f);
  for (const [aggregation, value] of [['median', 4], ['mean', 4], ['min', 1], ['max', 7]]) {
    Object.assign(b.rawManifest.metrics[0], { aggregation, value }); b.save(); const report = f.check(); passed(report);
    assert.equal(report.benchmarks[0].executionPerformed, false);
  }
});
for (const [name, mutate, code] of [
  ['fabricated aggregate', b => { b.metrics[0].value = 99; }, 'BENCHMARK_AGGREGATE_MISMATCH'],
  ['wrong count', b => { b.metrics[0].sampleCount = 5; }, 'BENCHMARK_AGGREGATE_MISMATCH'],
  ['wrong unit', b => { b.metrics[0].unit = 's'; }, 'METRIC_UNIT_MISMATCH'],
  ['missing raw input', b => { b.runs[0].raw = 'absent'; }, 'MISSING_OR_WRONG_REFERENCE'],
  ['duplicated raw run', b => { b.runs[1].raw = b.runs[0].raw; }, 'DUPLICATE_ENTRY'],
  ['missing metric', b => { b.metrics = []; }, 'INVALID_LIST'],
  ['wrong source', b => { b.sourceRevision = '2'.repeat(40); }, 'BENCHMARK_SOURCE_MISMATCH'],
  ['missing limits', b => { b.limitations = []; }, 'INVALID_LIST'],
]) test(name + ' cannot qualify a historical benchmark', t => {
  const f = fixture(t); const b = benchmark(f); mutate(b.rawManifest); b.save(); refused(f.check(), code);
});
test('failed and incomplete raw benchmark runs are refused', t => {
  const f = fixture(t); benchmark(f); const name = 'runs/raw-1.json'; const raw = JSON.parse(readFileSync(path.join(f.root, name), 'utf8'));
  for (const [field, value] of [['complete', false], ['exitCode', 2], ['run', 'other-run'], ['sourceRevision', '3'.repeat(40)]]) {
    f.write(name, { ...raw, [field]: value }); f.manifest.references = f.manifest.references.filter(ref => ref.id !== 'raw-1'); f.reference('raw-1', name, 'benchmark-raw');
    refused(f.check(), 'BENCHMARK_RAW_MISMATCH');
  }
});
test('invalid sample numbers and mixed units are refused', t => {
  const f = fixture(t); benchmark(f); const name = 'runs/raw-1.json'; const raw = JSON.parse(readFileSync(path.join(f.root, name), 'utf8'));
  for (const value of [-1, null, '3']) {
    raw.samples[0].value = value; f.write(name, raw); f.manifest.references = f.manifest.references.filter(ref => ref.id !== 'raw-1'); f.reference('raw-1', name, 'benchmark-raw'); refused(f.check(), 'INVALID_MEASUREMENT');
  }
});
test('planned and unavailable benchmarks cannot smuggle measured output', t => {
  const f = fixture(t); benchmark(f); f.manifest.benchmarks[0].status = 'plan'; refused(f.check(), 'UNEXECUTED_BENCHMARK_HAS_RESULTS');
  f.manifest.benchmarks[0].status = 'measured'; refused(f.check(), 'INVALID_BENCHMARK_STATUS');
});
for (const [name, value, code] of [
  ['private path', '/' + 'Users/example/work', 'PRIVATE_PATH'],
  ['volume path', '/' + 'Volumes/Example/work', 'PRIVATE_PATH'],
  ['Windows user path', 'C:' + '\\Users\\example\\work', 'PRIVATE_PATH'],
  ['file URI', 'file:' + '///tmp/private', 'PRIVATE_PATH'],
  ['email identity', 'person' + '@example.invalid', 'IDENTITY_EMAIL'],
  ['Turkish-specific letter', '\u0131', 'ENGLISH_REVIEW_REQUIRED'],
  ['token material', 'ghp_' + 'x'.repeat(24), 'SECRET_MATERIAL'],
  ['private key marker', '-----BEGIN ' + 'PRIVATE KEY-----', 'SECRET_MATERIAL'],
]) test(name + ' is refused without echoing the matched value', t => {
  const f = fixture(t); f.write('README.md', '# Example\n' + value + '\n'); const report = f.check(); refused(report, code);
  assert.equal(JSON.stringify(report).includes(value), false);
});
test('privacy guards apply to manifest limitations and raw result records', t => {
  const f = fixture(t); const item = claim('plan'); item.inputs = []; item.limitations = ['Saved in /' + 'Users/example/work'];
  f.manifest.claims.push(item); refused(f.check(), 'PRIVATE_PATH');
});
test('publication guard is explicitly limited rather than an anonymity claim', () => {
  assert.deepEqual(privacyCodes('English text with a public repository link.'), []);
  assert.deepEqual(privacyCodes('A person can still be identified by contextual data.'), []);
});
test('CLI rejects unknown options and omits local paths from errors', () => {
  const cli = fileURLToPath(new URL('./cli.mjs', import.meta.url));
  const result = spawnSync(process.execPath, [cli, '--unknown', '/' + 'Users/example'], { encoding: 'utf8' });
  assert.equal(result.status, 1); const report = JSON.parse(result.stdout);
  assert.equal(report.issues[0].code, 'INVALID_ARGUMENT_OR_UNAVAILABLE_TOOL'); assert.equal(result.stdout.includes('/' + 'Users/'), false);
});
test('CLI refuses duplicate inputs before reading them', () => {
  const cli = fileURLToPath(new URL('./cli.mjs', import.meta.url));
  const result = spawnSync(process.execPath, [cli, '--input', 'spec=one', '--input', 'spec=two'], { encoding: 'utf8' });
  assert.equal(result.status, 1); assert.equal(JSON.parse(result.stdout).issues[0].code, 'INVALID_ARGUMENT_OR_UNAVAILABLE_TOOL');
});
