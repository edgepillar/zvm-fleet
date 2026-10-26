import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { tmpdir } from 'node:os';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { captureInventory, verifyInventory, encodeInventory, unknownContext,
  readPrivateJson, writePrivateInventory, checkOutputDestination, DEFAULT_LIMITS } from './raw-inventory.mjs';

const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const aliases = { skip: process.env.ZVM_DOC_CHECK_STORAGE_PROFILE === 'regular-files-only'
  ? 'Owned storage refuses filesystem links; the full hosted profile executes this case.' : false };
const profile = () => ({ L1Pillar: 'fixture-pillar', L1RPC: 'fixture-rpc', EngineCandidates: ['fixture-engine'],
  EngineBCandidates: null, Node: 'fixture-node', NodeB: '', Relayer: 'fixture-relayer', Bridge: 'fixture-bridge', Faucet: 'fixture-faucet' });
function phase(id = '01-transfer', runs = true) {
  const value = { id, title: 'Owned fixture', startedAt: '2026-01-01T00:00:00Z', endedAt: '2026-01-01T00:01:00Z',
    before: {}, after: {}, samples: [], data: {} };
  if (runs) value.runs = [{ file: `runs/${id}-transfer-batch-w1.json`, scenario: 'transfer', lane: 'batch', wallets: 1,
    rate: 1, duration: '1m', prepareMs: 0, senders: 1, fromBlock: 1, toBlock: 2, summary: {} }];
  return value;
}
function fixture(t, mode = 'phases', label = 'baseline') {
  const container = fs.mkdtempSync(path.join(tmpdir(), 'fleet-raw-fixture-'));
  const root = path.join(container, 'inputs'); fs.mkdirSync(root);
  t.after(() => fs.rmSync(container, { recursive: true, force: true }));
  function write(name, value) {
    const filename = path.join(root, name); fs.mkdirSync(path.dirname(filename), { recursive: true });
    fs.writeFileSync(filename, typeof value === 'string' || Buffer.isBuffer(value) ? value : JSON.stringify(value) + '\n');
  }
  write('names.json', profile());
  const id = mode === 'users' ? '00-users-' + label : '01-transfer';
  const selected = phase(id, mode !== 'users'); write(id + '.json', selected);
  const raw = mode === 'phases' ? selected.runs[0].file : null;
  if (raw) write(raw, '{"records":[]}\n');
  if (mode === 'users') {
    write('users.json', { label, mix: [], duration: '1m', ramp: '0s', pace: 1, chainId: 69, l1ChainId: 1 });
    write('records.jsonl', '{"t":1,"user":0,"kind":"read","ok":false}\n');
    write('samples.jsonl', '{"t":1,"containers":{}}\n');
  }
  const capture = extra => captureInventory({ root, mode, ...extra });
  return { container, root, write, phase: selected, id, raw, mode, capture };
}
async function refusal(promise, code) {
  await assert.rejects(promise, error => { assert.equal(error.code, code, error.message); return true; });
}
function bytesOf(value) { const bytes = encodeInventory(value); return { manifestBytes: bytes, expectedSha256: digest(bytes) }; }
function cli(...args) {
  return spawnSync(process.execPath, [fileURLToPath(new URL('./raw-cli.mjs', import.meta.url)), ...args], { encoding: 'utf8', env: process.env });
}

test('phase capture binds streamed bytes and pins without asserting a measurement outcome', async t => {
  const f = fixture(t); const value = await f.capture();
  assert.equal(value.files.length, 3); assert.equal(value.qualification.performanceQualified, false);
  assert.equal(value.qualification.originalOutcomeEstablished, false);
  assert.equal(value.context.run.exitCode, null); assert.equal(value.context.run.completion, 'unknown');
  for (const file of value.files) {
    const raw = fs.readFileSync(path.join(f.root, file.name)); assert.equal(file.sha256, digest(raw)); assert.equal(file.bytes, raw.length);
  }
  assert.equal(value.files.find(file => file.name === f.raw).format, 'opaque-node-result-bytes');
  assert.equal((await verifyInventory({ root: f.root, ...bytesOf(value) })).status, 'verified-selected-byte-inventory');
});
test('capture is deterministic and does not mutate its inputs', async t => {
  const f = fixture(t); const a = await f.capture(), b = await f.capture(); assert.deepEqual(a, b);
  assert.equal(fs.readFileSync(path.join(f.root, f.raw), 'utf8'), '{"records":[]}\n');
});
test('raw files stream across multiple chunks without JSON decoding or a raw copy', async t => {
  const f = fixture(t); const raw = Buffer.alloc(3 * 65536 + 17, 120); f.write(f.raw, raw);
  const result = await f.capture(); const selected = result.files.find(file => file.name === f.raw);
  assert.equal(selected.sha256, digest(raw)); assert.equal(selected.bytes, raw.length);
  assert.equal(result.qualification.rawTransactionPayloadSchemaValidated, false);
  assert.deepEqual(fs.readdirSync(path.join(f.root, 'runs')), [path.basename(f.raw)]);
});
test('an empty transaction population is opaque retained data, not an empty file or a qualified result', async t => {
  const f = fixture(t); const value = await f.capture();
  assert.equal(value.qualification.performanceQualified, false);
  f.write(f.raw, ''); await refusal(f.capture(), 'FILE_BYTES_LIMIT');
});
test('recorded phase errors and missing snapshots remain distinct from capture IO success', async t => {
  const f = fixture(t); f.phase.error = 'Private fixture detail'; f.phase.before = null; f.phase.after = null;
  f.phase.endedAt = '0001-01-01T00:00:00Z'; f.write(f.id + '.json', f.phase);
  const value = await f.capture(); assert.deepEqual(value.phaseObservations[0], { file: f.id + '.json', boundary: 'incomplete-or-unknown',
    errorRecorded: true, beforeSnapshotPresent: false, afterSnapshotPresent: false, referencedRuns: 1 });
  assert.equal(JSON.stringify(value).includes('Private fixture detail'), false); assert.equal(value.qualification.originalOutcomeEstablished, false);
});
test('an ended timestamp cannot become original process completeness', async t => {
  const f = fixture(t); const value = await f.capture(); assert.equal(value.phaseObservations[0].boundary, 'ended-time-recorded');
  assert.equal(value.qualification.originalProcessCompletenessEstablished, false);
});
test('source omitempty phase fields can be absent without inventing successful runs or data', async t => {
  const f = fixture(t); delete f.phase.data; delete f.phase.runs;
  f.phase.samples = null; f.phase.error = 'Owned failed phase';
  f.write(f.id + '.json', f.phase); fs.unlinkSync(path.join(f.root, f.raw));
  const value = await f.capture(); assert.equal(value.files.length, 2);
  assert.equal(value.phaseObservations[0].errorRecorded, true);
  assert.equal(value.phaseObservations[0].referencedRuns, 0);
  assert.equal(value.qualification.originalOutcomeEstablished, false);
  await verifyInventory({ root: f.root, ...bytesOf(value) });
});
test('unknown and unverified source/build/run declarations are preserved with their authority', async t => {
  const f = fixture(t); const context = unknownContext();
  context.source.fleetRevision = '1'.repeat(40); context.build.artifactSha256 = '2'.repeat(64);
  context.build.runtime = 'go1.25.14'; context.build.os = 'linux'; context.build.architecture = 'arm64';
  Object.assign(context.run, { id: 'fixture-run', commandSha256: '3'.repeat(64), attempt: 1, exitCode: 0,
    completion: 'declared-complete', outcome: 'declared-success' });
  const value = await f.capture({ context }); assert.deepEqual(value.context, context);
  assert.equal(value.qualification.originalSourceBuildAuthenticated, false); assert.equal(value.qualification.originalOutcomeEstablished, false);
});
test('contradictory or falsely authenticated declarations fail', async t => {
  const f = fixture(t); const context = unknownContext(); context.authority = 'verified';
  await refusal(f.capture({ context }), 'CONTEXT_AUTHORITY_NOT_SUPPORTED'); context.authority = 'unverified-declaration';
  context.run.outcome = 'declared-success'; await refusal(f.capture({ context }), 'CONFLICTING_RUN_DECLARATION');
});
test('private path and mailbox strings cannot enter typed declaration tokens', async t => {
  const f = fixture(t); const context = unknownContext(); context.build.runtime = '/' + 'Users/example/build';
  await refusal(f.capture({ context }), 'INVALID_TOKEN'); context.build.runtime = 'person' + '@example.invalid';
  await refusal(f.capture({ context }), 'INVALID_TOKEN');
});
test('users mode retains complete JSONL object prefixes and metadata without interpreting transactions', async t => {
  const f = fixture(t, 'users'); const value = await f.capture();
  assert.equal(value.files.length, 5); assert.equal(value.files.find(file => file.name === 'records.jsonl').lineCount, 1);
  assert.equal(value.files.find(file => file.name === 'samples.jsonl').lineCount, 1);
  assert.equal(value.qualification.originalOutcomeEstablished, false); await verifyInventory({ root: f.root, ...bytesOf(value) });
});
test('the private user label can use source-supported ASCII spaces and case', async t => {
  const f = fixture(t, 'users', 'Evening Load'); const value = await f.capture(); assert.equal(value.phaseObservations[0].file, '00-users-Evening Load.json');
});
test('phase mode can retain an optional sample stream', async t => {
  const f = fixture(t); f.write('samples.jsonl', '{"t":1}\n'); assert.equal((await f.capture()).files.length, 4);
});
for (const name of ['names.json', 'users.json', 'records.jsonl', 'samples.jsonl']) {
  test('required user input is unavailable: ' + name, async t => {
    const f = fixture(t, 'users'); fs.unlinkSync(path.join(f.root, name));
    await refusal(f.capture(), name === 'names.json' ? 'PROFILE_INPUT_REQUIRED' : 'USER_INPUT_REQUIRED');
  });
}
test('a missing phase cannot be replaced by profiles or a saved report', async t => {
  const f = fixture(t); fs.unlinkSync(path.join(f.root, f.id + '.json')); f.write('FLEET.md', 'Saved summary');
  await refusal(f.capture(), 'PHASE_INPUT_REQUIRED');
});
test('a missing referenced run fails despite a saved summary', async t => {
  const f = fixture(t); fs.unlinkSync(path.join(f.root, f.raw)); await refusal(f.capture(), 'RAW_REFERENCE_SET_MISMATCH');
});
test('orphan runs are refused without deleting or adopting them', async t => {
  const f = fixture(t); f.write('runs/orphan.json', 'Do not adopt'); await refusal(f.capture(), 'RAW_REFERENCE_SET_MISMATCH');
  assert.equal(fs.readFileSync(path.join(f.root, 'runs/orphan.json'), 'utf8'), 'Do not adopt');
});
test('unrelated root data is not part of the selected snapshot', async t => {
  const f = fixture(t); f.write('notes.txt', 'Unrelated owner data'); f.write('state.json', 'Preserved state');
  const value = await f.capture(); assert.ok(!value.files.some(file => ['notes.txt', 'state.json'].includes(file.name)));
  f.write('notes.txt', 'Changed unrelated data'); await verifyInventory({ root: f.root, ...bytesOf(value) });
});
test('multiple user phases cannot be joined to one overwritten stream', async t => {
  const f = fixture(t, 'users'); f.write('00-users-other.json', phase('00-users-other', false));
  await refusal(f.capture(), 'SELECTION_MODE_MISMATCH');
});
test('mode must be explicit and consistent with the selected source layout', async t => {
  const f = fixture(t, 'users'); await refusal(captureInventory({ root: f.root }), 'MODE_REQUIRED');
  await refusal(captureInventory({ root: f.root, mode: 'phases' }), 'SELECTION_MODE_MISMATCH');
});
test('profile fields are required instead of defaulting to another chain', async t => {
  const f = fixture(t); const p = profile(); delete p.L1RPC; f.write('names.json', p); await refusal(f.capture(), 'INVALID_FIELDS');
});
test('phase identity is bound to its actual file name', async t => {
  const f = fixture(t); f.phase.id = '02-other'; f.write(f.id + '.json', f.phase); await refusal(f.capture(), 'PHASE_ID_MISMATCH');
});
for (const file of ['/outside.json', '../outside.json', 'runs/../outside.json', 'runs/a%2fb.json', 'runs/wrong.json']) {
  test('ambiguous or escaping run selector is refused: ' + file, async t => {
    const f = fixture(t); f.phase.runs[0].file = file; f.write(f.id + '.json', f.phase); await refusal(f.capture(), 'INVALID_RUN_SELECTOR');
  });
}
test('duplicate run references are refused', async t => {
  const f = fixture(t); f.phase.runs.push({ ...f.phase.runs[0] }); f.write(f.id + '.json', f.phase); await refusal(f.capture(), 'DUPLICATE_SELECTOR');
});
test('ambiguous null run lists and unknown phase fields are refused', async t => {
  const f = fixture(t); f.phase.runs = null; f.write(f.id + '.json', f.phase); await refusal(f.capture(), 'LIST_LIMIT');
  delete f.phase.runs; f.phase.unknown = true; f.write(f.id + '.json', f.phase); await refusal(f.capture(), 'INVALID_FIELDS');
});
test('a case alias of a fixed metadata file is refused', async t => {
  const f = fixture(t); fs.renameSync(path.join(f.root, 'names.json'), path.join(f.root, 'Names.json')); await refusal(f.capture(), 'CASE_ALIAS_REFUSED');
});
test('duplicate JSON members cannot silently change a selector', async t => {
  const f = fixture(t); const raw = JSON.stringify(f.phase).replace('"id":"01-transfer"', '"id":"wrong","id":"01-transfer"');
  f.write(f.id + '.json', raw); await refusal(f.capture(), 'DUPLICATE_JSON_KEY');
});
test('JSON depth, malformed JSON and invalid UTF-8 are bounded', async t => {
  const f = fixture(t); f.write(f.id + '.json', '['.repeat(66) + '0' + ']'.repeat(66)); await refusal(f.capture(), 'JSON_DEPTH_LIMIT');
  f.write(f.id + '.json', '{'); await refusal(f.capture(), 'INVALID_JSON');
  f.write(f.id + '.json', Buffer.from([0xc0, 0xaf])); await refusal(f.capture(), 'INVALID_JSON');
});
for (const [label, raw, code] of [
  ['partial final object', '{"t":1', 'INCOMPLETE_JSONL_RECORD'], ['missing final newline', '{"t":1}', 'INCOMPLETE_JSONL_RECORD'],
  ['duplicate member', '{"t":1,"t":2}\n', 'DUPLICATE_JSON_KEY'], ['blank record', '\n', 'EMPTY_JSONL_RECORD'],
  ['array record', '[]\n', 'INVALID_JSONL_RECORD'],
]) test('JSONL framing refuses ' + label, async t => {
  const f = fixture(t, 'users'); f.write('records.jsonl', raw); await refusal(f.capture(), code);
});
test('JSONL framing spans buffer boundaries and counts structural records only', async t => {
  const f = fixture(t, 'users'); const raw = JSON.stringify({ detail: 'x'.repeat(70000) }) + '\n{"t":2}\n';
  f.write('records.jsonl', raw); const value = await f.capture(); assert.equal(value.files.find(file => file.name === 'records.jsonl').lineCount, 2);
  assert.equal(JSON.stringify(value).includes('xxxx'), false);
});
for (const [label, limits, code] of [
  ['file count', { maxFiles: 1 }, 'FILE_COUNT_LIMIT'], ['metadata bytes', { maxPhaseBytes: 10 }, 'FILE_BYTES_LIMIT'],
  ['raw bytes', { maxRawBytes: 5 }, 'FILE_BYTES_LIMIT'], ['total bytes', { maxTotalBytes: 10 }, 'TOTAL_BYTES_LIMIT'],
]) test('capture enforces ' + label, async t => { const f = fixture(t); await refusal(f.capture({ limits }), code); });
test('JSONL line bytes and count are independently bounded', async t => {
  const f = fixture(t, 'users'); await refusal(f.capture({ limits: { maxJsonlLineBytes: 5 } }), 'JSONL_LINE_LIMIT');
  f.write('records.jsonl', '{}\n{}\n'); await refusal(f.capture({ limits: { maxJsonlLines: 1 } }), 'JSONL_COUNT_LIMIT');
});
test('invalid and unknown limits are not silently coerced', async t => {
  const f = fixture(t); await refusal(f.capture({ limits: { maxRawBytes: 0 } }), 'INVALID_LIMIT');
  await refusal(f.capture({ limits: { maxRawBytes: Number.MAX_SAFE_INTEGER } }), 'INVALID_LIMIT');
  await refusal(f.capture({ limits: { surprise: 1 } }), 'INVALID_FIELDS');
});
test('verification requires an independently supplied inventory digest', async t => {
  const f = fixture(t); const value = await f.capture(); const bytes = encodeInventory(value);
  await refusal(verifyInventory({ root: f.root, manifestBytes: bytes }), 'MANIFEST_PIN_REQUIRED');
  await refusal(verifyInventory({ root: f.root, manifestBytes: bytes, expectedSha256: '0'.repeat(64) }), 'MANIFEST_DIGEST_MISMATCH');
});
test('verification rejects each changed payload and each omitted or extra selected input', async t => {
  const f = fixture(t); const original = fs.readFileSync(path.join(f.root, f.raw)); const captured = bytesOf(await f.capture());
  f.write(f.raw, 'changed'); await refusal(verifyInventory({ root: f.root, ...captured }), 'INVENTORY_MISMATCH');
  f.write(f.raw, original); fs.unlinkSync(path.join(f.root, f.raw)); await refusal(verifyInventory({ root: f.root, ...captured }), 'RAW_REFERENCE_SET_MISMATCH');
  f.write(f.raw, original); f.write('runs/extra.json', '{}'); await refusal(verifyInventory({ root: f.root, ...captured }), 'RAW_REFERENCE_SET_MISMATCH');
});
test('a new supported phase is detected by selected-set verification', async t => {
  const f = fixture(t); const captured = bytesOf(await f.capture()); f.write('02-other.json', phase('02-other', false));
  await refusal(verifyInventory({ root: f.root, ...captured }), 'INVENTORY_MISMATCH');
});
test('consumer limits cannot be increased by a producer inventory', async t => {
  const f = fixture(t); const captured = bytesOf(await f.capture());
  await refusal(verifyInventory({ root: f.root, ...captured, limits: { maxRawBytes: 5 } }), 'FILE_BYTES_LIMIT');
});
test('tampered status, provenance and extra inventory fields cannot qualify measurements', async t => {
  const f = fixture(t); const original = await f.capture();
  for (const change of [value => { value.qualification.performanceQualified = true; },
    value => { value.schemaSources.fleet = '1'.repeat(40); }, value => { value.metrics = []; }]) {
    const value = structuredClone(original); change(value); const bytes = Buffer.from(JSON.stringify(value));
    await assert.rejects(verifyInventory({ root: f.root, manifestBytes: bytes, expectedSha256: digest(bytes) }));
  }
});
test('symlinked raw inputs are refused', aliases, async t => {
  const f = fixture(t); const external = path.join(f.container, 'owned-target'); fs.writeFileSync(external, 'Owned fixture');
  fs.unlinkSync(path.join(f.root, f.raw)); fs.symlinkSync(external, path.join(f.root, f.raw)); await refusal(f.capture(), 'SYMLINK_REFUSED');
});
test('symlinked input ancestors are refused', aliases, async t => {
  const f = fixture(t); const directory = path.join(f.container, 'owned-runs'); fs.renameSync(path.join(f.root, 'runs'), directory);
  fs.symlinkSync(directory, path.join(f.root, 'runs')); await refusal(f.capture(), 'SYMLINK_REFUSED');
});
test('hard-linked raw inputs are refused', aliases, async t => {
  const f = fixture(t); fs.linkSync(path.join(f.root, f.raw), path.join(f.container, 'owned-alias')); await refusal(f.capture(), 'NONREGULAR_OR_HARDLINK_INPUT');
});
test('a simulated device mismatch is refused before crossing the selected filesystem', async t => {
  const f = fixture(t); const original = fs.promises.lstat.bind(fs.promises);
  t.mock.method(fs.promises, 'lstat', async (...args) => {
    const info = await original(...args); if (args[0] !== path.join(f.root, 'runs')) return info;
    const changed = Object.create(info); Object.defineProperty(changed, 'dev', { value: info.dev + 1n }); return changed;
  });
  await refusal(f.capture(), 'FILESYSTEM_CROSSING');
});
test('a simulated foreign owner is held rather than adopted', async t => {
  const f = fixture(t); const original = fs.promises.lstat.bind(fs.promises);
  t.mock.method(fs.promises, 'lstat', async (...args) => {
    const info = await original(...args); if (args[0] !== path.join(f.root, 'names.json')) return info;
    const changed = Object.create(info); Object.defineProperty(changed, 'uid', { value: info.uid + 1n }); return changed;
  });
  await refusal(f.capture(), 'FOREIGN_OWNER');
});
test('mutation during the streamed raw read is refused', async t => {
  const f = fixture(t); const filename = path.join(f.root, f.raw); const original = fs.promises.open.bind(fs.promises); let changed = false;
  t.mock.method(fs.promises, 'open', async (...args) => {
    const handle = await original(...args); if (args[0] !== filename) return handle;
    const read = handle.read.bind(handle);
    handle.read = async (...values) => { const result = await read(...values); if (result.bytesRead && !changed) { changed = true; fs.appendFileSync(filename, 'changed'); } return result; };
    return handle;
  });
  await refusal(f.capture(), 'INPUT_CHANGED'); assert.equal(changed, true);
});
test('a same-byte replacement inode is refused', async t => {
  const f = fixture(t); const filename = path.join(f.root, f.raw); const original = fs.promises.open.bind(fs.promises); let replaced = false;
  t.mock.method(fs.promises, 'open', async (...args) => {
    const handle = await original(...args); if (args[0] !== filename) return handle;
    const read = handle.read.bind(handle);
    handle.read = async (...values) => {
      const result = await read(...values);
      if (result.bytesRead && !replaced) { replaced = true; const bytes = fs.readFileSync(filename); fs.renameSync(filename, filename + '.held'); fs.writeFileSync(filename, bytes); }
      return result;
    }; return handle;
  });
  // Renaming the open inode can update its ctime before the path guard runs.
  // Both the open-handle mutation guard and the path identity guard must refuse.
  await assert.rejects(f.capture(), error => ['INPUT_CHANGED', 'INPUT_REPLACED'].includes(error.code));
  assert.equal(replaced, true);
});
test('cancellation before and during IO produces no successful inventory', async t => {
  const f = fixture(t); const already = new AbortController(); already.abort(); await refusal(f.capture({ signal: already.signal }), 'ABORTED');
  const controller = new AbortController(); const timer = setTimeout(() => controller.abort(), 0);
  try { await refusal(f.capture({ signal: controller.signal }), 'ABORTED'); } finally { clearTimeout(timer); }
});
test('private output is explicit, pinned, owner-only and never replaces an existing file', async t => {
  const f = fixture(t); const value = await f.capture(); const output = path.join(f.container, 'inventory.json');
  const summary = await writePrivateInventory({ value, output, root: f.root }); assert.equal(summary.performanceQualified, false);
  assert.equal(fs.statSync(output).mode & 0o777, 0o600); assert.equal(digest(fs.readFileSync(output)), summary.manifestSha256);
  await refusal(writePrivateInventory({ value, output, root: f.root }), 'OUTPUT_EXISTS');
  await verifyInventory({ root: f.root, manifestBytes: await readPrivateJson(output), expectedSha256: summary.manifestSha256 });
});
test('private JSON reads enforce their byte cap and its independent hard ceiling', async t => {
  const f = fixture(t); const filename = path.join(f.root, 'names.json');
  await refusal(readPrivateJson(filename, 1), 'UNSAFE_INPUT_FILE');
  for (const cap of [0, -1, 2 * 1024 ** 2 + 1, Number.MAX_SAFE_INTEGER]) {
    await refusal(readPrivateJson(filename, cap), 'INVALID_LIMIT');
  }
});
test('output cannot collide with input or state namespaces', async t => {
  const f = fixture(t);
  for (const name of ['01-output.json', 'names.json', 'state.json', 'runs/new.json']) {
    await refusal(checkOutputDestination({ root: f.root, output: path.join(f.root, name) }), 'OUTPUT_COLLIDES_WITH_INPUT_SCOPE');
  }
});
test('an inventory can be stored under a non-input name without joining its own selected set', async t => {
  const f = fixture(t); const value = await f.capture(); const output = path.join(f.root, 'raw-inventory.json');
  const summary = await writePrivateInventory({ value, output, root: f.root });
  await verifyInventory({ root: f.root, manifestBytes: await readPrivateJson(output), expectedSha256: summary.manifestSha256 });
});
test('a failed new output preserves its actual partial bytes and all inputs', async t => {
  const f = fixture(t); const value = await f.capture(); const output = path.join(f.container, 'interrupted.json'); const open = fs.promises.open.bind(fs.promises);
  t.mock.method(fs.promises, 'open', async (...args) => {
    const handle = await open(...args); if (args[0] !== output) return handle;
    const write = handle.write.bind(handle); let first = true;
    handle.write = async (buffer, offset, length, position) => { if (!first) throw new Error('Owned write interruption'); first = false; return write(buffer, offset, Math.min(8, length), position); };
    return handle;
  });
  await refusal(writePrivateInventory({ value, output, root: f.root }), 'OUTPUT_WRITE_INCOMPLETE');
  assert.equal(fs.readFileSync(output).length, 8); assert.equal(fs.readFileSync(path.join(f.root, f.raw), 'utf8'), '{"records":[]}\n');
});
test('CLI capture/verify prints only sanitized status and independent pins', async t => {
  const f = fixture(t); const output = path.join(f.container, 'private-inventory.json');
  const capture = cli('capture', '--root', f.root, '--mode', 'phases', '--output', output); assert.equal(capture.status, 0, capture.stdout + capture.stderr);
  const saved = JSON.parse(capture.stdout); assert.equal(capture.stdout.includes(f.root), false); assert.equal(capture.stdout.includes(f.raw), false);
  const verify = cli('verify', '--root', f.root, '--manifest', output, '--manifest-sha256', saved.manifestSha256); assert.equal(verify.status, 0, verify.stdout + verify.stderr);
  assert.equal(JSON.parse(verify.stdout).performanceQualified, false);
});
test('CLI refuses missing pins, duplicate flags and unknown options without raw diagnostics', async t => {
  const f = fixture(t);
  for (const args of [[], ['verify', '--root', f.root, '--manifest', 'missing'],
    ['capture', '--root', f.root, '--root', f.root], ['capture', '--unexpected', 'value']]) {
    const result = cli(...args); assert.equal(result.status, 1); assert.equal(JSON.parse(result.stdout).status, 'refused');
    assert.equal(result.stdout.includes(f.root), false); assert.equal(result.stderr, '');
  }
});
test('default byte limits are explicit and independent from declarations', () => {
  assert.equal(DEFAULT_LIMITS.maxFiles, 256); assert.equal(DEFAULT_LIMITS.maxTotalBytes, 512 * 1024 ** 2);
});
