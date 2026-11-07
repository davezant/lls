#!/usr/bin/env node
// Dump Node.js built-in modules and JavaScript globals as lls JSON Lines.
//
// Usage: node scripts/introspect/node.js OUT.jsonl
//
// Records:
//   module "fs", "path", ...      every require()-able built-in module
//   module "globals"              global functions/classes (autoloaded by the spec)
//   module "Math", "JSON", ...    namespaces reachable as `Math.max`
//   container "fs#Stats", ...     prototype methods of exported classes
'use strict';

const fs = require('fs');
const { builtinModules } = require('module');

const out = fs.openSync(process.argv[2] || '/dev/stdout', 'w');
const MAX_DOC = 300;
let count = 0;

function write(rec) {
  fs.writeSync(out, JSON.stringify(rec) + '\n');
  count++;
}

// Recover parameter names from the function source when it is JavaScript;
// native functions only expose their arity.
function params(fn) {
  let src;
  try { src = Function.prototype.toString.call(fn); } catch { return null; }
  if (src.includes('[native code]')) {
    return Array.from({ length: fn.length }, (_, i) => 'arg' + i).join(', ');
  }
  const m = src.match(/^[^(]*\(([^)]*)\)/) || src.match(/^\s*(\w+)\s*=>/);
  if (!m) return '';
  return m[1].replace(/\/\*.*?\*\//g, '').replace(/\s+/g, ' ').trim().slice(0, 120);
}

function isClass(fn) {
  try {
    const src = Function.prototype.toString.call(fn);
    if (src.startsWith('class')) return true;
  } catch { /* ignore */ }
  return /^[A-Z]/.test(fn.name || '') && fn.prototype &&
    Object.getOwnPropertyNames(fn.prototype).length > 1;
}

function describe(mod, name, value, container) {
  const m = container ? `${mod}#${container}` : mod;
  const t = typeof value;
  if (t === 'function') {
    if (!container && isClass(value)) {
      write({ m, k: 'class', n: name, d: `class ${name}` });
      let proto;
      try { proto = Object.getOwnPropertyNames(value.prototype || {}); } catch { proto = []; }
      for (const p of proto) {
        if (p === 'constructor' || p.startsWith('_')) continue;
        let v;
        try { v = Object.getOwnPropertyDescriptor(value.prototype, p); } catch { continue; }
        if (v && typeof v.value === 'function') {
          write({ m: `${mod}#${name}`, k: 'method', n: p, d: `${p}(${params(v.value) || ''})` });
        } else if (v) {
          write({ m: `${mod}#${name}`, k: 'property', n: p, d: p });
        }
      }
      return;
    }
    write({ m, k: container ? 'method' : 'function', n: name, d: `function ${name}(${params(value) ?? ''})` });
  } else if (t === 'object' && value !== null) {
    write({ m, k: 'module', n: name, d: `${name}: object` });
  } else {
    const k = /^[A-Z0-9_]+$/.test(name) ? 'constant' : 'variable';
    let repr = t === 'string' ? JSON.stringify(value) : String(value);
    if (repr.length > 60) repr = t;
    write({ m, k, n: name, d: `${name} = ${repr}`.slice(0, 120) });
  }
}

function dumpObject(mod, obj) {
  write({ m: mod });
  let names;
  try { names = Object.getOwnPropertyNames(obj); } catch { return; }
  for (const name of names.sort()) {
    if (name.startsWith('_') || name === 'default') continue;
    let v;
    try { v = obj[name]; } catch { continue; }
    describe(mod, name, v);
  }
}

// Node built-in modules.
for (const name of builtinModules) {
  if (name.startsWith('_') || name.includes('internal')) continue;
  let mod;
  try { mod = require(name); } catch { continue; }
  dumpObject(name, mod);
  // Namespaces such as fs.promises / path.posix become modules too.
  for (const sub of ['promises', 'posix', 'win32']) {
    try {
      if (mod && typeof mod[sub] === 'object' && mod[sub] !== null) dumpObject(`${name}/${sub}`, mod[sub]);
    } catch { /* ignore */ }
  }
}

// JavaScript globals.
write({ m: 'globals' });
const skipGlobals = new Set(['global', 'globalThis', 'process', 'Buffer', 'require', 'module', 'exports']);
for (const name of Object.getOwnPropertyNames(globalThis).sort()) {
  if (name.startsWith('_')) continue;
  let v;
  try { v = globalThis[name]; } catch { continue; }
  if (skipGlobals.has(name)) {
    write({ m: 'globals', k: 'variable', n: name, d: `${name}: ${typeof v}` });
  } else {
    describe('globals', name, v);
  }
  // Namespaces (Math, JSON, Reflect, Intl, console) and static members of
  // classes (Object.keys, Array.from, Promise.all) become modules.
  if ((typeof v === 'object' && v !== null) || typeof v === 'function') {
    if (/^[A-Za-z]/.test(name) && name !== 'globalThis') dumpObject(name, v);
  }
}
for (const name of ['process', 'Buffer', 'console']) dumpObject(name, globalThis[name]);
for (const kw of ['require', 'module', 'exports', '__dirname', '__filename']) {
  write({ m: 'globals', k: 'variable', n: kw, d: kw });
}

fs.closeSync(out);
process.stderr.write(`${count} records\n`);
