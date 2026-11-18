// Domain operations shared by the REST, GraphQL and MCP surfaces: creating issues and pull
// requests, computing a pull's changed files, and squash-merging. One implementation, three
// serializations — that is what keeps the surfaces consistent with each other.

import {
  buildCommit,
  flattenTree,
  hashObject,
  isSha40,
  lineDiffCounts,
  mergeBase,
  mergeTrees,
  parseCommit,
  peelToCommit,
  type Lookup,
} from './gitobj.ts';
import {
  store,
  branchTip,
  repoLookup,
  repoPut,
  VIEWER_EMAIL,
  VIEWER_NAME,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';

// ---------------------------------------------------------------------------
// Ref resolution

/** Resolve a branch name, tag name, full sha or HEAD to a commit oid. */
export function resolveRefToCommit(repo: RepoRec, ref: string): string | undefined {
  const lookup = repoLookup(repo);
  if (ref === 'HEAD') return branchTip(repo, repo.defaultBranch);
  const branch = repo.refs.get(`refs/heads/${ref}`);
  if (branch) return branch;
  const tag = repo.refs.get(`refs/tags/${ref}`);
  if (tag) return peelToCommit(lookup, tag);
  if (isSha40(ref)) {
    const obj = lookup(ref);
    if (obj && obj.type === 'commit') return ref;
    if (obj && obj.type === 'tag') return peelToCommit(lookup, ref);
  }
  return undefined;
}

/** Resolve a ref name to the tree oid of its tip commit. */
export function refTree(repo: RepoRec, refName: string): string | undefined {
  const commit = resolveRefToCommit(repo, refName);
  if (!commit) return undefined;
  const obj = repoLookup(repo)(commit);
  if (!obj) return undefined;
  return parseCommit(obj.data).tree;
}

// ---------------------------------------------------------------------------
// Issues

export function createIssueRecord(
  repo: RepoRec,
  input: { title: string; body: string; labels: string[] },
): IssueRec {
  const now = store.tick();
  const issue: IssueRec = {
    kind: 'issue',
    id: store.allocEntityId(),
    number: repo.nextNumber,
    title: input.title,
    body: input.body,
    state: 'open',
    labels: [...input.labels],
    comments: [],
    createdSeq: store.nextSeq(),
    createdAt: now.iso,
    updatedAt: now.iso,
  };
  repo.nextNumber += 1;
  repo.entries.set(issue.number, issue);
  return issue;
}

// ---------------------------------------------------------------------------
// Pull requests

export type CreatePullResult =
  | { pull: PullRec }
  | { error: 'missing-ref' | 'same-ref' | 'no-commits' | 'duplicate' };

export function createPullRecord(
  repo: RepoRec,
  input: { title: string; body: string; headRef: string; baseRef: string },
): CreatePullResult {
  const headSha = branchTip(repo, input.headRef);
  const baseSha = branchTip(repo, input.baseRef);
  if (!headSha || !baseSha) return { error: 'missing-ref' };
  if (input.headRef === input.baseRef) return { error: 'same-ref' };
  const lookup = repoLookup(repo);
  const base = mergeBase(lookup, baseSha, headSha);
  if (!base || base === headSha) return { error: 'no-commits' };
  for (const entry of repo.entries.values()) {
    if (
      entry.kind === 'pull' &&
      entry.state === 'open' &&
      entry.headRef === input.headRef &&
      entry.baseRef === input.baseRef
    ) {
      return { error: 'duplicate' };
    }
  }
  const now = store.tick();
  const pull: PullRec = {
    kind: 'pull',
    id: store.allocEntityId(),
    number: repo.nextNumber,
    title: input.title,
    body: input.body,
    state: 'open',
    headRef: input.headRef,
    baseRef: input.baseRef,
    headShaSnapshot: headSha,
    baseShaSnapshot: baseSha,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    comments: [],
    createdSeq: store.nextSeq(),
    createdAt: now.iso,
    updatedAt: now.iso,
  };
  repo.nextNumber += 1;
  repo.entries.set(pull.number, pull);
  return { pull };
}

export function pullHeadSha(repo: RepoRec, pull: PullRec): string {
  return branchTip(repo, pull.headRef) ?? pull.headShaSnapshot;
}

export function pullBaseSha(repo: RepoRec, pull: PullRec): string {
  return branchTip(repo, pull.baseRef) ?? pull.baseShaSnapshot;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  sha: string | null;
}

/** The changed-file list between the pull's merge-base tree and its head tree. */
export function pullFiles(repo: RepoRec, pull: PullRec): PullFile[] {
  const lookup = repoLookup(repo);
  const headSha = pullHeadSha(repo, pull);
  const baseSha = pullBaseSha(repo, pull);
  const base = mergeBase(lookup, baseSha, headSha) ?? baseSha;
  const baseObj = lookup(base);
  const headObj = lookup(headSha);
  if (!baseObj || !headObj) return [];
  const baseFlat = flattenTree(lookup, parseCommit(baseObj.data).tree);
  const headFlat = flattenTree(lookup, parseCommit(headObj.data).tree);
  const files: PullFile[] = [];
  const names = new Set([...baseFlat.keys(), ...headFlat.keys()]);
  for (const filename of [...names].sort()) {
    const before = baseFlat.get(filename);
    const after = headFlat.get(filename);
    if (before && !after) {
      const blob = lookup(before.oid);
      const counts = lineDiffCounts(blob ? blob.data.toString('utf8') : '', '');
      files.push({ filename, status: 'removed', additions: 0, deletions: counts.deletions, changes: counts.deletions, sha: null });
    } else if (!before && after) {
      const blob = lookup(after.oid);
      const counts = lineDiffCounts('', blob ? blob.data.toString('utf8') : '');
      files.push({ filename, status: 'added', additions: counts.additions, deletions: 0, changes: counts.additions, sha: after.oid });
    } else if (before && after && before.oid !== after.oid) {
      const oldBlob = lookup(before.oid);
      const newBlob = lookup(after.oid);
      const counts = lineDiffCounts(
        oldBlob ? oldBlob.data.toString('utf8') : '',
        newBlob ? newBlob.data.toString('utf8') : '',
      );
      files.push({
        filename,
        status: 'modified',
        additions: counts.additions,
        deletions: counts.deletions,
        changes: counts.additions + counts.deletions,
        sha: after.oid,
      });
    }
  }
  return files;
}

// ---------------------------------------------------------------------------
// Merging

const mergeableCache = new Map<string, 'MERGEABLE' | 'CONFLICTING'>();

/** Dry-run the three-way merge to report mergeability, memoized per tip pair. */
export function mergeableOf(repo: RepoRec, pull: PullRec): 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN' {
  if (pull.merged) return 'MERGEABLE';
  const headSha = pullHeadSha(repo, pull);
  const baseSha = pullBaseSha(repo, pull);
  const key = `${repo.id}:${baseSha}:${headSha}`;
  const cached = mergeableCache.get(key);
  if (cached) return cached;
  const lookup = repoLookup(repo);
  const base = mergeBase(lookup, baseSha, headSha);
  if (!base) return 'UNKNOWN';
  const baseTree = refTree(repo, base);
  const oursTree = refTree(repo, baseSha);
  const theirsTree = refTree(repo, headSha);
  if (baseTree === undefined || oursTree === undefined || theirsTree === undefined) return 'UNKNOWN';
  // Merge into a scratch overlay so the dry run does not leak objects into the repo store.
  const scratch = new Map<string, { type: 'commit' | 'tree' | 'blob' | 'tag'; data: Buffer }>();
  const scratchLookup: Lookup = (oid) => scratch.get(oid) ?? lookup(oid);
  const scratchPut = (type: 'commit' | 'tree' | 'blob' | 'tag', data: Buffer): string => {
    const oid = hashObject(type, data);
    if (!scratch.has(oid)) scratch.set(oid, { type, data });
    return oid;
  };
  const outcome = mergeTrees(scratchLookup, scratchPut, baseTree, oursTree, theirsTree);
  const status = outcome.conflict ? 'CONFLICTING' : 'MERGEABLE';
  if (mergeableCache.size > 512) mergeableCache.clear();
  mergeableCache.set(key, status);
  return status;
}

export type MergeResult = { ok: true; sha: string } | { ok: false; conflict: boolean };

/** Squash-merge a pull request: one commit on the base branch carrying the merged tree. */
export function mergePullRecord(repo: RepoRec, pull: PullRec): MergeResult {
  if (pull.merged && pull.mergeCommitSha) return { ok: true, sha: pull.mergeCommitSha };
  const lookup = repoLookup(repo);
  const put = repoPut(repo);
  const headSha = pullHeadSha(repo, pull);
  const baseSha = pullBaseSha(repo, pull);
  const base = mergeBase(lookup, baseSha, headSha);
  if (!base) return { ok: false, conflict: true };
  const baseTree = refTree(repo, base);
  const oursTree = refTree(repo, baseSha);
  const theirsTree = refTree(repo, headSha);
  if (baseTree === undefined || oursTree === undefined || theirsTree === undefined) {
    return { ok: false, conflict: true };
  }
  const outcome = mergeTrees(lookup, put, baseTree, oursTree, theirsTree);
  if (outcome.conflict) return { ok: false, conflict: true };
  const now = store.tick();
  const identity = { name: VIEWER_NAME, email: VIEWER_EMAIL, time: now.sec };
  const message = pull.body ? `${pull.title}\n\n${pull.body}` : pull.title;
  const commitData = buildCommit({
    tree: outcome.tree,
    parents: [baseSha],
    author: identity,
    committer: identity,
    message,
  });
  const sha = put('commit', commitData);
  repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
  repo.refsVersion += 1;
  const stamp = store.tick();
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = stamp.iso;
  pull.mergeCommitSha = sha;
  pull.updatedAt = stamp.iso;
  return { ok: true, sha };
}
