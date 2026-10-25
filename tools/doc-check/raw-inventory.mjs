import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { parseInputJson as parseJson, Refusal, requireValue as require } from './raw-json.mjs';

export const SCHEMA_SOURCES = Object.freeze({
  fleet: 'd00b61aa734b4cb8e7103114772c60c1ebcaacb5',
  node: 'ae90b945b0ea95887c31cfb2f19e0b88504186d8',
});
export const DEFAULT_LIMITS = Object.freeze({
  maxFiles: 256, maxPhaseBytes: 8 * 1024 ** 2, maxRawBytes: 256 * 1024 ** 2,
  maxTotalBytes: 512 * 1024 ** 2, maxJsonlLineBytes: 1024 ** 2, maxJsonlLines: 1000000,
});
const HARD_LIMITS = Object.freeze({
  maxFiles: 4096, maxPhaseBytes: 16 * 1024 ** 2, maxRawBytes: 4 * 1024 ** 3,
  maxTotalBytes: 8 * 1024 ** 3, maxJsonlLineBytes: 1024 ** 2, maxJsonlLines: 10000000,
});
const MAX_MANIFEST_BYTES = 2 * 1024 ** 2;
const CHUNK = 64 * 1024;
const HEX = /^[a-f0-9]{64}$/;
const REVISION = /^[a-f0-9]{40}$/;
const TOKEN = /^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,127}$/;
const PART = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const PHASE = /^([0-9]{2}-[A-Za-z0-9][A-Za-z0-9._ -]*)\.json$/;
const PHASE_LIKE = /^[0-9]{2}-.*\.json$/i;
const FIXED_NAMES = ['names.json', 'users.json', 'records.jsonl', 'samples.jsonl', 'runs'];
const QUALIFICATION = Object.freeze({
  claim: 'selected-file-bytes-and-reference-closure-only',
  originalSourceBuildAuthenticated: false, originalCommandAuthenticated: false,
  originalProcessCompletenessEstablished: false, originalOutcomeEstablished: false,
  rawTransactionPayloadSchemaValidated: false, performanceQualified: false,
});
const LIMITATIONS = Object.freeze([
  'This private inventory binds selected existing file bytes and references. It does not copy or protect the raw files against later deletion.',
  'Schema-source pins describe the supported layout. Original source/build/runtime/platform/command/run declarations are unverified or unknown.',
  'Recorded errors, missing snapshots, partial streams and ended timestamps do not establish a successful or complete original process.',
  'Referenced transaction-result payloads are hashed as opaque bytes. Saved summaries and raw values are not used to calculate performance.',
  'Only supported selected files are covered; unrelated files and directories are not adopted, read or cleaned. This is not a hostile-writer sandbox.',
]);
const compare = (a, b) => a < b ? -1 : a > b ? 1 : 0;
const hash = bytes => createHash('sha256').update(bytes).digest('hex');

function record(value, required, optional = []) {
  require(value !== null && typeof value === 'object' && !Array.isArray(value), 'INVALID_RECORD');
  require(required.every(key => Object.hasOwn(value, key)) &&
    Object.keys(value).every(key => [...required, ...optional].includes(key)), 'INVALID_FIELDS');
}
function list(value, max, min = 0) {
  require(Array.isArray(value) && value.length >= min && value.length <= max, 'LIST_LIMIT');
  return value;
}
function token(value, allowEmpty = false) {
  require(typeof value === 'string' && (allowEmpty && value === '' || TOKEN.test(value)), 'INVALID_TOKEN');
}
function unique(values) { require(new Set(values).size === values.length, 'DUPLICATE_SELECTOR'); }
function knownLimits(value = {}) {
  record(value, [], Object.keys(DEFAULT_LIMITS));
  const result = {};
  for (const key of Object.keys(DEFAULT_LIMITS)) {
    const n = Object.hasOwn(value, key) ? value[key] : DEFAULT_LIMITS[key];
    require(Number.isSafeInteger(n) && n > 0 && n <= HARD_LIMITS[key], 'INVALID_LIMIT');
    result[key] = n;
  }
  return result;
}
export function unknownContext() {
  return {
    authority: 'unverified-declaration',
    source: { fleetRevision: null, nodeRevision: null, coreRevision: null },
    build: { artifactSha256: null, dependenciesSha256: null, runtime: null, os: null, architecture: null },
    run: { id: null, commandSha256: null, attempt: null, exitCode: null, completion: 'unknown', outcome: 'unknown' },
  };
}
function contextRecord(value) {
  record(value, ['authority', 'source', 'build', 'run']);
  require(value.authority === 'unverified-declaration', 'CONTEXT_AUTHORITY_NOT_SUPPORTED');
  record(value.source, ['fleetRevision', 'nodeRevision', 'coreRevision']);
  record(value.build, ['artifactSha256', 'dependenciesSha256', 'runtime', 'os', 'architecture']);
  record(value.run, ['id', 'commandSha256', 'attempt', 'exitCode', 'completion', 'outcome']);
  for (const entry of Object.values(value.source)) require(entry === null || typeof entry === 'string' && REVISION.test(entry), 'INVALID_SOURCE_DECLARATION');
  for (const entry of [value.build.artifactSha256, value.build.dependenciesSha256, value.run.commandSha256]) {
    require(entry === null || typeof entry === 'string' && HEX.test(entry), 'INVALID_DIGEST_DECLARATION');
  }
  for (const entry of [value.build.runtime, value.run.id]) if (entry !== null) token(entry);
  require([null, 'linux', 'darwin', 'windows', 'freebsd'].includes(value.build.os), 'INVALID_PLATFORM_DECLARATION');
  require([null, 'x64', 'arm64', 'arm', 'ia32', 'ppc64', 's390x', 'riscv64'].includes(value.build.architecture), 'INVALID_PLATFORM_DECLARATION');
  require(value.run.attempt === null || Number.isSafeInteger(value.run.attempt) && value.run.attempt > 0 && value.run.attempt <= 65535, 'INVALID_ATTEMPT_DECLARATION');
  require(value.run.exitCode === null || Number.isSafeInteger(value.run.exitCode) && value.run.exitCode >= 0 && value.run.exitCode <= 255, 'INVALID_EXIT_DECLARATION');
  require(['unknown', 'declared-complete', 'declared-incomplete'].includes(value.run.completion), 'INVALID_COMPLETION_DECLARATION');
  require(['unknown', 'declared-success', 'declared-failure'].includes(value.run.outcome), 'INVALID_OUTCOME_DECLARATION');
  if (value.run.outcome === 'declared-success') require(value.run.exitCode === 0 && value.run.completion === 'declared-complete', 'CONFLICTING_RUN_DECLARATION');
  return {
    authority: value.authority,
    source: Object.fromEntries(['fleetRevision', 'nodeRevision', 'coreRevision'].map(key => [key, value.source[key]])),
    build: Object.fromEntries(['artifactSha256', 'dependenciesSha256', 'runtime', 'os', 'architecture'].map(key => [key, value.build[key]])),
    run: Object.fromEntries(['id', 'commandSha256', 'attempt', 'exitCode', 'completion', 'outcome'].map(key => [key, value.run[key]])),
  };
}
function cancelled(signal) { require(!signal?.aborted, 'ABORTED'); }
function identity(info) {
  return [info.dev, info.ino, info.mode, info.uid, info.gid, info.nlink, info.size, info.mtimeNs, info.ctimeNs].map(String).join(':');
}
function owner(info) {
  if (typeof process.getuid === 'function') require(info.uid === BigInt(process.getuid()), 'FOREIGN_OWNER');
}
async function ancestors(filename) {
  for (let current = path.resolve(filename);;) {
    const info = await fs.promises.lstat(current, { bigint: true });
    require(!info.isSymbolicLink(), 'SYMLINK_REFUSED');
    const next = path.dirname(current); if (next === current) break; current = next;
  }
}
function normalize(error) { return error instanceof Refusal ? error : new Refusal('INPUT_UNAVAILABLE'); }
async function entries(directory, maximum, signal) {
  const result = [];
  const stream = await fs.promises.opendir(directory);
  for await (const entry of stream) {
    cancelled(signal); require(result.length < maximum, 'DIRECTORY_LIMIT'); result.push(entry.name);
  }
  return result.sort(compare);
}
function phaseName(name) {
  require(typeof name === 'string' && name.length <= 160 && PHASE.test(name), 'UNSUPPORTED_PHASE_NAME');
  return name.slice(0, -5);
}
function selector(value) {
  require(typeof value === 'string' && value.length <= 256 && /^[A-Za-z0-9][A-Za-z0-9._ /-]*$/.test(value) &&
    value.split('/').every(part => part && part !== '.' && part !== '..'), 'INVALID_SELECTOR');
}
function rootSelection(names, mode) {
  for (const name of names) {
    if (FIXED_NAMES.includes(name.toLowerCase())) require(FIXED_NAMES.includes(name), 'CASE_ALIAS_REFUSED');
    if (PHASE_LIKE.test(name)) phaseName(name);
  }
  const phases = names.filter(name => PHASE.test(name));
  require(phases.length > 0, 'PHASE_INPUT_REQUIRED');
  const users = phases.filter(name => /^[0-9]{2}-users-/.test(name));
  unique(phases.map(name => name.toLowerCase()));
  require(mode === 'users' ? users.length === 1 && phases.length === 1 : users.length === 0, 'SELECTION_MODE_MISMATCH');
  require(names.includes('names.json'), 'PROFILE_INPUT_REQUIRED');
  if (mode === 'users') require(['users.json', 'records.jsonl', 'samples.jsonl'].every(name => names.includes(name)), 'USER_INPUT_REQUIRED');
  else require(!names.includes('users.json') && !names.includes('records.jsonl'), 'SELECTION_MODE_MISMATCH');
  return [...phases, 'names.json', ...(mode === 'users' ? ['users.json', 'records.jsonl'] : []),
    ...(names.includes('samples.jsonl') ? ['samples.jsonl'] : [])].sort(compare);
}
function validateProfile(value) {
  record(value, ['L1Pillar', 'L1RPC', 'EngineCandidates', 'EngineBCandidates', 'Node', 'NodeB', 'Relayer', 'Bridge', 'Faucet']);
  for (const key of ['L1Pillar', 'L1RPC', 'Node', 'Relayer', 'Bridge', 'Faucet']) token(value[key]);
  token(value.NodeB, true);
  list(value.EngineCandidates, 16, 1).forEach(entry => token(entry)); unique(value.EngineCandidates);
  if (value.EngineBCandidates !== null) { list(value.EngineBCandidates, 16).forEach(entry => token(entry)); unique(value.EngineBCandidates); }
}
function boundary(start, end) {
  const form = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/;
  require(typeof start === 'string' && typeof end === 'string' && form.test(start) && form.test(end), 'INVALID_PHASE_TIME');
  const a = Date.parse(start), b = Date.parse(end);
  require(Number.isFinite(a) && Number.isFinite(b), 'INVALID_PHASE_TIME');
  return start.startsWith('0001-') || end.startsWith('0001-') || b < a ? 'incomplete-or-unknown' : 'ended-time-recorded';
}
function inspectPhase(value, name) {
  record(value, ['id', 'title', 'startedAt', 'endedAt', 'before', 'after', 'samples'], ['data', 'runs', 'error']);
  require(value.id === phaseName(name), 'PHASE_ID_MISMATCH');
  require(typeof value.title === 'string' && value.title.length <= 2048, 'INVALID_PHASE_TITLE');
  for (const selected of [value.before, value.after, ...(Object.hasOwn(value, 'data') ? [value.data] : [])]) {
    require(selected === null || typeof selected === 'object' && !Array.isArray(selected), 'INVALID_PHASE_OBJECT');
  }
  require(value.samples === null || Array.isArray(value.samples), 'INVALID_PHASE_SAMPLES');
  require(!Object.hasOwn(value, 'error') || typeof value.error === 'string' && value.error.length <= 65536, 'INVALID_PHASE_ERROR');
  const runs = [];
  for (const ref of list(Object.hasOwn(value, 'runs') ? value.runs : [], 256)) {
    record(ref, ['file', 'scenario', 'lane', 'wallets', 'rate', 'duration', 'prepareMs', 'senders', 'fromBlock', 'toBlock', 'summary'], ['errors']);
    require(typeof ref.scenario === 'string' && ref.scenario.length <= 64 && PART.test(ref.scenario), 'INVALID_RUN_SELECTOR');
    require(['batch', 'native'].includes(ref.lane) && Number.isSafeInteger(ref.wallets) && ref.wallets > 0 && ref.wallets <= 100000000, 'INVALID_RUN_SELECTOR');
    const expected = `runs/${value.id}-${ref.scenario}-${ref.lane}-w${ref.wallets}.json`;
    require(ref.file === expected && expected.length <= 256, 'INVALID_RUN_SELECTOR');
    runs.push(expected);
  }
  unique(runs);
  return { runs, observation: { file: name, boundary: boundary(value.startedAt, value.endedAt),
    errorRecorded: Boolean(value.error), beforeSnapshotPresent: value.before !== null,
    afterSnapshotPresent: value.after !== null, referencedRuns: runs.length } };
}

class Reader {
  constructor(root, limits, signal) { Object.assign(this, { root, limits, signal }); this.files = []; this.guards = new Map(); this.directories = new Map(); this.total = 0; }
  async initialize() {
    cancelled(this.signal); require(typeof fs.constants.O_NOFOLLOW === 'number', 'FILESYSTEM_GUARD_UNSUPPORTED');
    await ancestors(this.root); const info = await fs.promises.lstat(this.root, { bigint: true });
    require(info.isDirectory(), 'INVALID_ROOT'); owner(info); this.device = info.dev;
    this.directories.set('', identity(info));
  }
  async checked(relative, directory = false) {
    selector(relative);
    let current = this.root;
    const parts = relative.split('/');
    for (let i = 0; i < parts.length; i++) {
      current = path.join(current, parts[i]);
      const info = await fs.promises.lstat(current, { bigint: true });
      require(!info.isSymbolicLink(), 'SYMLINK_REFUSED'); require(info.dev === this.device, 'FILESYSTEM_CROSSING'); owner(info);
      if (i !== parts.length - 1 || directory) require(info.isDirectory(), 'NONREGULAR_INPUT');
      else require(info.isFile() && info.nlink === 1n, 'NONREGULAR_OR_HARDLINK_INPUT');
      if (i === parts.length - 1) return { filename: current, info };
    }
  }
  async directory(relative) {
    if (!relative) return entries(this.root, Math.max(256, this.limits.maxFiles * 4), this.signal);
    const { filename, info } = await this.checked(relative, true);
    const pin = identity(info);
    if (this.directories.has(relative)) require(this.directories.get(relative) === pin, 'DIRECTORY_CHANGED');
    else this.directories.set(relative, pin);
    return entries(filename, Math.max(256, this.limits.maxFiles * 4), this.signal);
  }
  async read(relative, role, format) {
    cancelled(this.signal); require(this.files.length < this.limits.maxFiles, 'FILE_COUNT_LIMIT');
    require(!this.guards.has(relative), 'DUPLICATE_SELECTOR');
    const { filename, info } = await this.checked(relative);
    const max = format === 'metadata-json' ? this.limits.maxPhaseBytes : this.limits.maxRawBytes;
    require(info.size > 0n && info.size <= BigInt(max), 'FILE_BYTES_LIMIT');
    require(BigInt(this.total) + info.size <= BigInt(this.limits.maxTotalBytes), 'TOTAL_BYTES_LIMIT');
    const initial = identity(info); const digest = createHash('sha256'); const chunks = [];
    const handle = await fs.promises.open(filename, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
    let count = 0, lineCount = 0, lineBytes = 0, pending = [];
    function line(raw) {
      require(raw.length > 0, 'EMPTY_JSONL_RECORD');
      const value = parseJson(raw); require(value !== null && typeof value === 'object' && !Array.isArray(value), 'INVALID_JSONL_RECORD');
      require(++lineCount <= this.limits.maxJsonlLines, 'JSONL_COUNT_LIMIT');
    }
    try {
      require(identity(await handle.stat({ bigint: true })) === initial, 'INPUT_REPLACED');
      const buffer = Buffer.alloc(CHUNK);
      for (;;) {
        cancelled(this.signal);
        const { bytesRead } = await handle.read(buffer, 0, buffer.length, null);
        if (!bytesRead) break;
        count += bytesRead; require(count <= max && this.total + count <= this.limits.maxTotalBytes, 'STREAM_BYTES_LIMIT');
        digest.update(buffer.subarray(0, bytesRead));
        if (format === 'metadata-json') chunks.push(Buffer.from(buffer.subarray(0, bytesRead)));
        if (format === 'jsonl-objects') {
          let start = 0;
          for (let i = 0; i < bytesRead; i++) if (buffer[i] === 10) {
            const piece = Buffer.from(buffer.subarray(start, i)); lineBytes += piece.length;
            require(lineBytes <= this.limits.maxJsonlLineBytes, 'JSONL_LINE_LIMIT');
            pending.push(piece); line.call(this, Buffer.concat(pending, lineBytes)); pending = []; lineBytes = 0; start = i + 1;
          }
          if (start < bytesRead) { const piece = Buffer.from(buffer.subarray(start, bytesRead)); pending.push(piece); lineBytes += piece.length; require(lineBytes <= this.limits.maxJsonlLineBytes, 'JSONL_LINE_LIMIT'); }
        }
      }
      require(count === Number(info.size) && identity(await handle.stat({ bigint: true })) === initial, 'INPUT_CHANGED');
    } finally { await handle.close(); }
    cancelled(this.signal);
    require(identity((await this.checked(relative)).info) === initial, 'INPUT_REPLACED');
    if (format === 'jsonl-objects') require(lineBytes === 0 && pending.length === 0 && lineCount > 0, 'INCOMPLETE_JSONL_RECORD');
    const item = { role, name: relative, bytes: count, sha256: digest.digest('hex'), format };
    if (format === 'jsonl-objects') item.lineCount = lineCount;
    this.total += count; this.guards.set(relative, initial); this.files.push(item);
    return format === 'metadata-json' ? parseJson(Buffer.concat(chunks, count)) : undefined;
  }
  async final() {
    cancelled(this.signal);
    for (const [name, pin] of this.guards) require(identity((await this.checked(name)).info) === pin, 'INPUT_CHANGED');
    for (const [name, pin] of this.directories) {
      const info = name ? (await this.checked(name, true)).info : await fs.promises.lstat(this.root, { bigint: true });
      require(!info.isSymbolicLink() && identity(info) === pin, 'DIRECTORY_CHANGED');
    }
  }
}

export async function captureInventory({ root, mode, context = unknownContext(), limits = {}, signal }) {
  try {
    require(typeof root === 'string' && root.length > 0, 'ROOT_REQUIRED');
    require(['phases', 'users'].includes(mode), 'MODE_REQUIRED');
    require(signal === undefined || signal instanceof AbortSignal, 'INVALID_SIGNAL');
    const budget = knownLimits(limits), declaration = contextRecord(context);
    const reader = new Reader(path.resolve(root), budget, signal); await reader.initialize();
    const names = await reader.directory(''); const selected = rootSelection(names, mode);
    const runNames = names.includes('runs') ? await reader.directory('runs') : [];
    const profile = await reader.read('names.json', 'profile', 'metadata-json'); validateProfile(profile);
    const observations = [], references = [];
    for (const name of selected.filter(name => PHASE.test(name))) {
      const value = await reader.read(name, 'phase', 'metadata-json');
      const inspected = inspectPhase(value, name);
      require(references.length + inspected.runs.length <= budget.maxFiles, 'REFERENCE_COUNT_LIMIT');
      observations.push(inspected.observation); references.push(...inspected.runs);
    }
    unique(references); references.sort(compare);
    require(references.length === runNames.length && references.every((name, index) => name === 'runs/' + runNames[index]), 'RAW_REFERENCE_SET_MISMATCH');
    for (const name of references) await reader.read(name, 'referenced-run', 'opaque-node-result-bytes');
    if (mode === 'users') {
      const users = await reader.read('users.json', 'users-metadata', 'metadata-json');
      record(users, ['label', 'mix', 'duration', 'ramp', 'pace', 'chainId', 'l1ChainId']);
      require(typeof users.label === 'string' && /^[A-Za-z0-9][A-Za-z0-9._ -]*$/.test(users.label) && users.label.length <= 128 &&
        observations[0].file === '00-users-' + users.label + '.json', 'USER_LABEL_MISMATCH');
      await reader.read('records.jsonl', 'user-record-stream', 'jsonl-objects');
    }
    if (selected.includes('samples.jsonl')) await reader.read('samples.jsonl', 'sample-stream', 'jsonl-objects');
    const finalNames = await reader.directory('');
    require(isDeepStrictEqual(rootSelection(finalNames, mode), selected), 'SELECTED_SET_CHANGED');
    const finalRuns = finalNames.includes('runs') ? await reader.directory('runs') : [];
    require(isDeepStrictEqual(finalRuns, runNames), 'SELECTED_SET_CHANGED');
    await reader.final();
    return {
      schema: 1, kind: 'zvm-fleet-raw-input-inventory', visibility: 'private-input-inventory',
      schemaSources: { ...SCHEMA_SOURCES }, selection: { mode, scope: 'existing-supported-files-only' },
      context: declaration, captureLimits: budget,
      files: reader.files.sort((a, b) => compare(a.name, b.name)), phaseObservations: observations,
      totalBytes: reader.total, qualification: { ...QUALIFICATION }, limitations: [...LIMITATIONS],
    };
  } catch (error) { throw normalize(error); }
}

function validateInventory(value) {
  record(value, ['schema', 'kind', 'visibility', 'schemaSources', 'selection', 'context', 'captureLimits', 'files', 'phaseObservations', 'totalBytes', 'qualification', 'limitations']);
  require(value.schema === 1 && value.kind === 'zvm-fleet-raw-input-inventory' && value.visibility === 'private-input-inventory', 'UNSUPPORTED_INVENTORY');
  require(isDeepStrictEqual(value.schemaSources, SCHEMA_SOURCES) && isDeepStrictEqual(value.qualification, QUALIFICATION) &&
    isDeepStrictEqual(value.limitations, LIMITATIONS), 'UNSUPPORTED_INVENTORY');
  record(value.selection, ['mode', 'scope']); require(['phases', 'users'].includes(value.selection.mode) && value.selection.scope === 'existing-supported-files-only', 'UNSUPPORTED_SELECTION');
  contextRecord(value.context); record(value.captureLimits, Object.keys(DEFAULT_LIMITS)); knownLimits(value.captureLimits);
  require(Number.isSafeInteger(value.totalBytes) && value.totalBytes > 0 && value.totalBytes <= HARD_LIMITS.maxTotalBytes, 'INVALID_TOTAL');
  list(value.files, HARD_LIMITS.maxFiles, 2); unique(value.files.map(file => file.name));
  let total = 0;
  for (const file of value.files) {
    record(file, ['role', 'name', 'bytes', 'sha256', 'format'], ['lineCount']);
    selector(file.name);
    require(['profile', 'phase', 'referenced-run', 'users-metadata', 'user-record-stream', 'sample-stream'].includes(file.role), 'INVALID_ROLE');
    require(['metadata-json', 'opaque-node-result-bytes', 'jsonl-objects'].includes(file.format), 'INVALID_FORMAT');
    require(Number.isSafeInteger(file.bytes) && file.bytes > 0 && file.bytes <= HARD_LIMITS.maxRawBytes && typeof file.sha256 === 'string' && HEX.test(file.sha256), 'INVALID_FILE_PIN');
    if (file.format === 'jsonl-objects') require(Number.isSafeInteger(file.lineCount) && file.lineCount > 0 && file.lineCount <= HARD_LIMITS.maxJsonlLines, 'INVALID_LINE_COUNT');
    else require(!Object.hasOwn(file, 'lineCount'), 'UNEXPECTED_LINE_COUNT');
    total += file.bytes;
  }
  require(total === value.totalBytes, 'TOTAL_MISMATCH');
  list(value.phaseObservations, HARD_LIMITS.maxFiles, 1);
  for (const phase of value.phaseObservations) {
    record(phase, ['file', 'boundary', 'errorRecorded', 'beforeSnapshotPresent', 'afterSnapshotPresent', 'referencedRuns']); phaseName(phase.file);
    require(['incomplete-or-unknown', 'ended-time-recorded'].includes(phase.boundary) &&
      [phase.errorRecorded, phase.beforeSnapshotPresent, phase.afterSnapshotPresent].every(value => typeof value === 'boolean') &&
      Number.isSafeInteger(phase.referencedRuns) && phase.referencedRuns >= 0 && phase.referencedRuns <= 256, 'INVALID_PHASE_OBSERVATION');
  }
}
export function encodeInventory(value) {
  validateInventory(value); const bytes = Buffer.from(JSON.stringify(value, null, 2) + '\n');
  require(bytes.length <= MAX_MANIFEST_BYTES, 'MANIFEST_BYTES_LIMIT'); return bytes;
}
export async function verifyInventory({ root, manifestBytes, expectedSha256, limits = {}, signal }) {
  try {
    require(typeof expectedSha256 === 'string' && HEX.test(expectedSha256), 'MANIFEST_PIN_REQUIRED');
    require(manifestBytes instanceof Uint8Array && manifestBytes.length > 0 && manifestBytes.length <= MAX_MANIFEST_BYTES, 'MANIFEST_BYTES_LIMIT');
    require(hash(manifestBytes) === expectedSha256, 'MANIFEST_DIGEST_MISMATCH');
    const original = parseJson(manifestBytes); validateInventory(original);
    // Consumer limits are independent. A producer cannot increase them through
    // the pinned inventory's captureLimits field.
    const current = await captureInventory({ root, mode: original.selection.mode, context: original.context, limits, signal });
    current.captureLimits = original.captureLimits;
    require(isDeepStrictEqual(current, original), 'INVENTORY_MISMATCH');
    return { schema: 1, status: 'verified-selected-byte-inventory', manifestSha256: expectedSha256,
      files: current.files.length, totalBytes: current.totalBytes, originalOutcomeEstablished: false, performanceQualified: false };
  } catch (error) { throw normalize(error); }
}
export async function readPrivateJson(filename, cap = MAX_MANIFEST_BYTES) {
  try {
    require(Number.isSafeInteger(cap) && cap > 0 && cap <= MAX_MANIFEST_BYTES, 'INVALID_LIMIT');
    require(typeof filename === 'string' && filename.length > 0, 'INPUT_FILE_REQUIRED'); await ancestors(filename);
    const before = await fs.promises.lstat(filename, { bigint: true }); owner(before);
    require(before.isFile() && before.nlink === 1n && before.size > 0n && before.size <= BigInt(cap), 'UNSAFE_INPUT_FILE');
    require(typeof fs.constants.O_NOFOLLOW === 'number', 'FILESYSTEM_GUARD_UNSUPPORTED');
    const handle = await fs.promises.open(filename, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
    let data;
    try {
      require(identity(await handle.stat({ bigint: true })) === identity(before), 'INPUT_REPLACED');
      data = Buffer.alloc(Number(before.size)); let offset = 0;
      while (offset < data.length) { const { bytesRead } = await handle.read(data, offset, data.length - offset, null); require(bytesRead > 0, 'INPUT_CHANGED'); offset += bytesRead; }
      const extra = Buffer.alloc(1); require((await handle.read(extra, 0, 1, null)).bytesRead === 0 && identity(await handle.stat({ bigint: true })) === identity(before), 'INPUT_CHANGED');
    } finally { await handle.close(); }
    require(identity(await fs.promises.lstat(filename, { bigint: true })) === identity(before), 'INPUT_REPLACED');
    return data;
  } catch (error) { throw normalize(error); }
}
export async function checkOutputDestination({ output, root }) {
  try {
    require(typeof output === 'string' && output.length > 0, 'OUTPUT_REQUIRED');
    const filename = path.resolve(output), parent = path.dirname(filename);
    await ancestors(parent); const directory = await fs.promises.lstat(parent, { bigint: true }); require(directory.isDirectory(), 'INVALID_OUTPUT_PARENT'); owner(directory);
    if (root) {
      const relative = path.relative(path.resolve(root), filename);
      if (relative !== '..' && !relative.startsWith('..' + path.sep) && !path.isAbsolute(relative)) {
        require(relative && !relative.includes(path.sep) && !PHASE_LIKE.test(relative) && !FIXED_NAMES.includes(relative.toLowerCase()) && !['state.json', '.env', 'credentials', 'wallet.json'].includes(relative.toLowerCase()), 'OUTPUT_COLLIDES_WITH_INPUT_SCOPE');
      }
    }
    try { await fs.promises.lstat(filename); throw new Refusal('OUTPUT_EXISTS'); }
    catch (error) { if (error instanceof Refusal) throw error; require(error.code === 'ENOENT', 'OUTPUT_UNAVAILABLE'); }
    return { filename, parent, directory };
  } catch (error) { throw error instanceof Refusal ? error : new Refusal('OUTPUT_UNAVAILABLE'); }
}
export async function writePrivateInventory({ value, output, root, signal }) {
  let handle;
  try {
    cancelled(signal);
    const { filename, parent, directory } = await checkOutputDestination({ output, root });
    const bytes = encodeInventory(value);
    require(typeof fs.constants.O_NOFOLLOW === 'number', 'FILESYSTEM_GUARD_UNSUPPORTED');
    try { handle = await fs.promises.open(filename, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW, 0o600); }
    catch (error) { if (error.code === 'EEXIST') throw new Refusal('OUTPUT_EXISTS'); throw error; }
    const info = await handle.stat({ bigint: true }); owner(info); require(info.isFile() && info.nlink === 1n && info.mode % 512n === 384n, 'OUTPUT_IDENTITY_OR_MODE');
    let offset = 0;
    while (offset < bytes.length) { cancelled(signal); const { bytesWritten } = await handle.write(bytes, offset, bytes.length - offset, null); require(bytesWritten > 0, 'OUTPUT_WRITE_INCOMPLETE'); offset += bytesWritten; }
    await handle.sync(); cancelled(signal);
    const after = await handle.stat({ bigint: true });
    require(after.ino === info.ino && after.dev === info.dev && after.mode === info.mode && after.uid === info.uid && after.gid === info.gid && after.nlink === 1n && after.size === BigInt(bytes.length), 'OUTPUT_CHANGED');
    require(identity(await fs.promises.lstat(filename, { bigint: true })) === identity(after), 'OUTPUT_REPLACED');
    const parentHandle = await fs.promises.open(parent, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
    try {
      const current = await parentHandle.stat({ bigint: true }); require(current.isDirectory() && current.dev === directory.dev && current.ino === directory.ino, 'OUTPUT_PARENT_REPLACED');
      await parentHandle.sync();
    } finally { await parentHandle.close(); }
    cancelled(signal);
    return { schema: 1, status: 'captured-private-byte-inventory', manifestSha256: hash(bytes), files: value.files.length,
      totalBytes: value.totalBytes, originalOutcomeEstablished: false, performanceQualified: false };
  } catch (error) { throw error instanceof Refusal ? error : new Refusal(handle ? 'OUTPUT_WRITE_INCOMPLETE' : 'OUTPUT_UNAVAILABLE'); }
  finally { if (handle) await handle.close(); }
}
