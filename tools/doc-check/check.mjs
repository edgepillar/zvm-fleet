import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstatSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { isDeepStrictEqual, TextDecoder } from 'node:util';
import { Marked } from 'marked';
import Slugger from 'github-slugger';
import { parseFragment } from 'parse5';

const markdown = new Marked({ gfm: true, breaks: false, pedantic: false, async: false });
const decoder = new TextDecoder('utf-8', { fatal: true });
const HEX = /^[a-f0-9]{64}$/;
const REVISION = /^[a-f0-9]{40}$/;
const ID = /^[a-z][a-z0-9-]{0,63}$/;
const isID = value => typeof value === 'string' && ID.test(value);
const MAX_FILE = 1024 * 1024;
const MAX_TOTAL = 32 * MAX_FILE;
const MAX_RECORDS = 256;
const sha = bytes => createHash('sha256').update(bytes).digest('hex');

class Refusal extends Error {
  constructor(code) { super(code); this.code = code; }
}
function requireValue(condition, code) { if (!condition) throw new Refusal(code); }
function object(value, required, optional = []) {
  requireValue(value && typeof value === 'object' && !Array.isArray(value), 'INVALID_RECORD');
  requireValue(required.every(key => Object.hasOwn(value, key)) &&
    Object.keys(value).every(key => [...required, ...optional].includes(key)), 'INVALID_FIELDS');
}
function array(value, cap = MAX_RECORDS, minimum = 0) {
  requireValue(Array.isArray(value) && value.length >= minimum && value.length <= cap, 'INVALID_LIST');
  return value;
}
function unique(values) { requireValue(new Set(values).size === values.length, 'DUPLICATE_ENTRY'); }
function text(value) {
  requireValue(typeof value === 'string' && value.length > 0 && value.length <= 2048 &&
    !/[\x00-\x08\x0b-\x1f\x7f]/.test(value), 'INVALID_TEXT');
}
function limits(value) { array(value, 32, 1).forEach(text); }
function relative(value) {
  requireValue(typeof value === 'string' && /^[A-Za-z0-9._ /-]+$/.test(value) &&
    !value.startsWith('/') && !value.includes('\\') &&
    value.split('/').every(part => part && part !== '.' && part !== '..'), 'UNSAFE_PATH');
  return value;
}
function publicFile(value) {
  return typeof value === 'string' && /^[A-Za-z0-9._ /-]+$/.test(value) &&
    !value.startsWith('/') && !value.split('/').includes('..') ? value : undefined;
}

// These are publication guards, not a language classifier or a secret scanner.
export function privacyCodes(value) {
  const codes = [];
  if (/(?:\/Users\/|\/Volumes\/|\/home\/[^\s/]+\/|[A-Za-z]:\\Users\\|file:\/\/)/i.test(value)) codes.push('PRIVATE_PATH');
  if (/(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN (?:[A-Z ]*PRIVATE KEY|OPENSSH PRIVATE KEY)-----)/.test(value)) codes.push('SECRET_MATERIAL');
  if (/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/i.test(value)) codes.push('IDENTITY_EMAIL');
  if (/[\u011e\u011f\u0130\u0131\u015e\u015f\u00c7\u00e7\u00d6\u00f6\u00dc\u00fc]/u.test(value)) codes.push('ENGLISH_REVIEW_REQUIRED');
  return codes;
}

function walk(root, callback) {
  const pending = [root];
  let visited = 0;
  while (pending.length) {
    const node = pending.pop();
    requireValue(++visited <= 100000, 'HTML_NODE_LIMIT');
    callback(node);
    if (node.content) pending.push(node.content);
    if (node.childNodes) pending.push(...node.childNodes.toReversed());
  }
}
function nodeText(root) {
  let result = '';
  walk(root, node => {
    if (node.nodeName === '#text') result += node.value;
    if (node.tagName === 'img') result += node.attrs.find(attr => attr.name === 'alt')?.value ?? '';
  });
  return result;
}
export function parseMarkdown(source) {
  requireValue(Buffer.byteLength(source) <= MAX_FILE, 'FILE_LIMIT');
  const root = parseFragment(markdown.parse(source));
  const anchors = new Set();
  const links = [];
  const slugger = new Slugger();
  walk(root, node => {
    if (!node.tagName) return;
    const attrs = Object.fromEntries(node.attrs.map(attr => [attr.name, attr.value]));
    if (/^h[1-6]$/.test(node.tagName)) anchors.add(slugger.slug(nodeText(node)));
    if (attrs.id) anchors.add(attrs.id);
    if (node.tagName === 'a' && attrs.name) anchors.add(attrs.name);
    if (['a', 'area', 'link'].includes(node.tagName) && Object.hasOwn(attrs, 'href')) links.push(attrs.href);
    if (['img', 'source', 'video', 'audio', 'track', 'iframe'].includes(node.tagName) && Object.hasOwn(attrs, 'src')) links.push(attrs.src);
    if (node.tagName === 'video' && attrs.poster) links.push(attrs.poster);
    requireValue(!Object.hasOwn(attrs, 'srcset'), 'UNSUPPORTED_SRCSET');
    requireValue(links.length <= 10000, 'LINK_LIMIT');
  });
  return { anchors, links };
}

function parseJson(bytes) {
  // JSON.parse otherwise silently overwrites duplicate members. Reject duplicate
  // keys at every nesting depth before using any policy or result field.
  const source = decoder.decode(bytes);
  let i = 0;
  const whitespace = () => { while (/\s/.test(source[i] ?? '') && i < source.length) i++; };
  function string() {
    const start = i++;
    while (i < source.length) {
      if (source[i] === '\\') { i += 2; continue; }
      if (source[i++] === '"') return JSON.parse(source.slice(start, i));
    }
    throw new Refusal('INVALID_JSON');
  }
  function value(depth) {
    requireValue(depth <= 64, 'JSON_DEPTH_LIMIT'); whitespace();
    if (source[i] === '{') {
      i++; whitespace(); const keys = new Set();
      if (source[i] === '}') { i++; return; }
      for (;;) {
        requireValue(source[i] === '"', 'INVALID_JSON'); const key = string();
        requireValue(!keys.has(key), 'DUPLICATE_JSON_KEY'); keys.add(key);
        whitespace(); requireValue(source[i++] === ':', 'INVALID_JSON'); value(depth + 1); whitespace();
        if (source[i] === '}') { i++; return; }
        requireValue(source[i++] === ',', 'INVALID_JSON'); whitespace();
      }
    }
    if (source[i] === '[') {
      i++; whitespace(); if (source[i] === ']') { i++; return; }
      for (;;) {
        value(depth + 1); whitespace(); if (source[i] === ']') { i++; return; }
        requireValue(source[i++] === ',', 'INVALID_JSON');
      }
    }
    if (source[i] === '"') { string(); return; }
    const match = /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(source.slice(i));
    requireValue(match, 'INVALID_JSON'); i += match[0].length;
  }
  try { value(0); whitespace(); requireValue(i === source.length, 'INVALID_JSON'); return JSON.parse(source); }
  catch (error) { if (error instanceof Refusal) throw error; throw new Refusal('INVALID_JSON'); }
}
function pointer(value, selected) {
  requireValue(typeof selected === 'string' && (selected === '' || selected.startsWith('/')) &&
    !/~(?![01])/u.test(selected), 'INVALID_JSON_POINTER');
  if (selected === '') return value;
  for (const encoded of selected.slice(1).split('/')) {
    const key = encoded.replaceAll('~1', '/').replaceAll('~0', '~');
    requireValue(value !== null && typeof value === 'object' && Object.hasOwn(value, key), 'MISSING_JSON_POINTER');
    value = value[key];
  }
  return value;
}
function git(root, args, gitDirectory) {
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('GIT_')));
  env.GIT_CONFIG_NOSYSTEM = '1'; env.GIT_CONFIG_GLOBAL = '/dev/null';
  const context = gitDirectory ? [`--git-dir=${gitDirectory}`, `--work-tree=${root}`] : ['-C', root];
  try { return execFileSync('git', [...context, ...args], { env, maxBuffer: 2 * MAX_FILE, stdio: ['ignore', 'pipe', 'pipe'], timeout: 10000 }); }
  catch { throw new Refusal('GIT_INVENTORY_UNAVAILABLE'); }
}
function inventory(root, gitDirectory) {
  const names = new Map();
  for (const entry of decoder.decode(git(root, ['ls-files', '--stage', '-z'], gitDirectory)).split('\0').filter(Boolean)) {
    const match = /^(\d{6}) ([a-f0-9]{40,64}) (\d)\t(.+)$/.exec(entry);
    requireValue(match && match[3] === '0', 'UNMERGED_INVENTORY');
    relative(match[4]); requireValue(!names.has(match[4]), 'DUPLICATE_ENTRY');
    names.set(match[4], match[1]);
  }
  requireValue(names.size > 0 && names.size <= 8192, 'INVENTORY_LIMIT');
  return names;
}
function readRegular(filename, cap = MAX_FILE) {
  for (let parent = path.resolve(filename);;) {
    requireValue(!lstatSync(parent).isSymbolicLink(), 'SYMLINK_REFUSED');
    const next = path.dirname(parent); if (next === parent) break; parent = next;
  }
  const info = lstatSync(filename);
  requireValue(info.isFile() && !info.isSymbolicLink() && info.size <= cap, 'UNSAFE_OR_OVERSIZED_FILE');
  const data = readFileSync(filename);
  requireValue(data.length === info.size && data.length <= cap, 'FILE_CHANGED_OR_OVERSIZED');
  return data;
}
function resultRecord(record, claim) {
  object(record, ['schema', 'kind', 'sourceRevision', 'complete', 'exitCode', 'status', 'scope', 'limitations']);
  requireValue(record.schema === 1 && record.sourceRevision === claim.sourceRevision &&
    record.complete === true && record.exitCode === 0 && record.status === 'passed', 'UNQUALIFIED_RESULT');
  requireValue(record.kind === 'local' || record.kind === 'hosted', 'INVALID_RESULT_KIND');
  text(record.scope); limits(record.limitations);
}
function benchmarkRecord(record, benchmark, reference) {
  object(record, ['schema', 'sourceRevision', 'suite', 'platform', 'command', 'runs', 'metrics', 'limitations']);
  requireValue(record.schema === 1 && record.sourceRevision === benchmark.sourceRevision, 'BENCHMARK_SOURCE_MISMATCH');
  text(record.suite); object(record.platform, ['os', 'architecture', 'runtime']);
  Object.values(record.platform).forEach(text); array(record.command, 64, 1).forEach(text); limits(record.limitations);
  array(record.runs, 64, 1); unique(record.runs.map(run => run.id)); unique(record.runs.map(run => run.raw));
  const values = new Map();
  for (const run of record.runs) {
    object(run, ['id', 'raw']); requireValue(isID(run.id), 'INVALID_ID');
    const raw = reference(run.raw, 'benchmark-raw');
    requireValue(raw.spec.path, 'RAW_BENCHMARK_NOT_RETAINED_LOCALLY');
    const sample = raw.json;
    object(sample, ['schema', 'sourceRevision', 'suite', 'platform', 'command', 'run', 'exitCode', 'complete', 'samples']);
    requireValue(sample.schema === 1 && sample.sourceRevision === record.sourceRevision && sample.suite === record.suite &&
      isDeepStrictEqual(sample.platform, record.platform) && isDeepStrictEqual(sample.command, record.command) &&
      sample.run === run.id && sample.exitCode === 0 && sample.complete === true, 'BENCHMARK_RAW_MISMATCH');
    for (const item of array(sample.samples, 4096, 1)) {
      object(item, ['metric', 'unit', 'value']); requireValue(isID(item.metric), 'INVALID_ID'); text(item.unit);
      requireValue(typeof item.value === 'number' && Number.isFinite(item.value) && item.value >= 0, 'INVALID_MEASUREMENT');
      if (!values.has(item.metric)) values.set(item.metric, { unit: item.unit, samples: [] });
      const group = values.get(item.metric); requireValue(group.unit === item.unit, 'METRIC_UNIT_MISMATCH');
      group.samples.push(item.value);
    }
  }
  array(record.metrics, 64, 1); unique(record.metrics.map(metric => metric.name));
  requireValue(record.metrics.length === values.size, 'METRIC_COVERAGE_MISMATCH');
  for (const metric of record.metrics) {
    object(metric, ['name', 'unit', 'aggregation', 'sampleCount', 'value']);
    const group = values.get(metric.name); requireValue(group && metric.unit === group.unit, 'METRIC_UNIT_MISMATCH');
    const samples = group.samples.toSorted((a, b) => a - b);
    const n = samples.length;
    const aggregations = { min: () => samples[0], max: () => samples.at(-1),
      mean: () => samples.reduce((a, b) => a + b, 0) / n,
      median: () => n % 2 ? samples[(n - 1) / 2] : (samples[n / 2 - 1] + samples[n / 2]) / 2 };
    requireValue(Object.hasOwn(aggregations, metric.aggregation) && metric.sampleCount === n &&
      Number.isFinite(metric.value) && metric.value === aggregations[metric.aggregation](), 'BENCHMARK_AGGREGATE_MISMATCH');
  }
}

export function checkRepository({ root, manifest: manifestPath = 'tools/doc-check/manifest.json', inputs = new Map(), gitDirectory }) {
  const report = { schema: 1, status: 'failed', offlineOnly: true, remoteAvailabilityChecked: false,
    networkRequests: 0, repositoryRevision: null, trackedChanges: null,
    markdownFiles: 0, localLinks: 0, remoteLinksNotFetched: 0, references: [], claims: [], benchmarks: [], issues: [],
    evidenceLimits: ['Checks bind selected file bytes and structured records; they do not execute vector generators or benchmarks.',
      'Historical records are not current execution. Hosted records require independent run/source/artifact qualification.',
      'Publication guards require human English and privacy review; they do not establish anonymity or scan every secret format.',
      'Markdown rendering follows the pinned parser profile, not every extension or sanitizer in the GitHub service.'] };
  let currentFile;
  try {
    root = path.resolve(root);
    requireValue(!lstatSync(root).isSymbolicLink() && lstatSync(root).isDirectory(), 'UNSAFE_ROOT');
    if (gitDirectory) requireValue(lstatSync(gitDirectory).isDirectory() && !lstatSync(gitDirectory).isSymbolicLink(), 'UNSAFE_GIT_DIRECTORY');
    const names = inventory(root, gitDirectory);
    report.repositoryRevision = decoder.decode(git(root, ['rev-parse', 'HEAD'], gitDirectory)).trim();
    requireValue(REVISION.test(report.repositoryRevision), 'INVALID_REVISION');
    report.trackedChanges = git(root, ['status', '--porcelain', '--untracked-files=no'], gitDirectory).length > 0;
    const reads = new Map(); let total = 0;
    function read(name) {
      relative(name); currentFile = name;
      requireValue(['100644', '100755'].includes(names.get(name)), 'UNTRACKED_OR_NONREGULAR_FILE');
      if (reads.has(name)) return reads.get(name);
      let filename = root;
      for (const part of name.split('/')) {
        filename = path.join(filename, part);
        requireValue(!lstatSync(filename).isSymbolicLink(), 'SYMLINK_REFUSED');
      }
      const data = readRegular(filename);
      total += data.length; requireValue(total <= MAX_TOTAL, 'TOTAL_INPUT_LIMIT');
      reads.set(name, data); return data;
    }
    const manifest = parseJson(read(manifestPath));
    object(manifest, ['schema', 'documents', 'references', 'comparisons', 'claims', 'benchmarks']);
    requireValue(manifest.schema === 1, 'INVALID_SCHEMA');
    array(manifest.documents, 512, 1); manifest.documents.forEach(relative); unique(manifest.documents);
    const actualDocuments = [...names.keys()].filter(name => /\.md$/i.test(name)).toSorted();
    requireValue(JSON.stringify(actualDocuments) === JSON.stringify(manifest.documents.toSorted()), 'DOCUMENT_COVERAGE_MISMATCH');
    const docs = new Map();
    for (const name of manifest.documents) {
      const source = decoder.decode(read(name));
      for (const code of privacyCodes(source)) report.issues.push({ code, file: publicFile(name) });
      docs.set(name, parseMarkdown(source));
    }
    report.markdownFiles = docs.size;
    function target(from, href) {
      currentFile = from;
      requireValue(typeof href === 'string' && href.length <= 8192 && !/[\x00-\x20\x7f\\]/.test(href.replaceAll(' ', '')), 'UNSAFE_LINK');
      if (/^(?:https?:)?\/\//i.test(href) || /^mailto:/i.test(href)) { report.remoteLinksNotFetched++; return; }
      requireValue(!/^[A-Za-z][A-Za-z0-9+.-]*:/.test(href), 'UNSUPPORTED_LINK_SCHEME');
      requireValue(!/%(?:2f|5c|00)/i.test(href), 'ENCODED_SEPARATOR');
      const hash = href.indexOf('#'); const query = href.indexOf('?');
      const end = Math.min(...[hash, query, href.length].filter(n => n >= 0));
      const part = decodeURIComponent(href.slice(0, end));
      const fragment = hash < 0 ? '' : decodeURIComponent(href.slice(hash + 1));
      let name = part === '' ? from : path.posix.normalize(part.startsWith('/') ? part.slice(1) : path.posix.join(path.posix.dirname(from), part));
      name = name.replace(/\/$/, '');
      if (name === '.') name = '';
      requireValue(name !== '..' && !name.startsWith('../'), 'LINK_OUTSIDE_REPOSITORY');
      if (name) relative(name);
      if (!names.has(name)) {
        const prefix = name ? name.replace(/\/$/, '') + '/' : '';
        const readme = ['README.md', 'readme.md', 'Readme.md'].map(file => prefix + file).find(file => docs.has(file));
        if (readme) name = readme;
        else {
          requireValue(!fragment && [...names.keys()].some(file => file.startsWith(prefix)), 'MISSING_LOCAL_TARGET');
          report.localLinks++; return;
        }
      }
      requireValue(['100644', '100755'].includes(names.get(name)), 'LINK_TO_NONREGULAR_FILE');
      // Reading even a path-only target rejects replaced files and symlink ancestors.
      const data = read(name); currentFile = from;
      if (fragment && docs.has(name)) requireValue(docs.get(name).anchors.has(fragment), 'MISSING_LOCAL_ANCHOR');
      else if (fragment) {
        const lines = /^L([1-9]\d*)(?:-L([1-9]\d*))?$/.exec(fragment);
        requireValue(lines, 'UNSUPPORTED_FILE_ANCHOR');
        const count = decoder.decode(data).replace(/\n$/, '').split('\n').length;
        requireValue(Number(lines[1]) <= count && (!lines[2] || Number(lines[2]) >= Number(lines[1]) && Number(lines[2]) <= count), 'LINE_ANCHOR_OUT_OF_RANGE');
      }
      report.localLinks++;
    }
    for (const [name, doc] of docs) for (const href of doc.links) target(name, href);
    const refs = new Map();
    const supplied = new Set(inputs.keys());
    for (const spec of array(manifest.references)) {
      object(spec, ['id', 'kind', 'sha256', 'bytes', 'format'], ['path', 'external', 'anchors', 'pointers']);
      requireValue(isID(spec.id) && !refs.has(spec.id), 'INVALID_OR_DUPLICATE_ID');
      requireValue(['spec', 'vector', 'source', 'result', 'benchmark-manifest', 'benchmark-raw'].includes(spec.kind), 'INVALID_REFERENCE_KIND');
      requireValue(HEX.test(spec.sha256) && Number.isInteger(spec.bytes) && spec.bytes > 0 && spec.bytes <= MAX_FILE, 'INVALID_PIN');
      requireValue(['bytes', 'markdown', 'json'].includes(spec.format), 'INVALID_FORMAT');
      requireValue(Object.hasOwn(spec, 'path') !== Object.hasOwn(spec, 'external'), 'AMBIGUOUS_REFERENCE');
      let bytes;
      if (spec.path) bytes = read(spec.path);
      else {
        object(spec.external, ['repository', 'revision', 'path']); relative(spec.external.path);
        requireValue(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(spec.external.repository) && REVISION.test(spec.external.revision), 'UNPINNED_EXTERNAL_SOURCE');
        requireValue(inputs.has(spec.id), 'EXTERNAL_INPUT_REQUIRED');
        // The caller supplies immutable bytes. The declared repository/ref is not
        // authenticated by hash equality alone, and no network fetch occurs here.
        bytes = readRegular(inputs.get(spec.id)); supplied.delete(spec.id);
        total += bytes.length; requireValue(total <= MAX_TOTAL, 'TOTAL_INPUT_LIMIT');
      }
      currentFile = manifestPath;
      requireValue(bytes.length === spec.bytes && sha(bytes) === spec.sha256, 'REFERENCE_DIGEST_MISMATCH');
      const parsed = { spec, bytes };
      if (spec.format === 'json') parsed.json = parseJson(bytes);
      if (spec.format === 'markdown') parsed.markdown = parseMarkdown(decoder.decode(bytes));
      if (Object.hasOwn(spec, 'anchors')) {
        requireValue(parsed.markdown, 'ANCHORS_REQUIRE_MARKDOWN'); array(spec.anchors, 64, 1); unique(spec.anchors);
        for (const anchor of spec.anchors) requireValue(parsed.markdown.anchors.has(anchor), 'MISSING_SPEC_ANCHOR');
      }
      if (Object.hasOwn(spec, 'pointers')) {
        requireValue(spec.format === 'json', 'POINTERS_REQUIRE_JSON'); array(spec.pointers, 64, 1); unique(spec.pointers);
        for (const selected of spec.pointers) pointer(parsed.json, selected);
      }
      refs.set(spec.id, parsed);
      report.references.push({ id: spec.id, kind: spec.kind, sha256: spec.sha256, bytes: bytes.length,
        origin: spec.external ?? { path: spec.path }, contentVerified: true, sourceAuthorityAuthenticated: false });
    }
    requireValue(supplied.size === 0, 'UNDECLARED_EXTERNAL_INPUT');
    function reference(id, kind) {
      requireValue(isID(id), 'MISSING_OR_WRONG_REFERENCE');
      const value = refs.get(id); requireValue(value && (!kind || value.spec.kind === kind), 'MISSING_OR_WRONG_REFERENCE');
      if (['result', 'benchmark-manifest', 'benchmark-raw'].includes(kind)) requireValue(value.spec.format === 'json', 'EVIDENCE_REQUIRES_JSON');
      return value;
    }
    for (const comparison of array(manifest.comparisons)) {
      object(comparison, ['left', 'right', 'mode']);
      requireValue(comparison.mode === 'bytes' && reference(comparison.left).bytes.equals(reference(comparison.right).bytes), 'REFERENCE_COMPARISON_MISMATCH');
    }
    function location(item) {
      requireValue(isID(item.id) && docs.has(item.document) && docs.get(item.document).anchors.has(item.anchor), 'MISSING_EVIDENCE_LOCATION');
      limits(item.limitations);
    }
    unique(array(manifest.claims).map(claim => claim.id));
    for (const claim of manifest.claims) {
      object(claim, ['id', 'document', 'anchor', 'status', 'sourceRevision', 'inputs', 'limitations'], ['result']); location(claim);
      requireValue(['plan', 'implementation', 'historical', 'unavailable'].includes(claim.status), 'INVALID_CLAIM_STATUS');
      requireValue(REVISION.test(claim.sourceRevision), 'INVALID_REVISION');
      array(claim.inputs); unique(claim.inputs); claim.inputs.forEach(id => reference(id));
      if (claim.status === 'historical') resultRecord(reference(claim.result, 'result').json, claim);
      else requireValue(!Object.hasOwn(claim, 'result'), 'NONRESULT_CLAIM_HAS_RESULT');
      if (claim.status === 'implementation') {
        requireValue(claim.inputs.length > 0, 'IMPLEMENTATION_INPUT_REQUIRED');
        const local = claim.inputs.map(id => reference(id)).filter(ref => ref.spec.path);
        requireValue(local.length > 0 && local.every(ref => ['source', 'spec', 'vector'].includes(ref.spec.kind)), 'IMPLEMENTATION_SOURCE_INPUT_REQUIRED');
        for (const ref of local) requireValue(git(root, ['show', claim.sourceRevision + ':' + ref.spec.path], gitDirectory).equals(ref.bytes), 'IMPLEMENTATION_SOURCE_MISMATCH');
      }
      report.claims.push({ id: claim.id, status: claim.status, sourceRevision: claim.sourceRevision, recordValidated: true, executionPerformed: false });
    }
    unique(array(manifest.benchmarks, 64).map(benchmark => benchmark.id));
    for (const benchmark of manifest.benchmarks) {
      object(benchmark, ['id', 'document', 'anchor', 'status', 'sourceRevision', 'limitations'], ['manifest']); location(benchmark);
      requireValue(['plan', 'unavailable', 'historical'].includes(benchmark.status) && REVISION.test(benchmark.sourceRevision), 'INVALID_BENCHMARK_STATUS');
      if (benchmark.status === 'historical') {
        const selected = reference(benchmark.manifest, 'benchmark-manifest');
        requireValue(selected.spec.path, 'BENCHMARK_MANIFEST_NOT_RETAINED_LOCALLY');
        benchmarkRecord(selected.json, benchmark, reference);
      } else requireValue(!Object.hasOwn(benchmark, 'manifest'), 'UNEXECUTED_BENCHMARK_HAS_RESULTS');
      report.benchmarks.push({ id: benchmark.id, status: benchmark.status, recordValidated: true, executionPerformed: false });
    }
    for (const [name, bytes] of reads) if (name === manifestPath || /\.json$/i.test(name) && manifest.references.some(ref => ref.path === name && ['result', 'benchmark-manifest', 'benchmark-raw'].includes(ref.kind))) {
      for (const code of privacyCodes(decoder.decode(bytes))) report.issues.push({ code, file: publicFile(name) });
    }
    report.selectedInputSha256 = sha(Buffer.from([...reads].toSorted(([a], [b]) => a.localeCompare(b, 'en')).map(([name, bytes]) => name + '\0' + sha(bytes) + '\n').join('')));
    report.status = report.issues.length ? 'failed' : 'passed';
  } catch (error) {
    report.issues.push({ code: error instanceof Refusal ? error.code : 'INPUT_UNAVAILABLE_OR_INVALID', file: publicFile(currentFile) });
  }
  return report;
}
