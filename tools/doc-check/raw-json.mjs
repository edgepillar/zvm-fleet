// The validation function below is retained byte-for-byte from the pinned Core
// checker. This module adds no package dependency and does not change that tool.
import { TextDecoder } from 'node:util';
const decoder = new TextDecoder('utf-8', { fatal: true });
export class Refusal extends Error {
  constructor(code) { super(code); this.code = code; }
}
export function requireValue(condition, code) { if (!condition) throw new Refusal(code); }
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

export { parseJson };
// The retained function decodes before its internal catch. Normalize encoding
// failures at this new module's boundary without changing that retained source.
export function parseInputJson(bytes) {
  try { return parseJson(bytes); }
  catch (error) {
    if (error instanceof Refusal) throw error;
    throw new Refusal('INVALID_JSON');
  }
}
export const JSON_VALIDATOR_SOURCE_SHA256 = "18ac74001cdb20c65c73bbc02f14d56834c41621b1d700b2a613ad61f13a110a";
