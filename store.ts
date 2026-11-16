// The forge's store: git objects, packs, trees, refs and the GitHub resources
// (repositories, issues, pull requests) that all four surfaces share.
//
// Everything here is in-memory and deterministic: server-built commits use a
// fixed identity and timestamp, so two runs that take the same requests build
// the same object ids. No wall clock, no randomness, no git binary, no network.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const STRANGER_LOGIN = 'olgabauer';
// The contest's scenario is built from wire.ts's fixed SCENARIO_SEED
// (0x6a09e667f3bcc909); buildScenario().viewer is always this user, in every
// practice and final trial, so the viewer identity is a constant here.
export const FIXED_EPOCH_SEC = 1735689600; // 2025-01-01T00:00:00Z
export const FIXED_ISO = '2025-01-01T00:00:00Z';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
}

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([head, data]));
}

export function isOid(value: string): boolean {
  return /^[0-9a-f]{40}$/.test(value);
}

// ---------------------------------------------------------------------------------------------
// object parsing and building
// ---------------------------------------------------------------------------------------------

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    if (sp < 0) throw new Error('bad tree entry');
    const nul = data.indexOf(0x00, sp + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('bad tree entry');
    entries.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return entries;
}

function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headEnd = blank < 0 ? data.length : blank;
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of data.subarray(0, headEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const sp = line.indexOf(' ');
    const key = sp < 0 ? line : line.slice(0, sp);
    const value = sp < 0 ? '' : line.slice(sp + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(opts: {
  tree: string;
  parents: readonly string[];
  message: string;
  author?: string;
  committer?: string;
  timestamp?: number;
}): Buffer {
  const ts = opts.timestamp ?? FIXED_EPOCH_SEC;
  const who = `${opts.author ?? `${VIEWER_NAME} <${VIEWER_EMAIL}>`} ${ts} +0000`;
  const committer = `${opts.committer ?? `${VIEWER_NAME} <${VIEWER_EMAIL}>`} ${ts} +0000`;
  const lines = [`tree ${opts.tree}`];
  for (const p of opts.parents) lines.push(`parent ${p}`);
  lines.push(`author ${who}`, `committer ${committer}`, '');
  const msg = opts.message.endsWith('\n') ? opts.message : `${opts.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n${msg}`, 'utf8');
}

export function parseTag(data: Buffer): { object: string; type: ObjType; tag: string; tagger: string; message: string } {
  const blank = data.indexOf('\n\n');
  const headEnd = blank < 0 ? data.length : blank;
  const headers = new Map<string, string>();
  for (const line of data.subarray(0, headEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const sp = line.indexOf(' ');
    if (sp < 0) continue;
    headers.set(line.slice(0, sp), line.slice(sp + 1));
  }
  const type = headers.get('type') ?? '';
  return {
    object: headers.get('object') ?? '',
    type: (type === 'commit' || type === 'tree' || type === 'blob' || type === 'tag' ? type : 'commit') as ObjType,
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

// ---------------------------------------------------------------------------------------------
// reachability, ancestry, trees
// ---------------------------------------------------------------------------------------------

export type ObjectStore = Map<string, GitObject>;

export function collectReachable(store: ObjectStore, roots: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const stack: string[] = [];
  for (const r of roots) if (isOid(r)) stack.push(r);
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = store.get(oid);
    if (obj === undefined) throw new Error(`object ${oid} is missing from the store`);
    seen.add(oid);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree);
      for (const p of c.parents) stack.push(p);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) if (e.mode !== '160000') stack.push(e.oid);
    }
  }
  return seen;
}

/** True when `ancestor` is reachable from `tip` by following parents (or equal). */
export function isAncestor(store: ObjectStore, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = store.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return false;
}

/** The first-parent chain (plus merge parents, breadth-first) from a tip, newest first. */
export function listCommits(store: ObjectStore, tip: string, limit = 100): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length > 0 && out.length < limit) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = store.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    out.push(oid);
    for (const p of parseCommit(obj.data).parents) queue.push(p);
  }
  return out;
}

/** Walk a tree to `path`; returns the blob entry or null. */
export function readFileAt(store: ObjectStore, rootOid: string, path: string): { oid: string; data: Buffer } | null {
  const parts = path.split('/').filter((p) => p !== '');
  let oid = rootOid;
  for (let i = 0; i < parts.length; i += 1) {
    const obj = store.get(oid);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === parts[i]);
    if (entry === undefined) return null;
    if (i === parts.length - 1) {
      if (entry.mode === '40000') return null;
      const blob = store.get(entry.oid);
      if (blob === undefined || blob.type !== 'blob') return null;
      return { oid: entry.oid, data: blob.data };
    }
    if (entry.mode !== '40000') return null;
    oid = entry.oid;
  }
  return null;
}

/** List a directory's entries at `path` ('' for the root); null when not a tree. */
export function listDirAt(store: ObjectStore, rootOid: string, path: string): TreeEntry[] | null {
  const parts = path.split('/').filter((p) => p !== '');
  let oid = rootOid;
  for (const part of parts) {
    const obj = store.get(oid);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === part);
    if (entry === undefined || entry.mode !== '40000') return null;
    oid = entry.oid;
  }
  const obj = store.get(oid);
  if (obj === undefined || obj.type !== 'tree') return null;
  return parseTree(obj.data);
}

/**
 * Return a new root tree oid with `path` set to `blobOid` (or removed when null).
 * Missing intermediate trees are created; empty directories are pruned.
 */
export function updateTree(store: ObjectStore, rootOid: string | null, path: string, blobOid: string | null): string {
  const parts = path.split('/').filter((p) => p !== '');
  const put = (oid: string | null, depth: number): string | null => {
    const entries: TreeEntry[] = oid === null ? [] : parseTree((store.get(oid)?.data ?? Buffer.alloc(0)) as Buffer);
    const name = parts[depth] as string;
    if (depth === parts.length - 1) {
      const idx = entries.findIndex((e) => e.name === name);
      if (blobOid === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else if (idx >= 0) {
        entries[idx] = { mode: '100644', name, oid: blobOid };
      } else {
        entries.push({ mode: '100644', name, oid: blobOid });
      }
    } else {
      const idx = entries.findIndex((e) => e.name === name);
      const child = idx >= 0 ? entries[idx] : undefined;
      const next = put(child !== undefined && child.mode === '40000' ? child.oid : null, depth + 1);
      if (next === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else if (idx >= 0) {
        entries[idx] = { mode: '40000', name, oid: next };
      } else {
        entries.push({ mode: '40000', name, oid: next });
      }
    }
    if (entries.length === 0) return null;
    const data = buildTree(entries);
    const newOid = hashObject('tree', data);
    if (!store.has(newOid)) store.set(newOid, { type: 'tree', data });
    return newOid;
  };
  return put(rootOid, 0) ?? (() => {
    const data = buildTree([]);
    const oid = hashObject('tree', data);
    if (!store.has(oid)) store.set(oid, { type: 'tree', data });
    return oid;
  })();
}

export interface FileChange {
  path: string;
  status: 'added' | 'modified' | 'removed';
  oldOid: string | null;
  newOid: string | null;
  additions: number;
  deletions: number;
}

function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  const oldLines = oldText === '' ? [] : oldText.split('\n');
  const newLines = newText === '' ? [] : newText.split('\n');
  const counts = new Map<string, number>();
  for (const l of oldLines) counts.set(l, (counts.get(l) ?? 0) + 1);
  let additions = 0;
  for (const l of newLines) {
    const c = counts.get(l) ?? 0;
    if (c > 0) counts.set(l, c - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const c of counts.values()) deletions += c;
  return { additions, deletions };
}

function flattenTree(store: ObjectStore, rootOid: string, prefix: string, out: Map<string, string>): void {
  const obj = store.get(rootOid);
  if (obj === undefined || obj.type !== 'tree') return;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') flattenTree(store, e.oid, path, out);
    else if (e.mode !== '160000') out.set(path, e.oid);
  }
}

/** The file-level diff between two root trees. */
export function diffTrees(store: ObjectStore, oldRoot: string | null, newRoot: string | null): FileChange[] {
  const oldFiles = new Map<string, string>();
  const newFiles = new Map<string, string>();
  if (oldRoot !== null) flattenTree(store, oldRoot, '', oldFiles);
  if (newRoot !== null) flattenTree(store, newRoot, '', newFiles);
  const paths = new Set([...oldFiles.keys(), ...newFiles.keys()]);
  const changes: FileChange[] = [];
  for (const path of [...paths].sort()) {
    const oldOid = oldFiles.get(path) ?? null;
    const newOid = newFiles.get(path) ?? null;
    if (oldOid === newOid) continue;
    if (oldOid === null && newOid !== null) {
      const data = store.get(newOid)?.data ?? Buffer.alloc(0);
      const lines = data.length === 0 ? 0 : data.toString('utf8').split('\n').length;
      changes.push({ path, status: 'added', oldOid, newOid, additions: lines, deletions: 0 });
    } else if (newOid === null && oldOid !== null) {
      const data = store.get(oldOid)?.data ?? Buffer.alloc(0);
      const lines = data.length === 0 ? 0 : data.toString('utf8').split('\n').length;
      changes.push({ path, status: 'removed', oldOid, newOid, additions: 0, deletions: lines });
    } else if (oldOid !== null && newOid !== null) {
      const d = lineDiff((store.get(oldOid)?.data ?? Buffer.alloc(0)).toString('utf8'), (store.get(newOid)?.data ?? Buffer.alloc(0)).toString('utf8'));
      changes.push({ path, status: 'modified', oldOid, newOid, additions: d.additions, deletions: d.deletions });
    }
  }
  return changes;
}

/** The merge-base of two commits (first common ancestor by breadth-first search). */
export function mergeBase(store: ObjectStore, a: string, b: string): string | null {
  const ancestorsA = new Set<string>();
  const queueA = [a];
  while (queueA.length > 0) {
    const oid = queueA.shift() as string;
    if (ancestorsA.has(oid)) continue;
    ancestorsA.add(oid);
    const obj = store.get(oid);
    if (obj !== undefined && obj.type === 'commit') for (const p of parseCommit(obj.data).parents) queueA.push(p);
  }
  const queueB = [b];
  const seenB = new Set<string>();
  while (queueB.length > 0) {
    const oid = queueB.shift() as string;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    if (ancestorsA.has(oid)) return oid;
    const obj = store.get(oid);
    if (obj !== undefined && obj.type === 'commit') for (const p of parseCommit(obj.data).parents) queueB.push(p);
  }
  return null;
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

function readVarint(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start] as number;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let off = start + 1;
  while (byte & 0x80) {
    if (off >= buffer.length) throw new Error('pack entry size varint is truncated');
    byte = buffer[off] as number;
    off += 1;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: off, first };
}

function readOfsBase(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start] as number;
  let off = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    if (off >= buffer.length) throw new Error('ofs-delta base offset is truncated');
    byte = buffer[off] as number;
    off += 1;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: off };
}

function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const view = buffer.subarray(offset);
  let result: { buffer: Buffer; engine: { bytesWritten: number } };
  try {
    result = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  } catch (err) {
    throw new Error(`zlib stream at offset ${offset} did not inflate: ${(err as Error).message}`);
  }
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result.buffer, next: offset + written };
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (off >= delta.length) throw new Error('delta size varint is truncated');
      const byte = delta[off] as number;
      off += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error(`delta expects a ${sourceSize}-byte base, got ${base.length}`);
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (off < delta.length) {
    const op = delta[off] as number;
    off += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          if (off >= delta.length) throw new Error('delta copy offset is truncated');
          copyOffset += (delta[off] as number) * 2 ** shift;
          off += 1;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          if (off >= delta.length) throw new Error('delta copy size is truncated');
          copySize += (delta[off] as number) * 2 ** shift;
          off += 1;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      if (copyOffset + copySize > base.length) throw new Error('delta copy is out of the base');
      if (written + copySize > targetSize) throw new Error('delta writes past its target size');
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      if (off + literal > delta.length) throw new Error('delta insert is truncated');
      if (written + literal > targetSize) throw new Error('delta writes past its target size');
      delta.copy(out, written, off, off + literal);
      off += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error(`delta produced ${written} of ${targetSize} bytes`);
  return out;
}

/** Read a packfile into objects (deltas resolved). Throws on any corruption. */
export function readPack(pack: Buffer): Map<string, GitObject> {
  if (pack.length < 32) throw new Error(`pack is ${pack.length} bytes, too short`);
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const trailer = pack.subarray(trailerStart).toString('hex');
  if (sha1Hex(pack.subarray(0, trailerStart)) !== trailer) throw new Error('pack trailer checksum mismatch');

  interface Raw {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let off = 12;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = off;
    const header = readVarint(pack, off);
    off = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsBase(pack, off);
      off = base.next;
      if (base.baseOffset <= 0 || base.baseOffset > entryOffset) throw new Error('ofs-delta names an invalid base offset');
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      if (off + 20 > trailerStart) throw new Error('ref-delta base id is truncated');
      entry.baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`pack entry ${i} has unknown type code ${typeCode}`);
    }
    const inflated = inflateAt(pack.subarray(0, trailerStart), off);
    if (inflated.data.length !== header.size) throw new Error(`pack entry at ${entryOffset} declares ${header.size} bytes but inflated to ${inflated.data.length}`);
    entry.data = inflated.data;
    off = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  if (off !== trailerStart) throw new Error(`pack has ${trailerStart - off} stray bytes before its trailer`);

  const objects = new Map<string, GitObject>();
  const resolvedByOid = new Map<string, { type: ObjType; data: Buffer }>();
  // Plain entries by their computed id, so a REF_DELTA base is an O(1) lookup.
  const plainByOid = new Map<string, Raw>();
  for (const entry of raw) {
    if (entry.typeCode !== 6 && entry.typeCode !== 7) {
      plainByOid.set(hashObject(CODE_TYPE[entry.typeCode] as ObjType, entry.data), entry);
    }
  }
  const resolving = new Set<number>();
  const resolve = (entry: Raw): { type: ObjType; data: Buffer } => {
    if (resolving.has(entry.offset)) throw new Error(`delta cycle at pack offset ${entry.offset}`);
    resolving.add(entry.offset);
    let type: ObjType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (baseEntry === undefined) throw new Error('ofs-delta base is not in the pack');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid as string;
      const direct = resolvedByOid.get(wanted);
      if (direct !== undefined) {
        type = direct.type;
        data = applyDelta(direct.data, entry.data);
      } else {
        const baseEntry = plainByOid.get(wanted);
        if (baseEntry !== undefined) {
          const base = resolve(baseEntry);
          type = base.type;
          data = applyDelta(base.data, entry.data);
        } else {
          // A delta-chained base: resolve every other entry until the base appears.
          let found: { type: ObjType; data: Buffer } | undefined;
          for (const candidate of raw) {
            if (candidate.offset === entry.offset) continue;
            const r = resolve(candidate);
            if (hashObject(r.type, r.data) === wanted) {
              found = r;
              break;
            }
          }
          if (found === undefined) throw new Error(`ref-delta base ${wanted} is not in the pack`);
          type = found.type;
          data = applyDelta(found.data, entry.data);
        }
      }
    } else {
      type = CODE_TYPE[entry.typeCode] as ObjType;
      data = entry.data;
    }
    resolving.delete(entry.offset);
    const oid = hashObject(type, data);
    resolvedByOid.set(oid, { type, data });
    return { type, data };
  };
  for (const entry of raw) {
    const r = resolve(entry);
    const oid = hashObject(r.type, r.data);
    if (!objects.has(oid)) objects.set(oid, { type: r.type, data: r.data });
  }
  return objects;
}

function writePackObjectHeader(type: ObjType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas). */
export function writePack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const object of list) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    // Level 1 is far faster than the default and still a valid zlib stream;
    // pack bytes are not scored, only their integrity, so trade size for speed.
    parts.push(deflateSync(object.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ---------------------------------------------------------------------------------------------
// the forge's resources
// ---------------------------------------------------------------------------------------------

export interface IssueComment {
  id: number;
  body: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: IssueComment[];
  seq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergeCommitSha: string | null;
  mergedAt: string | null;
  seq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  refs: Map<string, string>;
  objects: ObjectStore;
  labels: Map<string, { name: string; color: string }>;
  issues: Issue[];
  issueByNumber: Map<number, Issue>;
  pulls: Pull[];
  pullByNumber: Map<number, Pull>;
  nextNumber: number;
  nextCommentId: number;
  seq: number;
}

export function globalId(kind: string, ...parts: (string | number)[]): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64');
}

export function parseGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const [kind, ...parts] = text.split(':');
    if (kind === undefined || kind === '' || parts.length === 0) return null;
    return { kind, parts };
  } catch {
    return null;
  }
}

export class Forge {
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 1;
  private nextIssueId = 1;
  private nextPullId = 1;
  private seqCounter = 0;

  seq(): number {
    this.seqCounter += 1;
    return this.seqCounter;
  }

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    const repo: Repo = {
      id: this.nextRepoId,
      name,
      owner,
      description,
      isPrivate,
      defaultBranch: 'main',
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: [],
      issueByNumber: new Map(),
      pulls: [],
      pullByNumber: new Map(),
      nextNumber: 1,
      nextCommentId: 1,
      seq: this.seq(),
    };
    this.nextRepoId += 1;
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  /** Rename a repository, moving it under its new key. */
  renameRepo(repo: Repo, newName: string): void {
    this.repos.delete(this.key(repo.owner, repo.name));
    repo.name = newName;
    this.repos.set(this.key(repo.owner, repo.name), repo);
  }

  /** Create a label (or return the existing one with that name). */
  createLabel(repo: Repo, name: string, color: string): { name: string; color: string } {
    const existing = repo.labels.get(name);
    if (existing !== undefined) return existing;
    const label = { name, color };
    repo.labels.set(name, label);
    return label;
  }

  /** Add a comment to an issue. */
  addComment(repo: Repo, issue: Issue, body: string): IssueComment {
    const comment: IssueComment = { id: repo.nextCommentId, body };
    repo.nextCommentId += 1;
    issue.comments.push(comment);
    return comment;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER_LOGIN) out.push(repo);
    return out;
  }

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const issue: Issue = {
      id: this.nextIssueId,
      number: repo.nextNumber,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
      seq: this.seq(),
    };
    this.nextIssueId += 1;
    repo.nextNumber += 1;
    repo.issues.push(issue);
    repo.issueByNumber.set(issue.number, issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, head: string, base: string, headSha: string, baseSha: string): Pull {
    const pull: Pull = {
      id: this.nextPullId,
      number: repo.nextNumber,
      title,
      body,
      state: 'open',
      head,
      base,
      headSha,
      baseSha,
      merged: false,
      mergeCommitSha: null,
      mergedAt: null,
      seq: this.seq(),
    };
    this.nextPullId += 1;
    repo.nextNumber += 1;
    repo.pulls.push(pull);
    repo.pullByNumber.set(pull.number, pull);
    return pull;
  }

  /** The current tip of a pull request's base branch (or the sha captured at creation if the branch is gone). */
  baseTip(repo: Repo, pull: Pull): string {
    return repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  }

  /** The changed files of a pull request (merge-base tree vs head tree). */
  pullFiles(repo: Repo, pull: Pull): FileChange[] {
    const base = mergeBase(repo.objects, pull.headSha, this.baseTip(repo, pull));
    const baseTree = base === null ? null : parseCommit(repo.objects.get(base)?.data ?? Buffer.alloc(0)).tree;
    const headTree = parseCommit(repo.objects.get(pull.headSha)?.data ?? Buffer.alloc(0)).tree;
    return diffTrees(repo.objects, baseTree, headTree);
  }

  /** Whether a pull request merges cleanly (no path changed on both sides differently). */
  pullMergeable(repo: Repo, pull: Pull): boolean {
    const baseTip = this.baseTip(repo, pull);
    const base = mergeBase(repo.objects, pull.headSha, baseTip);
    if (base === null) return true;
    const baseTree = parseCommit(repo.objects.get(base)?.data ?? Buffer.alloc(0)).tree;
    const headTree = parseCommit(repo.objects.get(pull.headSha)?.data ?? Buffer.alloc(0)).tree;
    const baseTipTree = parseCommit(repo.objects.get(baseTip)?.data ?? Buffer.alloc(0)).tree;
    const headChanges = diffTrees(repo.objects, baseTree, headTree);
    const baseChanges = diffTrees(repo.objects, baseTree, baseTipTree);
    const baseByPath = new Map(baseChanges.map((c) => [c.path, c]));
    for (const change of headChanges) {
      const other = baseByPath.get(change.path);
      if (other === undefined) continue;
      if (other.newOid !== change.newOid) return false;
    }
    return true;
  }

  /** Squash-merge a pull request onto its base branch. Returns the merge commit sha or null on conflict. */
  mergePull(repo: Repo, pull: Pull): string | null {
    if (pull.state !== 'open' || pull.merged) return null;
    if (!this.pullMergeable(repo, pull)) return null;
    const baseTip = this.baseTip(repo, pull);
    const headTree = parseCommit(repo.objects.get(pull.headSha)?.data ?? Buffer.alloc(0)).tree;
    const commitData = buildCommit({
      tree: headTree,
      parents: [baseTip],
      message: `Merge pull request #${pull.number} from ${repo.owner}/${pull.head}\n\n${pull.title}`,
    });
    const sha = hashObject('commit', commitData);
    repo.objects.set(sha, { type: 'commit', data: commitData });
    repo.refs.set(`refs/heads/${pull.base}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergeCommitSha = sha;
    pull.mergedAt = FIXED_ISO;
    return sha;
  }

  /** Commit one file change on a branch; returns the new commit sha. */
  commitFile(repo: Repo, branch: string, path: string, content: Buffer, message: string): { commitSha: string; blobSha: string } {
    const blobSha = hashObject('blob', content);
    repo.objects.set(blobSha, { type: 'blob', data: content });
    const refName = `refs/heads/${branch}`;
    const tip = repo.refs.get(refName);
    const parentTree = tip === undefined ? null : parseCommit(repo.objects.get(tip)?.data ?? Buffer.alloc(0)).tree;
    const rootOid = updateTree(repo.objects, parentTree, path, blobSha);
    const commitData = buildCommit({ tree: rootOid, parents: tip === undefined ? [] : [tip], message });
    const commitSha = hashObject('commit', commitData);
    repo.objects.set(commitSha, { type: 'commit', data: commitData });
    repo.refs.set(refName, commitSha);
    return { commitSha, blobSha };
  }

  /** Delete one file on a branch; returns the new commit sha, or null when the file is absent. */
  deleteFile(repo: Repo, branch: string, path: string, message: string): string | null {
    const refName = `refs/heads/${branch}`;
    const tip = repo.refs.get(refName);
    if (tip === undefined) return null;
    const parentTree = parseCommit(repo.objects.get(tip)?.data ?? Buffer.alloc(0)).tree;
    if (readFileAt(repo.objects, parentTree, path) === null) return null;
    const rootOid = updateTree(repo.objects, parentTree, path, null);
    const commitData = buildCommit({ tree: rootOid, parents: [tip], message });
    const commitSha = hashObject('commit', commitData);
    repo.objects.set(commitSha, { type: 'commit', data: commitData });
    repo.refs.set(refName, commitSha);
    return commitSha;
  }
}
