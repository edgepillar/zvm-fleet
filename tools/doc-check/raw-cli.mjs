// Output contains operation status and inventory pins, never raw records or paths.
let response;
try {
  const { captureInventory, verifyInventory, readPrivateJson, writePrivateInventory,
    checkOutputDestination, unknownContext } = await import('./raw-inventory.mjs');
  const { parseInputJson: parseJson, Refusal } = await import('./raw-json.mjs');
  const args = process.argv.slice(2); const command = args.shift(); const flags = new Map();
  const limitFlags = new Map([['--max-files', 'maxFiles'], ['--max-phase-bytes', 'maxPhaseBytes'],
    ['--max-raw-bytes', 'maxRawBytes'], ['--max-total-bytes', 'maxTotalBytes'],
    ['--max-jsonl-line-bytes', 'maxJsonlLineBytes'], ['--max-jsonl-lines', 'maxJsonlLines']]);
  const allowed = new Set(['--root', ...(command === 'capture' ? ['--mode', '--output', '--context'] :
    command === 'verify' ? ['--manifest', '--manifest-sha256'] : []), ...limitFlags.keys()]);
  if (!['capture', 'verify'].includes(command) || args.length % 2) throw new Refusal('INVALID_ARGUMENT');
  for (let i = 0; i < args.length; i += 2) {
    if (!allowed.has(args[i]) || flags.has(args[i]) || !args[i + 1]) throw new Refusal('INVALID_ARGUMENT');
    flags.set(args[i], args[i + 1]);
  }
  const required = ['--root', ...(command === 'capture' ? ['--mode', '--output'] : ['--manifest', '--manifest-sha256'])];
  if (!required.every(flag => flags.has(flag))) throw new Refusal('INVALID_ARGUMENT');
  const limits = {};
  for (const [flag, key] of limitFlags) if (flags.has(flag)) {
    const value = flags.get(flag); if (!/^[1-9][0-9]*$/.test(value)) throw new Refusal('INVALID_LIMIT'); limits[key] = Number(value);
  }
  const controller = new AbortController();
  process.once('SIGINT', () => controller.abort()); process.once('SIGTERM', () => controller.abort());
  const root = flags.get('--root');
  if (command === 'capture') {
    const output = flags.get('--output'); await checkOutputDestination({ output, root });
    const context = flags.has('--context') ? parseJson(await readPrivateJson(flags.get('--context'), 32768)) : unknownContext();
    const value = await captureInventory({ root, mode: flags.get('--mode'), context, limits, signal: controller.signal });
    response = await writePrivateInventory({ value, output, root, signal: controller.signal });
  } else {
    const manifestBytes = await readPrivateJson(flags.get('--manifest'));
    response = await verifyInventory({ root, manifestBytes, expectedSha256: flags.get('--manifest-sha256'), limits, signal: controller.signal });
  }
} catch (error) {
  response = { schema: 1, status: 'refused', code: typeof error?.code === 'string' && /^[A-Z][A-Z0-9_]{0,63}$/.test(error.code)
    ? error.code : 'INPUT_OR_TOOL_UNAVAILABLE', originalOutcomeEstablished: false, performanceQualified: false };
  process.exitCode = 1;
}
process.stdout.write(JSON.stringify(response) + '\n');
