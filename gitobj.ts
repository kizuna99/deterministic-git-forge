// Git object model: hashing, parsing, building, packs, reachability and tree merges.
// Pure functions over an object lookup; the store lives in store.ts. Objects are kept as their raw
// payloads (the bytes after the "<type> <len>\0" header) and hashed with SHA-1, exactly like git.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject {
  type: ObjType;
  data: Buffer;
}
export type Lookup = (oid: string) => GitObject | undefined;

export const ZERO_OID = '0'.repeat(40);
const TYPE_IDS: Record<string, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const TYPE_TO_ID: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

export function hashObject(type: ObjType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(`${type} ${data.length}\0`);
  hash.update(data);
  return hash.digest('hex');
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function isSha40(value: string): boolean {
  return /^[0-9a-f]{40}$/.test(value);
}

// ---------------------------------------------------------------------------
// Commits, trees, tags

export interface CommitInfo {
  tree: string;
  parents: string[];
  authorName: string;
  authorEmail: string;
  authorTime: number;
  committerName: string;
  committerEmail: string;
  committerTime: number;
  message: string;
}

function parseIdent(line: string): { name: string; email: string; time: number } {
  // "Name <email> 1700000000 +0000"
  const lt = line.indexOf('<');
  const gt = line.indexOf('>', lt);
  const name = lt >= 0 ? line.slice(0, lt).trim() : line.trim();
  const email = lt >= 0 && gt > lt ? line.slice(lt + 1, gt) : '';
  const rest = gt >= 0 ? line.slice(gt + 1).trim() : '';
  const time = Number.parseInt(rest.split(' ')[0] ?? '0', 10) || 0;
  return { name, email, time };
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split >= 0 ? text.slice(0, split) : text;
  const message = split >= 0 ? text.slice(split + 2) : '';
  const info: CommitInfo = {
    tree: '',
    parents: [],
    authorName: '',
    authorEmail: '',
    authorTime: 0,
    committerName: '',
    committerEmail: '',
    committerTime: 0,
    message,
  };
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) info.tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) info.parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      const id = parseIdent(line.slice(7));
      info.authorName = id.name;
      info.authorEmail = id.email;
      info.authorTime = id.time;
    } else if (line.startsWith('committer ')) {
      const id = parseIdent(line.slice(10));
      info.committerName = id.name;
      info.committerEmail = id.email;
      info.committerTime = id.time;
    }
  }
  return info;
}

export interface Identity {
  name: string;
  email: string;
  time: number;
}

export function buildCommit(opts: {
  tree: string;
  parents: string[];
  author: Identity;
  committer: Identity;
  message: string;
}): Buffer {
  const lines = [`tree ${opts.tree}`];
  for (const parent of opts.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${opts.author.name} <${opts.author.email}> ${opts.author.time} +0000`);
  lines.push(`committer ${opts.committer.name} <${opts.committer.email}> ${opts.committer.time} +0000`);
  return Buffer.from(`${lines.join('\n')}\n\n${opts.message}`, 'utf8');
}

export interface TreeEntry {
  mode: string; // '100644' | '100755' | '120000' | '40000' (dirs stored without leading zero in the raw bytes)
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const mode = data.toString('ascii', offset, space);
    const nul = data.indexOf(0x00, space);
    if (nul < 0 || nul + 21 > data.length) break;
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Git's tree ordering: names compare bytewise, with directories sorting as if they ended in '/'. */
export function compareTreeEntries(a: TreeEntry, b: TreeEntry): number {
  const an = a.mode === '40000' ? `${a.name}/` : a.name;
  const bn = b.mode === '40000' ? `${b.name}/` : b.name;
  return an < bn ? -1 : an > bn ? 1 : 0;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeEntries);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export function parseTagTarget(data: Buffer): string {
  const match = /^object ([0-9a-f]{40})/m.exec(data.toString('utf8'));
  return match ? match[1] : '';
}

/** Peel a tag (or any object) down to the commit it ultimately points at; undefined if it never lands on one. */
export function peelToCommit(lookup: Lookup, oid: string, depth = 0): string | undefined {
  if (depth > 16) return undefined;
  const obj = lookup(oid);
  if (!obj) return undefined;
  if (obj.type === 'commit') return oid;
  if (obj.type === 'tag') {
    const target = parseTagTarget(obj.data);
    return target ? peelToCommit(lookup, target, depth + 1) : undefined;
  }
  return undefined;
}

// ---------------------------------------------------------------------------
// Reachability and ancestry

/** Every object reachable from the given roots (commits walk to trees, trees to entries, tags to targets). */
export function collectReachable(lookup: Lookup, roots: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [...roots];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = lookup(oid);
    if (!obj) continue;
    seen.add(oid);
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      if (commit.tree) stack.push(commit.tree);
      for (const parent of commit.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    } else if (obj.type === 'tag') {
      const target = parseTagTarget(obj.data);
      if (target) stack.push(target);
    }
  }
  return seen;
}

/** All commit ancestors of a commit, itself included. */
export function commitAncestors(lookup: Lookup, tip: string): Set<string> {
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    seen.add(oid);
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return seen;
}

export function isAncestor(lookup: Lookup, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return false;
}

/** The best common ancestor of two commits (first shared commit on a breadth-first walk from b). */
export function mergeBase(lookup: Lookup, a: string, b: string): string | undefined {
  const ancestorsOfA = commitAncestors(lookup, a);
  const seen = new Set<string>();
  const queue = [b];
  while (queue.length > 0) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (ancestorsOfA.has(oid)) return oid;
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) queue.push(parent);
  }
  return undefined;
}

/**
 * Depth-limited reachability for shallow fetches. Returns the objects to send and the new shallow
 * boundary: commits at exactly `depth` generations from a want (the want itself is generation 1)
 * that still have parents. Ancestors beyond the boundary are excluded entirely.
 */
export function collectShallow(
  lookup: Lookup,
  wants: string[],
  depth: number,
): { objects: Set<string>; boundary: string[] } {
  const commitDepth = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  for (const want of wants) {
    const commit = peelToCommit(lookup, want);
    if (commit) queue.push({ oid: commit, depth: 1 });
  }
  const boundary: string[] = [];
  while (queue.length > 0) {
    const { oid, depth: d } = queue.shift() as { oid: string; depth: number };
    const known = commitDepth.get(oid);
    if (known !== undefined && known <= d) continue;
    commitDepth.set(oid, d);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    const commit = parseCommit(obj.data);
    if (d >= depth) {
      if (commit.parents.length > 0) boundary.push(oid);
      continue;
    }
    for (const parent of commit.parents) queue.push({ oid: parent, depth: d + 1 });
  }
  const objects = new Set<string>();
  for (const oid of commitDepth.keys()) {
    objects.add(oid);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    const tree = parseCommit(obj.data).tree;
    if (tree) {
      // Full tree closure for every included commit, minus nothing: trees are shared freely.
      const stack = [tree];
      while (stack.length > 0) {
        const current = stack.pop() as string;
        if (objects.has(current)) continue;
        const treeObj = lookup(current);
        if (!treeObj) continue;
        objects.add(current);
        if (treeObj.type === 'tree') for (const entry of parseTree(treeObj.data)) stack.push(entry.oid);
      }
    }
  }
  // Tag objects pointing at wanted commits ride along (the client asks for the ref target itself).
  for (const want of wants) {
    const obj = lookup(want);
    if (obj && obj.type === 'tag') objects.add(want);
  }
  return { objects, boundary: [...new Set(boundary)] };
}

// ---------------------------------------------------------------------------
// Tree flattening, path access, rebuild — the contents API's working set

/** Flatten a tree into path -> {mode, oid} for files (and symlinks), recursing into subtrees. */
export function flattenTree(lookup: Lookup, treeOid: string): Map<string, { mode: string; oid: string }> {
  const out = new Map<string, { mode: string; oid: string }>();
  const walk = (oid: string, prefix: string): void => {
    const obj = lookup(oid);
    if (!obj || obj.type !== 'tree') return;
    for (const entry of parseTree(obj.data)) {
      const path = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, { mode: entry.mode, oid: entry.oid });
    }
  };
  if (treeOid) walk(treeOid, '');
  return out;
}

/** Look up one path inside a tree: {kind:'blob'|'tree', oid, mode} or undefined. */
export function lookupPath(
  lookup: Lookup,
  treeOid: string,
  path: string,
): { kind: 'blob' | 'tree'; oid: string; mode: string } | undefined {
  const parts = path.split('/').filter((part) => part.length > 0);
  let current = treeOid;
  for (let i = 0; i < parts.length; i += 1) {
    const obj = lookup(current);
    if (!obj || obj.type !== 'tree') return undefined;
    const entry = parseTree(obj.data).find((candidate) => candidate.name === parts[i]);
    if (!entry) return undefined;
    if (i === parts.length - 1) {
      return entry.mode === '40000'
        ? { kind: 'tree', oid: entry.oid, mode: entry.mode }
        : { kind: 'blob', oid: entry.oid, mode: entry.mode };
    }
    if (entry.mode !== '40000') return undefined;
    current = entry.oid;
  }
  return undefined;
}

/** List one directory level of a tree. */
export function listTreeLevel(lookup: Lookup, treeOid: string): TreeEntry[] {
  const obj = lookup(treeOid);
  if (!obj || obj.type !== 'tree') return [];
  return parseTree(obj.data);
}

/**
 * Rebuild a tree hierarchy from a flattened file map, storing every new tree object through `put`.
 * Returns the root tree oid (the empty tree when the map is empty).
 */
export function buildTreeFromFlat(
  flat: Map<string, { mode: string; oid: string }>,
  put: (type: ObjType, data: Buffer) => string,
): string {
  interface Dir {
    files: Map<string, { mode: string; oid: string }>;
    dirs: Map<string, Dir>;
  }
  const root: Dir = { files: new Map(), dirs: new Map() };
  for (const [path, file] of flat) {
    const parts = path.split('/');
    let dir = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      let next = dir.dirs.get(parts[i]);
      if (!next) {
        next = { files: new Map(), dirs: new Map() };
        dir.dirs.set(parts[i], next);
      }
      dir = next;
    }
    dir.files.set(parts[parts.length - 1], file);
  }
  const emit = (dir: Dir): string => {
    const entries: TreeEntry[] = [];
    for (const [name, file] of dir.files) entries.push({ mode: file.mode, name, oid: file.oid });
    for (const [name, sub] of dir.dirs) entries.push({ mode: '40000', name, oid: emit(sub) });
    return put('tree', buildTree(entries));
  };
  return emit(root);
}

// ---------------------------------------------------------------------------
// Three-way tree merge (squash merges and mergeability)

export interface MergeOutcome {
  tree: string;
  conflict: boolean;
}

/**
 * Merge `theirs` into `ours` relative to `base`, tree by tree. A path changed on only one side takes
 * that side; changed identically on both takes it; changed differently on both (including
 * delete-vs-modify and file-vs-directory) is a conflict. No line-level merging.
 */
export function mergeTrees(
  lookup: Lookup,
  put: (type: ObjType, data: Buffer) => string,
  base: string,
  ours: string,
  theirs: string,
): MergeOutcome {
  if (ours === theirs) return { tree: ours, conflict: false };
  if (base === ours) return { tree: theirs, conflict: false };
  if (base === theirs) return { tree: ours, conflict: false };

  const baseEntries = new Map(listTreeLevel(lookup, base).map((e) => [e.name, e]));
  const ourEntries = new Map(listTreeLevel(lookup, ours).map((e) => [e.name, e]));
  const theirEntries = new Map(listTreeLevel(lookup, theirs).map((e) => [e.name, e]));
  const names = new Set([...baseEntries.keys(), ...ourEntries.keys(), ...theirEntries.keys()]);
  const merged: TreeEntry[] = [];
  let conflict = false;

  const sameEntry = (a?: TreeEntry, b?: TreeEntry): boolean =>
    a === b || (a !== undefined && b !== undefined && a.mode === b.mode && a.oid === b.oid);

  for (const name of names) {
    const b = baseEntries.get(name);
    const o = ourEntries.get(name);
    const t = theirEntries.get(name);
    if (sameEntry(o, t)) {
      if (o) merged.push(o);
      continue;
    }
    if (sameEntry(b, o)) {
      if (t) merged.push(t);
      continue;
    }
    if (sameEntry(b, t)) {
      if (o) merged.push(o);
      continue;
    }
    // Both sides changed this name differently.
    if (o && t && o.mode === '40000' && t.mode === '40000') {
      const sub = mergeTrees(lookup, put, b && b.mode === '40000' ? b.oid : '', o.oid, t.oid);
      if (sub.conflict) conflict = true;
      merged.push({ mode: '40000', name, oid: sub.tree });
      continue;
    }
    conflict = true;
    if (o) merged.push(o);
    else if (t) merged.push(t);
  }
  return { tree: put('tree', buildTree(merged)), conflict };
}

// ---------------------------------------------------------------------------
// Line diff counts for the pull-request files view

/** Addition/deletion counts between two text payloads via a simple LCS over lines. */
export function lineDiffCounts(oldText: string, newText: string): { additions: number; deletions: number } {
  const oldLines = oldText === '' ? [] : oldText.split('\n');
  const newLines = newText === '' ? [] : newText.split('\n');
  // Trailing newline produces a phantom empty final element; drop it for line counting.
  if (oldLines.length > 0 && oldLines[oldLines.length - 1] === '') oldLines.pop();
  if (newLines.length > 0 && newLines[newLines.length - 1] === '') newLines.pop();
  const m = oldLines.length;
  const n = newLines.length;
  if (m * n > 4_000_000) {
    // Very large files: fall back to a prefix/suffix trim count rather than an O(mn) table.
    let prefix = 0;
    while (prefix < m && prefix < n && oldLines[prefix] === newLines[prefix]) prefix += 1;
    let suffix = 0;
    while (suffix < m - prefix && suffix < n - prefix && oldLines[m - 1 - suffix] === newLines[n - 1 - suffix]) suffix += 1;
    return { additions: n - prefix - suffix, deletions: m - prefix - suffix };
  }
  const dp: number[] = new Array((m + 1) * (n + 1)).fill(0);
  for (let i = m - 1; i >= 0; i -= 1) {
    for (let j = n - 1; j >= 0; j -= 1) {
      dp[i * (n + 1) + j] =
        oldLines[i] === newLines[j]
          ? dp[(i + 1) * (n + 1) + j + 1] + 1
          : Math.max(dp[(i + 1) * (n + 1) + j], dp[i * (n + 1) + j + 1]);
    }
  }
  const common = dp[0];
  return { additions: n - common, deletions: m - common };
}

// ---------------------------------------------------------------------------
// Packs

/** Serialize objects into a pack v2 byte stream (plain objects only, zlib-deflated whole). */
export function writePack(objects: Array<{ oid: string; type: ObjType; data: Buffer }>): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    const size = obj.data.length;
    let first = (TYPE_TO_ID[obj.type] << 4) | (size & 0x0f);
    let rest = size >>> 4;
    const headBytes: number[] = [];
    if (rest > 0) first |= 0x80;
    headBytes.push(first);
    while (rest > 0) {
      let byte = rest & 0x7f;
      rest >>>= 7;
      if (rest > 0) byte |= 0x80;
      headBytes.push(byte);
    }
    parts.push(Buffer.from(headBytes));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

export interface PackObject {
  oid: string;
  type: ObjType;
  data: Buffer;
}

/**
 * Parse a pack, resolving OFS/REF deltas against objects earlier in the pack or through `base`
 * (the receiver's existing store, for thin packs). Throws on any malformed input.
 */
export function readPack(pack: Buffer, base: Lookup): PackObject[] {
  if (pack.length < 12 + 20) throw new Error('pack too short');
  if (pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('bad pack signature');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const dataEnd = pack.length - 20;
  const trailer = pack.subarray(dataEnd);
  const computed = createHash('sha1').update(pack.subarray(0, dataEnd)).digest();
  if (!computed.equals(trailer)) throw new Error('pack checksum mismatch');

  const byOffset = new Map<number, PackObject>();
  const byOid = new Map<string, PackObject>();
  let offset = 12;

  const readVarintOffset = (): number => {
    let byte = pack[offset];
    offset += 1;
    let value = byte & 0x7f;
    while (byte & 0x80) {
      byte = pack[offset];
      offset += 1;
      value = ((value + 1) << 7) | (byte & 0x7f);
    }
    return value;
  };

  for (let i = 0; i < count; i += 1) {
    const objectStart = offset;
    let byte = pack[offset];
    offset += 1;
    const typeId = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[offset];
      offset += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let resolved: { type: ObjType; data: Buffer };
    if (typeId === 6) {
      // OFS_DELTA
      const back = readVarintOffset();
      const baseObj = byOffset.get(objectStart - back);
      if (!baseObj) throw new Error('ofs-delta base missing');
      const delta = inflateSync(pack.subarray(offset, dataEnd));
      offset += consumedInflateLength(pack, offset, dataEnd);
      resolved = { type: baseObj.type, data: applyDelta(baseObj.data, delta) };
    } else if (typeId === 7) {
      // REF_DELTA
      const baseOid = pack.toString('hex', offset, offset + 20);
      offset += 20;
      const inPack = byOid.get(baseOid);
      const stored = inPack ?? (base(baseOid) ? { type: (base(baseOid) as GitObject).type, data: (base(baseOid) as GitObject).data } : undefined);
      if (!stored) throw new Error('ref-delta base missing');
      const delta = inflateSync(pack.subarray(offset, dataEnd));
      offset += consumedInflateLength(pack, offset, dataEnd);
      resolved = { type: stored.type, data: applyDelta(stored.data, delta) };
    } else {
      const type = TYPE_IDS[typeId];
      if (!type) throw new Error(`unknown pack object type ${typeId}`);
      const data = inflateSync(pack.subarray(offset, dataEnd));
      offset += consumedInflateLength(pack, offset, dataEnd);
      if (data.length !== size) throw new Error('pack object size mismatch');
      resolved = { type, data };
    }
    const oid = hashObject(resolved.type, resolved.data);
    const record: PackObject = { oid, type: resolved.type, data: resolved.data };
    byOffset.set(objectStart, record);
    byOid.set(oid, record);
  }
  return [...byOid.values()];
}

/**
 * Length of the deflate stream starting at `offset`. zlib's inflateSync does not report consumed
 * bytes, but a truncated stream throws while an exact or longer prefix inflates (trailing bytes
 * are ignored), so the stream's length is the smallest prefix that inflates successfully.
 */
function consumedInflateLength(pack: Buffer, offset: number, end: number): number {
  let lo = 1;
  let hi = end - offset;
  let best = hi;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    try {
      inflateSync(pack.subarray(offset, offset + mid));
      best = mid;
      hi = mid - 1;
    } catch {
      lo = mid + 1;
    }
  }
  return best;
}

/** Apply a git delta buffer to a base payload. */
export function applyDelta(baseData: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset];
      offset += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  if (sourceSize !== baseData.length) throw new Error('delta base size mismatch');
  const targetSize = readVarint();
  const out: Buffer[] = [];
  let produced = 0;
  while (offset < delta.length) {
    const opcode = delta[offset];
    offset += 1;
    if (opcode & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (opcode & (1 << bit)) {
          copyOffset |= delta[offset] << (8 * bit);
          offset += 1;
        }
      }
      for (let bit = 0; bit < 3; bit += 1) {
        if (opcode & (0x10 << bit)) {
          copySize |= delta[offset] << (8 * bit);
          offset += 1;
        }
      }
      if (copySize === 0) copySize = 0x10000;
      out.push(baseData.subarray(copyOffset, copyOffset + copySize));
      produced += copySize;
    } else if (opcode > 0) {
      out.push(delta.subarray(offset, offset + opcode));
      produced += opcode;
      offset += opcode;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out);
  if (result.length !== targetSize || produced !== targetSize) throw new Error('delta target size mismatch');
  return result;
}

// ---------------------------------------------------------------------------
// Pkt-line framing

export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const length = body.length + 4;
  if (length > 0xffff) throw new Error('pkt-line too large');
  return Buffer.concat([Buffer.from(length.toString(16).padStart(4, '0'), 'ascii'), body]);
}

export const FLUSH_PKT = Buffer.from('0000', 'ascii');
export const DELIM_PKT = Buffer.from('0001', 'ascii');

export interface PktRead {
  /** Each payload in order; the flush packet is represented as null, the delimiter as the string '\x01'. */
  lines: Array<Buffer | null>;
  /** Byte offset just past the last consumed packet (start of any raw pack bytes). */
  rest: number;
}

/** Parse pkt-lines from the front of a buffer. Throws when a length prefix is malformed. */
export function readPktLines(buffer: Buffer, start = 0): PktRead {
  const lines: Array<Buffer | null> = [];
  let offset = start;
  while (offset < buffer.length) {
    if (offset + 4 > buffer.length) throw new Error('truncated pkt-line');
    const hex = buffer.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error('invalid pkt-line length');
    const length = Number.parseInt(hex, 16);
    if (length === 0) {
      lines.push(null);
      offset += 4;
      break; // a flush terminates this segment; the caller decides what follows
    }
    if (length === 1) {
      lines.push(Buffer.from('\x01'));
      offset += 4;
      continue;
    }
    if (length === 2) {
      // response-end packet (v2): treat as a segment terminator like flush
      lines.push(null);
      offset += 4;
      break;
    }
    if (length < 4 || offset + length > buffer.length) throw new Error('pkt-line overruns buffer');
    lines.push(buffer.subarray(offset + 4, offset + length));
    offset += length;
    if (lines.length > 100000) throw new Error('too many pkt-lines');
  }
  return { lines, rest: offset };
}

/**
 * Read one pkt-line segment starting at `start`: payloads up to the terminating flush (or
 * response-end) packet. Delimiter packets are kept as a one-byte Buffer('\x01') line so callers
 * can split command arguments. `rest` is the offset just past the terminator — the start of the
 * next segment, or of raw pack bytes after a push's command block.
 */
export function readPktSegment(buffer: Buffer, start = 0): { lines: Buffer[]; rest: number } {
  const lines: Buffer[] = [];
  let offset = start;
  while (offset < buffer.length) {
    if (offset + 4 > buffer.length) throw new Error('truncated pkt-line');
    const hex = buffer.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error('invalid pkt-line length');
    const length = Number.parseInt(hex, 16);
    if (length === 0 || length === 2) {
      return { lines, rest: offset + 4 };
    }
    if (length === 1) {
      lines.push(Buffer.from('\x01'));
      offset += 4;
      continue;
    }
    if (length < 4 || offset + length > buffer.length) throw new Error('pkt-line overruns buffer');
    lines.push(buffer.subarray(offset + 4, offset + length));
    offset += length;
    if (lines.length > 100000) throw new Error('too many pkt-lines');
  }
  return { lines, rest: offset };
}
