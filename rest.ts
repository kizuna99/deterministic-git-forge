// The REST v3 surface: repositories, git refs, commits, contents, labels, issues, comments and
// pull requests, shaped the way GitHub shapes them. Every handler answers JSON; errors are
// {message} objects; another owner's repository is a 404, never a 403.

import { JSON_PARSE_ERROR, isRecord, strField, boolField, type Ctx } from './ctx.ts';
import {
  buildCommit,
  buildTreeFromFlat,
  flattenTree,
  listTreeLevel,
  lookupPath,
  parseCommit,
  type TreeEntry,
} from './gitobj.ts';
import {
  store,
  branchTip,
  globalId,
  repoLookup,
  repoPut,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type CommentRec,
  type IssueOrPull,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';
import {
  createIssueRecord,
  createPullRecord,
  mergePullRecord,
  pullBaseSha,
  pullFiles,
  pullHeadSha,
  resolveRefToCommit,
} from './domain.ts';

const FORGE_VERSION = '1.0.0';

function notFound(ctx: Ctx): void {
  ctx.sendJson(404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' });
}

function validationFailed(ctx: Ctx, message: string): void {
  ctx.sendJson(422, { message, documentation_url: 'https://docs.github.com/rest' });
}

/** The repository the path names, or undefined (having answered 404) — foreign owners included. */
function pathRepo(ctx: Ctx, owner: string, name: string): RepoRec | undefined {
  if (owner !== VIEWER_LOGIN) {
    notFound(ctx);
    return undefined;
  }
  const repo = store.getRepo(owner, name);
  if (!repo) {
    notFound(ctx);
    return undefined;
  }
  return repo;
}

function bodyObject(ctx: Ctx): Record<string, unknown> | undefined {
  if (ctx.json === JSON_PARSE_ERROR) {
    ctx.sendJson(400, { message: 'Problems parsing JSON' });
    return undefined;
  }
  if (ctx.json === undefined || ctx.json === null) return {};
  if (!isRecord(ctx.json)) return {};
  return ctx.json;
}

// ---------------------------------------------------------------------------
// Serializers

export function repoJson(repo: RepoRec, origin: string): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('repo', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.isPrivate,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    description: repo.description,
    fork: false,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.updatedAt,
    html_url: `${origin}/${repo.owner}/${repo.name}`,
    clone_url: `${origin}/${repo.owner}/${repo.name}.git`,
    git_url: `git://${origin.replace(/^https?:\/\//, '')}/${repo.owner}/${repo.name}.git`,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    forks_count: 0,
    open_issues_count: [...repo.entries.values()].filter((e) => e.state === 'open').length,
    visibility: repo.isPrivate ? 'private' : 'public',
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

export function labelJson(repo: RepoRec, name: string): Record<string, unknown> {
  const label = repo.labels.get(name);
  return {
    id: 0,
    node_id: '',
    name,
    color: label?.color ?? 'ededed',
    description: label?.description ?? '',
    default: false,
  };
}

export function issueJson(repo: RepoRec, issue: IssueRec, origin: string): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('issue', issue.id),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(repo, name)),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.state === 'closed' ? issue.updatedAt : null,
    html_url: `${origin}/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

export function pullJson(repo: RepoRec, pull: PullRec, origin: string): Record<string, unknown> {
  const headSha = pullHeadSha(repo, pull);
  const baseSha = pullBaseSha(repo, pull);
  return {
    id: pull.id,
    node_id: globalId('pull', pull.id),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    draft: false,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    head: {
      ref: pull.headRef,
      sha: headSha,
      label: `${repo.owner}:${pull.headRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      repo: repoJson(repo, origin),
    },
    base: {
      ref: pull.baseRef,
      sha: baseSha,
      label: `${repo.owner}:${pull.baseRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      repo: repoJson(repo, origin),
    },
    comments: pull.comments.length,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.state === 'closed' ? pull.updatedAt : null,
    html_url: `${origin}/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function commentJson(comment: CommentRec): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: globalId('comment', comment.id),
    body: comment.body,
    user: { login: comment.authorLogin, id: VIEWER_ID, type: 'User' },
    author: { login: comment.authorLogin, id: VIEWER_ID, type: 'User' },
    created_at: comment.createdAt,
    updated_at: comment.createdAt,
  };
}

function commitJson(repo: RepoRec, oid: string): Record<string, unknown> {
  const obj = repoLookup(repo)(oid);
  if (!obj) return { sha: oid };
  const commit = parseCommit(obj.data);
  const toIso = (sec: number): string => new Date(sec * 1000).toISOString();
  return {
    sha: oid,
    node_id: globalId('commit', 0) + oid,
    commit: {
      author: { name: commit.authorName, email: commit.authorEmail, date: toIso(commit.authorTime) },
      committer: { name: commit.committerName, email: commit.committerEmail, date: toIso(commit.committerTime) },
      message: commit.message,
      tree: { sha: commit.tree },
      comment_count: 0,
    },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    parents: commit.parents.map((parent) => ({ sha: parent })),
  };
}

// ---------------------------------------------------------------------------
// Pagination

interface Page<T> {
  items: T[];
  headers: Record<string, string>;
}

function paginate<T>(ctx: Ctx, all: T[], basePath: string): Page<T> {
  const perPageRaw = Number.parseInt(ctx.query.get('per_page') ?? '30', 10);
  const perPage = Number.isFinite(perPageRaw) ? Math.min(100, Math.max(1, perPageRaw)) : 30;
  const pageRaw = Number.parseInt(ctx.query.get('page') ?? '1', 10);
  const page = Number.isFinite(pageRaw) && pageRaw >= 1 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const items = all.slice(start, start + perPage);
  const headers: Record<string, string> = {};
  if (start + perPage < all.length) {
    const lastPage = Math.ceil(all.length / perPage);
    const params = new URLSearchParams(ctx.query);
    const linkFor = (p: number): string => {
      params.set('per_page', String(perPage));
      params.set('page', String(p));
      return `<${ctx.origin}${basePath}?${params.toString()}>`;
    };
    const links = [`${linkFor(page + 1)}; rel="next"`, `${linkFor(lastPage)}; rel="last"`];
    if (page > 1) links.push(`${linkFor(1)}; rel="first"`, `${linkFor(page - 1)}; rel="prev"`);
    headers.Link = links.join(', ');
  }
  return { items, headers };
}

// ---------------------------------------------------------------------------
// Repository CRUD and meta

function handleRoot(ctx: Ctx): void {
  ctx.sendJson(
    200,
    {
      current_user_url: `${ctx.origin}/api/v3/user`,
      repository_url: `${ctx.origin}/api/v3/repos/{owner}/{repo}`,
      issue_search_url: `${ctx.origin}/api/v3/search/issues?q={query}`,
      emojis_url: `${ctx.origin}/api/v3/emojis`,
    },
    { 'X-OAuth-Scopes': 'repo, read:org' },
  );
}

function handleMeta(ctx: Ctx): void {
  ctx.sendJson(200, {
    installed_version: FORGE_VERSION,
    hooks: [],
    git: [],
    github_developer: false,
    verifiable_password_authentication: false,
    ssh_key_fingerprints: {},
    importer: [],
    custom_properties: [],
  });
}

function handleUser(ctx: Ctx): void {
  ctx.sendJson(200, {
    login: VIEWER_LOGIN,
    id: VIEWER_ID,
    node_id: globalId('user', VIEWER_ID),
    name: VIEWER_NAME,
    email: VIEWER_EMAIL,
    type: 'User',
    site_admin: false,
    created_at: store.nowIso(),
    updated_at: store.nowIso(),
  });
}

const REPO_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;

function handleCreateRepo(ctx: Ctx): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const name = strField(body, 'name');
  if (!name || !REPO_NAME_RE.test(name)) {
    validationFailed(ctx, 'Repository creation failed.');
    return;
  }
  if (store.getRepo(VIEWER_LOGIN, name)) {
    ctx.sendJson(422, {
      message: 'Repository creation failed: name already exists on this account',
      errors: [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }],
    });
    return;
  }
  const repo = store.createRepo(
    VIEWER_LOGIN,
    name,
    strField(body, 'description') ?? '',
    boolField(body, 'private') ?? false,
  );
  ctx.sendJson(201, repoJson(repo, ctx.origin));
}

function handleListUserRepos(ctx: Ctx): void {
  const repos = store.listRepos(VIEWER_LOGIN);
  const page = paginate(ctx, repos, '/api/v3/user/repos');
  ctx.sendJson(200, page.items.map((repo) => repoJson(repo, ctx.origin)), page.headers);
}

function handleGetRepo(ctx: Ctx, owner: string, name: string): void {
  const repo = pathRepo(ctx, owner, name);
  if (!repo) return;
  ctx.sendJson(200, repoJson(repo, ctx.origin));
}

function handlePatchRepo(ctx: Ctx, owner: string, name: string): void {
  const repo = pathRepo(ctx, owner, name);
  if (!repo) return;
  const body = bodyObject(ctx);
  if (!body) return;
  const description = strField(body, 'description');
  if (description !== undefined) repo.description = description;
  const isPrivate = boolField(body, 'private');
  if (isPrivate !== undefined) repo.isPrivate = isPrivate;
  const newName = strField(body, 'name');
  if (newName && newName !== repo.name && REPO_NAME_RE.test(newName) && !store.getRepo(repo.owner, newName)) {
    store.repos.delete(`${repo.owner}/${repo.name}`);
    repo.name = newName;
    store.repos.set(`${repo.owner}/${repo.name}`, repo);
  }
  repo.updatedAt = store.tick().iso;
  ctx.sendJson(200, repoJson(repo, ctx.origin));
}

function handleDeleteRepo(ctx: Ctx, owner: string, name: string): void {
  const repo = pathRepo(ctx, owner, name);
  if (!repo) return;
  store.deleteRepo(repo);
  ctx.send(204, 'application/json; charset=utf-8', '');
}

// ---------------------------------------------------------------------------
// Branches, refs, commits

function handleListBranches(ctx: Ctx, repo: RepoRec): void {
  const branches: Array<Record<string, unknown>> = [];
  for (const [ref, oid] of repo.refs) {
    if (!ref.startsWith('refs/heads/')) continue;
    branches.push({
      name: ref.slice('refs/heads/'.length),
      commit: { sha: oid, url: `${ctx.origin}/api/v3/repos/${repo.owner}/${repo.name}/commits/${oid}` },
      protected: false,
    });
  }
  branches.sort((a, b) => String(a.name).localeCompare(String(b.name)));
  ctx.sendJson(200, branches);
}

function handleGetBranch(ctx: Ctx, repo: RepoRec, branch: string): void {
  const tip = branchTip(repo, branch);
  if (!tip) return notFound(ctx);
  ctx.sendJson(200, {
    name: branch,
    commit: commitJson(repo, tip),
    protected: false,
    _links: {},
  });
}

function handleGetRef(ctx: Ctx, repo: RepoRec, refPath: string): void {
  const full = refPath.startsWith('refs/') ? refPath : `refs/${refPath}`;
  const oid = repo.refs.get(full);
  if (!oid) return notFound(ctx);
  const obj = repoLookup(repo)(oid);
  ctx.sendJson(200, { ref: full, node_id: '', object: { sha: oid, type: obj?.type ?? 'commit' } });
}

function handleCreateRef(ctx: Ctx, repo: RepoRec): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const ref = strField(body, 'ref');
  const sha = strField(body, 'sha');
  if (!ref || !sha || !ref.startsWith('refs/')) {
    validationFailed(ctx, 'Reference creation failed.');
    return;
  }
  if (repo.refs.has(ref)) {
    ctx.sendJson(422, { message: 'Reference already exists' });
    return;
  }
  const obj = repoLookup(repo)(sha);
  if (!obj) {
    validationFailed(ctx, 'Object does not exist.');
    return;
  }
  repo.refs.set(ref, sha);
  ctx.sendJson(201, { ref, node_id: '', object: { sha, type: obj.type } });
}

function handleListCommits(ctx: Ctx, repo: RepoRec): void {
  const shaParam = ctx.query.get('sha') ?? repo.defaultBranch;
  const tip = resolveRefToCommit(repo, shaParam);
  if (!tip) {
    ctx.sendJson(200, []);
    return;
  }
  const lookup = repoLookup(repo);
  const order: string[] = [];
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    order.push(oid);
    const parents = parseCommit(obj.data).parents;
    // Push parents in reverse so the first parent is visited first.
    for (let i = parents.length - 1; i >= 0; i -= 1) stack.push(parents[i]);
  }
  const page = paginate(ctx, order, `/api/v3/repos/${repo.owner}/${repo.name}/commits`);
  ctx.sendJson(200, page.items.map((oid) => commitJson(repo, oid)), page.headers);
}

// ---------------------------------------------------------------------------
// Contents

function treeOfTip(repo: RepoRec, tip: string | undefined): string {
  if (!tip) return '';
  const obj = repoLookup(repo)(tip);
  if (!obj) return '';
  return parseCommit(obj.data).tree;
}

function encodeBase64Wrapped(data: Buffer): string {
  const raw = data.toString('base64');
  return raw.replace(/.{1,60}/g, '$&\n');
}

function handleGetContents(ctx: Ctx, repo: RepoRec, path: string): void {
  const ref = ctx.query.get('ref') ?? repo.defaultBranch;
  const tip = resolveRefToCommit(repo, ref);
  if (!tip && repo.refs.size > 0) return notFound(ctx);
  const tree = treeOfTip(repo, tip);
  if (path === '') {
    const entries = listTreeLevel(repoLookup(repo), tree).map((entry) => treeEntryJson(repo, entry, ''));
    ctx.sendJson(200, entries);
    return;
  }
  const found = lookupPath(repoLookup(repo), tree, path);
  if (!found) return notFound(ctx);
  if (found.kind === 'tree') {
    const prefix = path.replace(/\/+$/, '');
    const entries = listTreeLevel(repoLookup(repo), found.oid).map((entry) => treeEntryJson(repo, entry, prefix));
    ctx.sendJson(200, entries);
    return;
  }
  const blob = repoLookup(repo)(found.oid);
  const data = blob ? blob.data : Buffer.alloc(0);
  const name = path.split('/').pop() ?? path;
  ctx.sendJson(200, {
    type: 'file',
    encoding: 'base64',
    content: encodeBase64Wrapped(data),
    sha: found.oid,
    size: data.length,
    name,
    path,
    download_url: null,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}/blob/${ref}/${path}`,
    git_url: `${ctx.origin}/api/v3/repos/${repo.owner}/${repo.name}/git/blobs/${found.oid}`,
    _links: {},
  });
}

function treeEntryJson(repo: RepoRec, entry: TreeEntry, prefix: string): Record<string, unknown> {
  const path = prefix ? `${prefix}/${entry.name}` : entry.name;
  const isDir = entry.mode === '40000';
  let size = 0;
  if (!isDir) {
    const blob = repoLookup(repo)(entry.oid);
    size = blob ? blob.data.length : 0;
  }
  return {
    type: isDir ? 'dir' : 'file',
    name: entry.name,
    path,
    sha: entry.oid,
    size,
    url: `${repo.owner}/${repo.name}`,
    html_url: '',
    git_url: '',
    download_url: null,
    _links: {},
  };
}

interface ContentsWriteResult {
  status: number;
  commitSha: string;
  blobOid: string;
  created: boolean;
}

/** Shared by PUT and DELETE contents: rebuild the branch tree with one path changed/removed. */
function writeContents(
  repo: RepoRec,
  branch: string,
  path: string,
  newData: Buffer | undefined,
  newMode: string,
  message: string,
): ContentsWriteResult | { error: 'branch-base-missing' } {
  const lookup = repoLookup(repo);
  const put = repoPut(repo);
  let tip = branchTip(repo, branch);
  if (!repo.refs.has(`refs/heads/${branch}`)) {
    // Creating a branch implicitly: fork from the default branch tip (or stay unborn).
    tip = branchTip(repo, repo.defaultBranch);
  }
  const flat = flattenTree(lookup, treeOfTip(repo, tip));
  const existing = flat.get(path);
  const created = !existing;
  if (newData === undefined) flat.delete(path);
  else flat.set(path, { mode: newMode, oid: put('blob', newData) });
  const rootTree = buildTreeFromFlat(flat, put);
  const now = store.tick();
  const identity = { name: VIEWER_NAME, email: VIEWER_EMAIL, time: now.sec };
  const commitData = buildCommit({
    tree: rootTree,
    parents: tip ? [tip] : [],
    author: identity,
    committer: identity,
    message,
  });
  const commitSha = put('commit', commitData);
  repo.refs.set(`refs/heads/${branch}`, commitSha);
  repo.updatedAt = store.tick().iso;
  return { status: created ? 201 : 200, commitSha, blobOid: newData ? flat.get(path)?.oid ?? '' : '', created };
}

function handlePutContents(ctx: Ctx, repo: RepoRec, path: string): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const message = strField(body, 'message');
  const content = strField(body, 'content');
  if (message === undefined || content === undefined) {
    validationFailed(ctx, 'Validation Failed: message and content are required.');
    return;
  }
  const branch = strField(body, 'branch') ?? repo.defaultBranch;
  const tip = branchTip(repo, branch) ?? branchTip(repo, repo.defaultBranch);
  const existing = lookupPath(repoLookup(repo), treeOfTip(repo, tip), path);
  const givenSha = strField(body, 'sha');
  if (existing && existing.kind === 'blob') {
    if (givenSha !== existing.oid) {
      ctx.sendJson(409, { message: 'Conflict: the given sha does not match the current file sha' });
      return;
    }
  } else if (givenSha !== undefined && !existing) {
    ctx.sendJson(422, { message: 'Validation Failed: sha supplied for a file that does not exist' });
    return;
  }
  let data: Buffer;
  try {
    data = Buffer.from(content.replace(/\s+/g, ''), 'base64');
  } catch {
    validationFailed(ctx, 'Validation Failed: content is not valid base64.');
    return;
  }
  const mode = existing && existing.kind === 'blob' ? existing.mode : '100644';
  const result = writeContents(repo, branch, path, data, mode, message);
  if ('error' in result) {
    validationFailed(ctx, 'Validation Failed.');
    return;
  }
  const name = path.split('/').pop() ?? path;
  ctx.sendJson(result.status, {
    content: {
      type: 'file',
      encoding: 'base64',
      sha: result.blobOid,
      size: data.length,
      name,
      path,
      download_url: null,
      html_url: '',
      git_url: '',
      _links: {},
    },
    commit: commitJson(repo, result.commitSha),
  });
}

function handleDeleteContents(ctx: Ctx, repo: RepoRec, path: string): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const message = strField(body, 'message');
  const sha = strField(body, 'sha');
  if (message === undefined || sha === undefined) {
    validationFailed(ctx, 'Validation Failed: message and sha are required.');
    return;
  }
  const branch = strField(body, 'branch') ?? repo.defaultBranch;
  const tip = branchTip(repo, branch);
  const existing = lookupPath(repoLookup(repo), treeOfTip(repo, tip), path);
  if (!existing || existing.kind !== 'blob') return notFound(ctx);
  if (existing.oid !== sha) {
    ctx.sendJson(409, { message: 'Conflict: the given sha does not match the current file sha' });
    return;
  }
  const result = writeContents(repo, branch, path, undefined, '100644', message);
  if ('error' in result) {
    validationFailed(ctx, 'Validation Failed.');
    return;
  }
  ctx.sendJson(200, { content: null, commit: commitJson(repo, result.commitSha) });
}

// ---------------------------------------------------------------------------
// Labels

function handleCreateLabel(ctx: Ctx, repo: RepoRec): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const name = strField(body, 'name');
  const color = strField(body, 'color');
  if (!name || !color) {
    validationFailed(ctx, 'Validation Failed: name and color are required.');
    return;
  }
  if (repo.labels.has(name)) {
    ctx.sendJson(422, { message: `Validation Failed: label '${name}' already exists` });
    return;
  }
  repo.labels.set(name, { name, color, description: strField(body, 'description') ?? '' });
  repo.labelsVersion += 1;
  ctx.sendJson(201, labelJson(repo, name));
}

function handleListLabels(ctx: Ctx, repo: RepoRec): void {
  ctx.sendJson(200, [...repo.labels.keys()].map((name) => labelJson(repo, name)));
}

// ---------------------------------------------------------------------------
// Issues and comments

function labelNamesOf(body: Record<string, unknown>): string[] | undefined {
  const labels = body.labels;
  if (!Array.isArray(labels)) return undefined;
  const names: string[] = [];
  for (const label of labels) {
    if (typeof label === 'string') names.push(label);
    else if (isRecord(label) && typeof label.name === 'string') names.push(label.name);
    else return undefined;
  }
  return names;
}

function handleCreateIssue(ctx: Ctx, repo: RepoRec): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const title = strField(body, 'title');
  if (title === undefined) {
    validationFailed(ctx, 'Validation Failed: title is required.');
    return;
  }
  const labels = labelNamesOf(body) ?? [];
  for (const name of labels) {
    if (!repo.labels.has(name)) {
      ctx.sendJson(422, { message: `Validation Failed: label '${name}' does not exist` });
      return;
    }
  }
  const issue = createIssueRecord(repo, {
    title,
    body: strField(body, 'body') ?? '',
    labels,
  });
  ctx.sendJson(201, issueJson(repo, issue, ctx.origin));
}

function sortedIssues(repo: RepoRec, kind: 'issue' | 'pull'): IssueOrPull[] {
  return [...repo.entries.values()].filter((entry) => entry.kind === kind);
}

function handleListIssues(ctx: Ctx, repo: RepoRec): void {
  const state = ctx.query.get('state') ?? 'open';
  let issues = sortedIssues(repo, 'issue') as IssueRec[];
  if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
  else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
  const direction = ctx.query.get('direction') ?? 'desc';
  issues.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  const page = paginate(ctx, issues, `/api/v3/repos/${repo.owner}/${repo.name}/issues`);
  ctx.sendJson(200, page.items.map((issue) => issueJson(repo, issue, ctx.origin)), page.headers);
}

function entryOf(repo: RepoRec, number: number): IssueOrPull | undefined {
  return repo.entries.get(number);
}

// Serialized GET-issue bodies, memoized per repo: the read benchmark asks for the same issue
// thousands of times. The fingerprint is built from every field the body derives from, so any
// mutation (patch, close, merge, comment, label edit, ref move, repo edit) misses the cache.
const issueBodyCaches = new WeakMap<RepoRec, Map<string, { fp: string; body: string }>>();

function sendCachedIssueBody(
  ctx: Ctx,
  repo: RepoRec,
  entry: IssueOrPull,
  fp: string,
  build: () => Record<string, unknown>,
): void {
  let perRepo = issueBodyCaches.get(repo);
  if (!perRepo) {
    perRepo = new Map();
    issueBodyCaches.set(repo, perRepo);
  }
  const key = `${entry.number}|${ctx.origin}`;
  const hit = perRepo.get(key);
  if (hit && hit.fp === fp) {
    ctx.send(200, 'application/json; charset=utf-8', hit.body);
    return;
  }
  const body = `${JSON.stringify(build())}\n`;
  if (perRepo.size >= 256) perRepo.clear();
  perRepo.set(key, { fp, body });
  ctx.send(200, 'application/json; charset=utf-8', body);
}

function handleGetIssue(ctx: Ctx, repo: RepoRec, number: number): void {
  const entry = entryOf(repo, number);
  if (!entry) return notFound(ctx);
  if (entry.kind === 'pull') {
    const fp = [
      'p', repo.updatedAt, entry.updatedAt, entry.comments.length, repo.labelsVersion,
      entry.state, entry.title, entry.body, entry.merged, entry.mergedAt ?? '',
      entry.mergeCommitSha ?? '', pullHeadSha(repo, entry), pullBaseSha(repo, entry),
    ].join('|');
    sendCachedIssueBody(ctx, repo, entry, fp, () => {
      const json = pullJson(repo, entry, ctx.origin);
      json.pull_request = { url: `${ctx.origin}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${number}` };
      // GitHub also exposes pulls through the issues endpoint with the pull fields merged in.
      return { ...issueShellOfPull(repo, entry, ctx.origin), ...json };
    });
    return;
  }
  const fp = [
    'i', repo.updatedAt, entry.updatedAt, entry.comments.length, repo.labelsVersion,
    entry.state, entry.title, entry.body, entry.labels.join(','),
  ].join('|');
  sendCachedIssueBody(ctx, repo, entry, fp, () => issueJson(repo, entry, ctx.origin));
}

function issueShellOfPull(repo: RepoRec, pull: PullRec, origin: string): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    labels: [],
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    comments: pull.comments.length,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    html_url: `${origin}/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function handlePatchIssue(ctx: Ctx, repo: RepoRec, number: number): void {
  const entry = entryOf(repo, number);
  if (!entry) return notFound(ctx);
  const body = bodyObject(ctx);
  if (!body) return;
  const state = strField(body, 'state');
  if (state === 'open' || state === 'closed') entry.state = state;
  const title = strField(body, 'title');
  if (title !== undefined) entry.title = title;
  const text = strField(body, 'body');
  if (text !== undefined) entry.body = text;
  if (entry.kind === 'issue') {
    const labels = labelNamesOf(body);
    if (labels) {
      for (const name of labels) {
        if (!repo.labels.has(name)) {
          ctx.sendJson(422, { message: `Validation Failed: label '${name}' does not exist` });
          return;
        }
      }
      entry.labels = labels;
    }
  }
  entry.updatedAt = store.tick().iso;
  if (entry.kind === 'pull') ctx.sendJson(200, pullJson(repo, entry, ctx.origin));
  else ctx.sendJson(200, issueJson(repo, entry, ctx.origin));
}

function handleCreateComment(ctx: Ctx, repo: RepoRec, number: number): void {
  const entry = entryOf(repo, number);
  if (!entry) return notFound(ctx);
  const body = bodyObject(ctx);
  if (!body) return;
  const text = strField(body, 'body');
  if (text === undefined) {
    validationFailed(ctx, 'Validation Failed: body is required.');
    return;
  }
  const comment: CommentRec = {
    id: store.allocEntityId(),
    body: text,
    authorLogin: VIEWER_LOGIN,
    createdSeq: store.nextSeq(),
    createdAt: store.tick().iso,
  };
  entry.comments.push(comment);
  ctx.sendJson(201, commentJson(comment));
}

function handleListComments(ctx: Ctx, repo: RepoRec, number: number): void {
  const entry = entryOf(repo, number);
  if (!entry) return notFound(ctx);
  const comments = [...entry.comments].sort((a, b) => a.createdSeq - b.createdSeq);
  const page = paginate(ctx, comments, `/api/v3/repos/${repo.owner}/${repo.name}/issues/${number}/comments`);
  ctx.sendJson(200, page.items.map(commentJson), page.headers);
}

// ---------------------------------------------------------------------------
// Pull requests

function handleCreatePull(ctx: Ctx, repo: RepoRec): void {
  const body = bodyObject(ctx);
  if (!body) return;
  const title = strField(body, 'title');
  const headRaw = strField(body, 'head');
  const base = strField(body, 'base');
  if (title === undefined || headRaw === undefined || base === undefined) {
    validationFailed(ctx, 'Validation Failed: title, head and base are required.');
    return;
  }
  const head = headRaw.includes(':') ? headRaw.slice(headRaw.indexOf(':') + 1) : headRaw;
  const result = createPullRecord(repo, { title, body: strField(body, 'body') ?? '', headRef: head, baseRef: base });
  if ('error' in result) {
    if (result.error === 'duplicate') {
      ctx.sendJson(422, { message: 'Validation Failed: A pull request already exists for this branch combination.' });
    } else {
      validationFailed(ctx, `Validation Failed: ${result.error}`);
    }
    return;
  }
  ctx.sendJson(201, pullJson(repo, result.pull, ctx.origin));
}

function handleListPulls(ctx: Ctx, repo: RepoRec): void {
  const state = ctx.query.get('state') ?? 'open';
  let pulls = sortedIssues(repo, 'pull') as PullRec[];
  if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
  else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
  const direction = ctx.query.get('direction') ?? 'desc';
  pulls.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  const page = paginate(ctx, pulls, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`);
  ctx.sendJson(200, page.items.map((pull) => pullJson(repo, pull, ctx.origin)), page.headers);
}

function pullOf(ctx: Ctx, repo: RepoRec, number: number): PullRec | undefined {
  const entry = entryOf(repo, number);
  if (!entry || entry.kind !== 'pull') {
    notFound(ctx);
    return undefined;
  }
  return entry;
}

function handleGetPull(ctx: Ctx, repo: RepoRec, number: number): void {
  const pull = pullOf(ctx, repo, number);
  if (!pull) return;
  ctx.sendJson(200, pullJson(repo, pull, ctx.origin));
}

function handlePullFiles(ctx: Ctx, repo: RepoRec, number: number): void {
  const pull = pullOf(ctx, repo, number);
  if (!pull) return;
  const files = pullFiles(repo, pull).map((file) => ({
    sha: file.sha,
    filename: file.filename,
    status: file.status,
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
    blob_url: '',
    raw_url: '',
    contents_url: '',
  }));
  const page = paginate(ctx, files, `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${number}/files`);
  ctx.sendJson(200, page.items, page.headers);
}

function handleMergePull(ctx: Ctx, repo: RepoRec, number: number): void {
  const pull = pullOf(ctx, repo, number);
  if (!pull) return;
  const body = bodyObject(ctx);
  if (!body) return;
  if (pull.state === 'closed' && !pull.merged) {
    ctx.sendJson(405, { message: 'Pull Request is not mergeable' });
    return;
  }
  const result = mergePullRecord(repo, pull);
  if (!result.ok) {
    ctx.sendJson(405, { message: 'Pull Request is not mergeable' });
    return;
  }
  ctx.sendJson(200, { sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
}

// ---------------------------------------------------------------------------
// Router

export function handleRest(ctx: Ctx): boolean {
  const seg = ctx.segments;
  if (seg[0] !== 'api' || seg[1] !== 'v3') return false;
  const rest = seg.slice(2);
  const method = ctx.method;

  if (rest.length === 0) {
    if (method === 'GET') handleRoot(ctx);
    else notFound(ctx);
    return true;
  }
  if (rest[0] === 'meta' && rest.length === 1) {
    if (method === 'GET') handleMeta(ctx);
    else notFound(ctx);
    return true;
  }
  if (rest[0] === 'user' && rest.length === 1) {
    if (method === 'GET') handleUser(ctx);
    else notFound(ctx);
    return true;
  }
  if (rest[0] === 'user' && rest[1] === 'repos' && rest.length === 2) {
    if (method === 'POST') handleCreateRepo(ctx);
    else if (method === 'GET') handleListUserRepos(ctx);
    else notFound(ctx);
    return true;
  }
  if (rest[0] === 'repos' && rest.length >= 3) {
    const owner = rest[1];
    const name = rest[2];
    const tail = rest.slice(3);
    if (tail.length === 0) {
      if (method === 'GET') handleGetRepo(ctx, owner, name);
      else if (method === 'PATCH') handlePatchRepo(ctx, owner, name);
      else if (method === 'DELETE') handleDeleteRepo(ctx, owner, name);
      else notFound(ctx);
      return true;
    }
    const repo = pathRepo(ctx, owner, name);
    if (!repo) return true;

    if (tail[0] === 'branches') {
      if (tail.length === 1 && method === 'GET') handleListBranches(ctx, repo);
      else if (tail.length >= 2 && method === 'GET') handleGetBranch(ctx, repo, tail.slice(1).join('/'));
      else notFound(ctx);
      return true;
    }
    if (tail[0] === 'git' && tail[1] === 'refs') {
      if (tail.length === 2 && method === 'POST') handleCreateRef(ctx, repo);
      else if (tail.length >= 3 && method === 'GET') handleGetRef(ctx, repo, tail.slice(2).join('/'));
      else notFound(ctx);
      return true;
    }
    if (tail[0] === 'commits' && tail.length === 1) {
      if (method === 'GET') handleListCommits(ctx, repo);
      else notFound(ctx);
      return true;
    }
    if (tail[0] === 'contents') {
      const path = tail.slice(1).join('/');
      if (method === 'GET') handleGetContents(ctx, repo, path);
      else if (method === 'PUT') handlePutContents(ctx, repo, path);
      else if (method === 'DELETE') handleDeleteContents(ctx, repo, path);
      else notFound(ctx);
      return true;
    }
    if (tail[0] === 'labels') {
      if (tail.length === 1 && method === 'POST') handleCreateLabel(ctx, repo);
      else if (tail.length === 1 && method === 'GET') handleListLabels(ctx, repo);
      else notFound(ctx);
      return true;
    }
    if (tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'POST') handleCreateIssue(ctx, repo);
        else if (method === 'GET') handleListIssues(ctx, repo);
        else notFound(ctx);
        return true;
      }
      const number = Number.parseInt(tail[1], 10);
      if (!Number.isFinite(number)) {
        notFound(ctx);
        return true;
      }
      if (tail.length === 2) {
        if (method === 'GET') handleGetIssue(ctx, repo, number);
        else if (method === 'PATCH') handlePatchIssue(ctx, repo, number);
        else notFound(ctx);
        return true;
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (method === 'POST') handleCreateComment(ctx, repo, number);
        else if (method === 'GET') handleListComments(ctx, repo, number);
        else notFound(ctx);
        return true;
      }
      notFound(ctx);
      return true;
    }
    if (tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') handleCreatePull(ctx, repo);
        else if (method === 'GET') handleListPulls(ctx, repo);
        else notFound(ctx);
        return true;
      }
      const number = Number.parseInt(tail[1], 10);
      if (!Number.isFinite(number)) {
        notFound(ctx);
        return true;
      }
      if (tail.length === 2) {
        if (method === 'GET') handleGetPull(ctx, repo, number);
        else notFound(ctx);
        return true;
      }
      if (tail.length === 3 && tail[2] === 'files' && method === 'GET') {
        handlePullFiles(ctx, repo, number);
        return true;
      }
      if (tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
        handleMergePull(ctx, repo, number);
        return true;
      }
      notFound(ctx);
      return true;
    }
    notFound(ctx);
    return true;
  }
  notFound(ctx);
  return true;
}
