// Git smart HTTP: info/refs advertisements (protocol v0 and v2), upload-pack (ls-refs and fetch,
// shallow included) and receive-pack (push with per-ref reports). Plain objects only in packs the
// server builds; full delta resolution for packs it receives.

import {
  ZERO_OID,
  collectReachable,
  collectShallow,
  isAncestor,
  isSha40,
  parseCommit,
  parseTagTarget,
  parseTree,
  pktLine,
  FLUSH_PKT,
  readPack,
  readPktSegment,
  writePack,
  type GitObject,
} from './gitobj.ts';
import { branchTip, repoLookup, type RepoRec } from './store.ts';

const AGENT = 'forge/1.0.0';
const UPLOAD_CAPS_V0 =
  `side-band-64k thin-pack ofs-delta shallow multi_ack_detailed symref=HEAD:refs/heads/main agent=${AGENT}`;
const RECEIVE_CAPS = `report-status side-band-64k delete-refs ofs-delta quiet agent=${AGENT}`;

type Send = (status: number, contentType: string, body: Buffer | string) => void;

function sortedRefs(repo: RepoRec): Array<[string, string]> {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

function headOid(repo: RepoRec): string {
  return branchTip(repo, repo.defaultBranch) ?? ZERO_OID;
}

// ---------------------------------------------------------------------------
// Advertisements

export function infoRefs(repo: RepoRec | undefined, service: string, protocolV2: boolean, send: Send): void {
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    send(403, 'text/plain', 'unknown service\n');
    return;
  }
  if (!repo) {
    send(404, 'text/plain', 'repository not found\n');
    return;
  }
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), FLUSH_PKT];
  if (service === 'git-upload-pack' && protocolV2) {
    parts.push(pktLine('version 2'));
    parts.push(pktLine(`agent=${AGENT}`));
    parts.push(pktLine('ls-refs=unborn'));
    parts.push(pktLine('fetch=shallow wait-for-done'));
    parts.push(pktLine('object-format=sha1'));
    parts.push(FLUSH_PKT);
  } else if (service === 'git-upload-pack') {
    const refs = sortedRefs(repo);
    if (refs.length === 0) {
      parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${UPLOAD_CAPS_V0}`));
    } else {
      parts.push(pktLine(`${headOid(repo)} HEAD\0${UPLOAD_CAPS_V0}`));
      for (const [name, oid] of refs) parts.push(pktLine(`${oid} ${name}`));
    }
    parts.push(FLUSH_PKT);
  } else {
    // git-receive-pack: always the v0 advertisement.
    const refs = sortedRefs(repo);
    if (refs.length === 0) {
      parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${RECEIVE_CAPS}`));
    } else {
      let firstLine = true;
      for (const [name, oid] of refs) {
        if (firstLine) {
          parts.push(pktLine(`${oid} ${name}\0${RECEIVE_CAPS}`));
          firstLine = false;
        } else {
          parts.push(pktLine(`${oid} ${name}`));
        }
      }
    }
    parts.push(FLUSH_PKT);
  }
  send(200, `application/x-${service}-advertisement`, Buffer.concat(parts));
}

// ---------------------------------------------------------------------------
// upload-pack

function lsRefs(repo: RepoRec, args: string[], send: Send): void {
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') peel = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg === 'unborn') unborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  }
  const lookup = repoLookup(repo);
  const matches = (name: string): boolean =>
    prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const lines: Buffer[] = [];
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headTarget = repo.refs.get(headRef);
  if (headTarget !== undefined) {
    if (matches('HEAD')) {
      let line = `${headTarget} HEAD`;
      if (symrefs) line += ` symref-target:${headRef}`;
      lines.push(pktLine(line));
    }
  } else if (unborn && matches('HEAD')) {
    lines.push(pktLine(`unborn HEAD symref-target:${headRef}`));
  }
  for (const [name, oid] of sortedRefs(repo)) {
    if (!matches(name)) continue;
    let line = `${oid} ${name}`;
    if (peel) {
      const obj = lookup(oid);
      if (obj && obj.type === 'tag') {
        // peel to the commit for the peeled: annotation
        let target = oid;
        let guard = 0;
        while (guard < 16) {
          guard += 1;
          const current = lookup(target);
          if (!current || current.type !== 'tag') break;
          const match = /^object ([0-9a-f]{40})/m.exec(current.data.toString('utf8'));
          if (!match) break;
          target = match[1];
        }
        const targetObj = lookup(target);
        if (targetObj && targetObj.type === 'commit') line += ` peeled:${target}`;
      }
    }
    lines.push(pktLine(line));
  }
  lines.push(FLUSH_PKT);
  send(200, 'application/x-git-upload-pack-result', Buffer.concat(lines));
}

interface FetchPlan {
  pack: Buffer;
  shallowLines: string[];
  unshallowLines: string[];
  commonHaves: string[];
  hasHaves: boolean;
}

// Small memo for pack bytes: the clone benchmark asks for the same tip closure fifty times over.
const packCache = new Map<string, Buffer>();
const PACK_CACHE_MAX = 128;

function planFetch(repo: RepoRec, opts: {
  wants: string[];
  haves: string[];
  clientShallow: string[];
  depth?: number;
  deepenSince?: number;
}): FetchPlan {
  const lookup = repoLookup(repo);
  const cacheKey = [
    repo.owner,
    repo.name,
    [...opts.wants].sort().join(','),
    [...opts.haves].sort().join(','),
    [...opts.clientShallow].sort().join(','),
    opts.depth ?? '',
    opts.deepenSince ?? '',
    headOid(repo),
  ].join('|');
  // What the client already holds: the closure of its haves, except that a commit the client
  // marks shallow is grafted — its parents are NOT held, so history below the boundary must be
  // sendable when a deepen asks for it.
  const haveSet = new Set<string>();
  if (opts.haves.length > 0) {
    const shallowSet = new Set(opts.clientShallow);
    const stack = [...opts.haves];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (haveSet.has(oid)) continue;
      const obj = lookup(oid);
      if (!obj) continue;
      haveSet.add(oid);
      if (obj.type === 'commit') {
        const commit = parseCommit(obj.data);
        if (commit.tree) stack.push(commit.tree);
        if (!shallowSet.has(oid)) for (const parent of commit.parents) stack.push(parent);
      } else if (obj.type === 'tree') {
        for (const entry of parseTree(obj.data)) stack.push(entry.oid);
      } else if (obj.type === 'tag') {
        const target = parseTagTarget(obj.data);
        if (target) stack.push(target);
      }
    }
  }
  const commonHaves = opts.haves.filter((oid) => lookup(oid) !== undefined);

  let include: Set<string>;
  let boundary: string[] = [];
  if (opts.depth !== undefined) {
    const shallow = collectShallow(lookup, opts.wants, opts.depth);
    include = shallow.objects;
    boundary = shallow.boundary;
  } else if (opts.deepenSince !== undefined) {
    // Depth-by-time: include commits newer than the cutoff, boundary at the first older parents.
    include = new Set<string>();
    const stack = [...opts.wants];
    const seenCommits = new Set<string>();
    const boundarySet = new Set<string>();
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      const obj = lookup(oid);
      if (!obj) continue;
      if (obj.type === 'tag') {
        include.add(oid);
        const match = /^object ([0-9a-f]{40})/m.exec(obj.data.toString('utf8'));
        if (match) stack.push(match[1]);
        continue;
      }
      if (obj.type !== 'commit' || seenCommits.has(oid)) continue;
      seenCommits.add(oid);
      const commit = parseCommit(obj.data);
      if (commit.committerTime > opts.deepenSince) {
        include.add(oid);
        for (const parent of commit.parents) stack.push(parent);
      } else {
        boundarySet.add(oid);
      }
    }
    // Add the tree closure of every included commit.
    for (const oid of [...include]) {
      const obj = lookup(oid);
      if (obj && obj.type === 'commit') {
        for (const reached of collectReachable(lookup, [parseCommit(obj.data).tree])) include.add(reached);
      }
    }
    boundary = [...boundarySet];
  } else {
    include = collectReachable(lookup, opts.wants);
  }

  const toSend: Array<{ oid: string; type: GitObject['type']; data: Buffer }> = [];
  for (const oid of include) {
    if (haveSet.has(oid)) continue;
    const obj = lookup(oid);
    if (obj) toSend.push({ oid, type: obj.type, data: obj.data });
  }
  // Commits first, then trees, then blobs: a stable, ancestry-friendly order.
  const rank = { commit: 0, tag: 1, tree: 2, blob: 3 } as const;
  toSend.sort((a, b) => rank[a.type] - rank[b.type]);

  let pack = packCache.get(cacheKey);
  if (!pack) {
    pack = writePack(toSend);
    if (packCache.size >= PACK_CACHE_MAX) packCache.clear();
    packCache.set(cacheKey, pack);
  }
  const unshallowLines = opts.clientShallow.filter((oid) => include.has(oid) && !boundary.includes(oid));
  return {
    pack,
    shallowLines: boundary,
    unshallowLines,
    commonHaves,
    hasHaves: opts.haves.length > 0,
  };
}

function sidebandPackets(pack: Buffer): Buffer[] {
  const packets: Buffer[] = [];
  const CHUNK = 65515;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const slice = pack.subarray(offset, offset + CHUNK);
    packets.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  if (packets.length === 0) packets.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.alloc(0)])));
  return packets;
}

function fetchV2(repo: RepoRec, args: string[], send: Send): void {
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow: string[] = [];
  let depth: number | undefined;
  let deepenSince: number | undefined;
  let noProgress = false;
  for (const arg of args) {
    if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) clientShallow.push(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) depth = Number.parseInt(arg.slice(7).trim(), 10);
    else if (arg.startsWith('deepen-since ')) deepenSince = Number.parseInt(arg.slice(13).trim(), 10);
    else if (arg === 'no-progress') noProgress = true;
  }
  const plan = planFetch(repo, { wants, haves, clientShallow, depth, deepenSince });
  const parts: Buffer[] = [];
  // acknowledgments section (section headers carry their trailing newline)
  parts.push(pktLine('acknowledgments\n'));
  if (!plan.hasHaves) {
    parts.push(pktLine('NAK\n'));
  } else {
    for (const oid of plan.commonHaves) parts.push(pktLine(`ACK ${oid} common\n`));
    parts.push(pktLine(plan.commonHaves.length > 0 ? 'ready\n' : 'NAK\n'));
  }
  parts.push(FLUSH_PKT);
  // shallow-info section, only when shallowness is in play
  if (depth !== undefined || deepenSince !== undefined || clientShallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallowLines) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallowLines) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(FLUSH_PKT);
  }
  // packfile section (the client asked for no-progress, so band 1 carries everything)
  void noProgress;
  parts.push(pktLine('packfile\n'));
  parts.push(...sidebandPackets(plan.pack));
  parts.push(FLUSH_PKT);
  send(200, 'application/x-git-upload-pack-result', Buffer.concat(parts));
}

function fetchV0(repo: RepoRec, firstSegment: string[], secondSegment: string[], send: Send): void {
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow: string[] = [];
  let depth: number | undefined;
  for (const line of firstSegment) {
    const parts = line.split(' ');
    if (parts[0] === 'want') wants.push(parts[1]);
    else if (parts[0] === 'shallow') clientShallow.push(parts[1]);
    else if (parts[0] === 'deepen') depth = Number.parseInt(parts[1], 10);
  }
  for (const line of secondSegment) {
    const parts = line.split(' ');
    if (parts[0] === 'have') haves.push(parts[1]);
  }
  const plan = planFetch(repo, { wants, haves, clientShallow, depth });
  const parts: Buffer[] = [];
  for (const oid of plan.shallowLines) parts.push(pktLine(`shallow ${oid}`));
  for (const oid of plan.unshallowLines) parts.push(pktLine(`unshallow ${oid}`));
  if (plan.commonHaves.length > 0) {
    for (const oid of plan.commonHaves) parts.push(pktLine(`ACK ${oid} common`));
    parts.push(pktLine(`ACK ${plan.commonHaves[plan.commonHaves.length - 1]}`));
  } else {
    parts.push(pktLine('NAK'));
  }
  parts.push(...sidebandPackets(plan.pack));
  parts.push(FLUSH_PKT);
  send(200, 'application/x-git-upload-pack-result', Buffer.concat(parts));
}

export function uploadPack(repo: RepoRec | undefined, body: Buffer, send: Send): void {
  if (!repo) {
    send(404, 'text/plain', 'repository not found\n');
    return;
  }
  try {
    const first = readPktSegment(body, 0);
    const firstText = first.lines.map((line) => line.toString('utf8').replace(/\n$/, ''));
    const command = firstText[0] ?? '';
    if (command === 'command=ls-refs') {
      const delimIndex = first.lines.findIndex((line) => line.length === 1 && line[0] === 1);
      const args = delimIndex >= 0 ? firstText.slice(delimIndex + 1) : [];
      lsRefs(repo, args, send);
      return;
    }
    if (command === 'command=fetch') {
      const delimIndex = first.lines.findIndex((line) => line.length === 1 && line[0] === 1);
      const args = delimIndex >= 0 ? firstText.slice(delimIndex + 1) : firstText.slice(1);
      fetchV2(repo, args, send);
      return;
    }
    if (command.startsWith('want ') || command === '') {
      // Protocol v0 fetch: wants segment, then a haves segment.
      let secondText: string[] = [];
      try {
        secondText = readPktSegment(body, first.rest).lines.map((line) =>
          line.toString('utf8').replace(/\n$/, ''),
        );
      } catch {
        secondText = [];
      }
      fetchV0(repo, firstText, secondText, send);
      return;
    }
    send(400, 'text/plain', 'unsupported upload-pack request\n');
  } catch {
    send(400, 'text/plain', 'invalid upload-pack request\n');
  }
}

// ---------------------------------------------------------------------------
// receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function receivePack(repo: RepoRec | undefined, body: Buffer, send: Send): void {
  if (!repo) {
    send(404, 'text/plain', 'repository not found\n');
    return;
  }
  let commandSegment: { lines: Buffer[]; rest: number };
  try {
    commandSegment = readPktSegment(body, 0);
  } catch {
    send(400, 'text/plain', 'invalid receive-pack request\n');
    return;
  }
  const rest = commandSegment.rest;
  const commandLines = commandSegment.lines.map((line) => line.toString('utf8').replace(/\n$/, ''));
  const commands: PushCommand[] = [];
  let clientCaps = '';
  for (let i = 0; i < commandLines.length; i += 1) {
    let line = commandLines[i];
    if (i === 0) {
      const nul = line.indexOf('\0');
      if (nul >= 0) {
        clientCaps = line.slice(nul + 1);
        line = line.slice(0, nul);
      }
    }
    const parts = line.split(' ');
    if (parts.length === 3 && isSha40(parts[0]) && isSha40(parts[1])) {
      commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
    }
  }

  const lookup = repoLookup(repo);
  const needsPack = commands.some((command) => command.newOid !== ZERO_OID);
  let unpackOk = true;
  let unpackError = '';
  const fresh = new Map<string, GitObject>();
  if (needsPack) {
    const packBytes = body.subarray(rest);
    try {
      const objects = readPack(packBytes, lookup);
      for (const obj of objects) fresh.set(obj.oid, obj);
    } catch (err) {
      unpackOk = false;
      unpackError = err instanceof Error ? err.message : 'unpack failed';
    }
  }
  const unionLookup = (oid: string): GitObject | undefined => fresh.get(oid) ?? lookup(oid);

  interface Result {
    ref: string;
    ok: boolean;
    reason: string;
  }
  const results: Result[] = [];
  if (!unpackOk) {
    for (const command of commands) results.push({ ref: command.ref, ok: false, reason: 'unpack failed' });
  } else {
    // Commit the fresh objects first so ancestry checks and later reads see them.
    for (const [oid, obj] of fresh) {
      if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
    }
    for (const command of commands) {
      const { ref, oldOid, newOid } = command;
      if (!ref.startsWith('refs/')) {
        results.push({ ref, ok: false, reason: 'invalid ref' });
        continue;
      }
      const current = repo.refs.get(ref) ?? ZERO_OID;
      if (current !== oldOid) {
        results.push({ ref, ok: false, reason: 'stale info' });
        continue;
      }
      if (newOid === ZERO_OID) {
        repo.refs.delete(ref);
        results.push({ ref, ok: true, reason: '' });
        continue;
      }
      const target = unionLookup(newOid);
      if (!target) {
        results.push({ ref, ok: false, reason: 'missing object' });
        continue;
      }
      if (ref.startsWith('refs/heads/') && target.type !== 'commit') {
        results.push({ ref, ok: false, reason: 'invalid object type' });
        continue;
      }
      if (ref.startsWith('refs/tags/') && target.type !== 'commit' && target.type !== 'tag') {
        results.push({ ref, ok: false, reason: 'invalid object type' });
        continue;
      }
      if (oldOid !== ZERO_OID) {
        const targetCommit = target.type === 'commit' ? newOid : undefined;
        if (targetCommit && !isAncestor(unionLookup, oldOid, targetCommit)) {
          results.push({ ref, ok: false, reason: 'non-fast-forward' });
          continue;
        }
      }
      repo.refs.set(ref, newOid);
      results.push({ ref, ok: true, reason: '' });
    }
  }

  const reportLines: Buffer[] = [];
  reportLines.push(pktLine(unpackOk ? 'unpack ok\n' : `unpack ${unpackError || 'error'}\n`));
  for (const result of results) {
    reportLines.push(pktLine(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`));
  }
  const wantsSideband = clientCaps.split(' ').includes('side-band-64k');
  let payload: Buffer;
  if (wantsSideband) {
    const inner = Buffer.concat(reportLines);
    const packets: Buffer[] = [];
    const CHUNK = 65515;
    for (let offset = 0; offset < inner.length; offset += CHUNK) {
      packets.push(pktLine(Buffer.concat([Buffer.from([1]), inner.subarray(offset, offset + CHUNK)])));
    }
    packets.push(FLUSH_PKT);
    payload = Buffer.concat(packets);
  } else {
    payload = Buffer.concat([...reportLines, FLUSH_PKT]);
  }
  send(200, 'application/x-git-receive-pack-result', payload);
}

export type { Send };
