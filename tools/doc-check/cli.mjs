// Keep diagnostics stable and omit raw input, process errors and absolute paths.
let report;
try {
  const { checkRepository } = await import('./check.mjs');
  const options = { root: process.cwd(), inputs: new Map() };
  const args = process.argv.slice(2);
  for (let i = 0; i < args.length; i += 2) {
    const value = args[i + 1];
    if (!value) throw new Error('argument');
    if (args[i] === '--root' && !options.rootSet) { options.root = value; options.rootSet = true; }
    else if (args[i] === '--manifest' && !options.manifest) options.manifest = value;
    else if (args[i] === '--input') {
      const match = /^([a-z][a-z0-9-]{0,63})=(.+)$/.exec(value);
      if (!match || options.inputs.has(match[1])) throw new Error('argument');
      options.inputs.set(match[1], match[2]);
    } else throw new Error('argument');
  }
  report = checkRepository(options);
} catch {
  report = { schema: 1, status: 'failed', offlineOnly: true, remoteAvailabilityChecked: false,
    networkRequests: 0, issues: [{ code: 'INVALID_ARGUMENT_OR_UNAVAILABLE_TOOL' }] };
}
process.stdout.write(JSON.stringify(report, null, 2) + '\n');
process.exitCode = report.status === 'passed' ? 0 : 1;
