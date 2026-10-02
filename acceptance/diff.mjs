#!/usr/bin/env node
// Reference decoder for the real-cache differential test.
//
//   node acceptance/diff.mjs < payloads
//
// stdin is a sequence of records, each a 4-byte big-endian length followed by that many bytes of
// V8 payload (starting at its 0xFF <version> header, Blink envelope already removed). For every
// record stdout gets one line:
//
//   ok <canonical json>   node:v8 deserialized it; the JSON is scripts/v8vectors/canon.mjs output
//   nc <reason>           not checkable: Node rejected the payload (host objects, unknown
//                         version, ...). Counted by the test, never a failure.
//
// Wire version 16 only widens ArrayBuffer length fields, so, as slacrawl's redux.go does, a
// version 16 payload is relabelled 15 in a copy before Node reads it.
import v8 from 'node:v8';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const { canonical } = await import(pathToFileURL(path.join(here, '..', 'scripts', 'v8vectors', 'canon.mjs')).href);

function check(payload) {
  const buf = Buffer.from(payload); // a copy: the relabel below must not touch the input
  if (buf.length >= 2 && buf[0] === 0xff && buf[1] === 16) buf[1] = 15;
  let value;
  try {
    value = v8.deserialize(buf);
  } catch (e) {
    return `nc ${String(e.message).replace(/\d+/g, 'N').slice(0, 80)}`;
  }
  try {
    return `ok ${canonical(value)}`;
  } catch (e) {
    return `nc canon: ${String(e.message).replace(/\d+/g, 'N').slice(0, 80)}`;
  }
}

let pending = Buffer.alloc(0);
const out = process.stdout;

async function emit(line) {
  if (!out.write(line + '\n')) await new Promise((resolve) => out.once('drain', resolve));
}

for await (const chunk of process.stdin) {
  pending = pending.length === 0 ? chunk : Buffer.concat([pending, chunk]);
  for (;;) {
    if (pending.length < 4) break;
    const n = pending.readUInt32BE(0);
    if (pending.length < 4 + n) break;
    await emit(check(pending.subarray(4, 4 + n)));
    pending = pending.subarray(4 + n);
  }
}
if (pending.length !== 0) {
  console.error('truncated input');
  process.exit(1);
}
