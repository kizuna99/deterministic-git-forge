// The MCP surface: a stateless JSON-RPC endpoint speaking the modern protocol era (server/discover
// first, legacy initialize tolerated), with six tools that act on the same store as REST and
// GraphQL. Tool results carry both a text content block and structuredContent.

import { isRecord, type Ctx } from './ctx.ts';
import { store, VIEWER_LOGIN, type RepoRec } from './store.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';
import { createIssueRecord, createPullRecord, mergePullRecord } from './domain.ts';

const SERVER_NAME = 'makefun-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user and return it.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository and return it.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch and return it.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch to merge from.' },
        base: { type: 'string', description: 'The base branch to merge into.' },
        body: { type: 'string', description: 'The pull request body.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request and report the merge.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

// Built once so tools/list is byte-identical across calls.
const TOOLS_LIST_JSON = JSON.stringify(TOOLS);
export const toolsListStable = (): string => TOOLS_LIST_JSON;

interface RpcResult {
  status: number;
  payload?: unknown;
}

function rpcError(id: unknown, code: number, message: string): RpcResult {
  return { status: 200, payload: { jsonrpc: '2.0', id: id ?? null, error: { code, message } } };
}

function rpcOk(id: unknown, result: unknown): RpcResult {
  return { status: 200, payload: { jsonrpc: '2.0', id: id ?? null, result } };
}

function toolOk(value: unknown): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(value) }],
    structuredContent: value as Record<string, unknown>,
    isError: false,
  };
}

function toolError(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: message }],
    isError: true,
  };
}

function findRepo(args: Record<string, unknown>): RepoRec | undefined {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const name = typeof args.repo === 'string' ? args.repo : '';
  if (owner !== VIEWER_LOGIN) return undefined;
  return store.getRepo(owner, name);
}

function callTool(ctx: Ctx, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const def = TOOLS.find((tool) => tool.name === name);
  if (!def) return toolError(`Unknown tool: ${name}`);
  for (const required of def.inputSchema.required) {
    if (args[required] === undefined || args[required] === null) {
      return toolError(`Missing required argument: ${required}`);
    }
  }
  switch (name) {
    case 'create_repo': {
      const repoName = String(args.name);
      if (store.getRepo(VIEWER_LOGIN, repoName)) return toolError(`Repository '${repoName}' already exists`);
      const repo = store.createRepo(
        VIEWER_LOGIN,
        repoName,
        typeof args.description === 'string' ? args.description : '',
        typeof args.private === 'boolean' ? args.private : false,
      );
      return toolOk(repoJson(repo, ctx.origin));
    }
    case 'get_repo': {
      const repo = findRepo(args);
      if (!repo) return toolError('Repository not found');
      return toolOk(repoJson(repo, ctx.origin));
    }
    case 'create_issue': {
      const repo = findRepo(args);
      if (!repo) return toolError('Repository not found');
      const issue = createIssueRecord(repo, {
        title: String(args.title),
        body: typeof args.body === 'string' ? args.body : '',
        labels: [],
      });
      return toolOk(issueJson(repo, issue, ctx.origin));
    }
    case 'list_issues': {
      const repo = findRepo(args);
      if (!repo) return toolError('Repository not found');
      const state = typeof args.state === 'string' ? args.state : 'open';
      let issues = [...repo.entries.values()].filter((entry) => entry.kind === 'issue');
      if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
      else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
      issues.sort((a, b) => a.createdSeq - b.createdSeq);
      return toolOk(issues.map((issue) => (issue.kind === 'issue' ? issueJson(repo, issue, ctx.origin) : {})));
    }
    case 'create_pull_request': {
      const repo = findRepo(args);
      if (!repo) return toolError('Repository not found');
      const result = createPullRecord(repo, {
        title: String(args.title),
        body: typeof args.body === 'string' ? args.body : '',
        headRef: String(args.head),
        baseRef: String(args.base),
      });
      if ('error' in result) return toolError(`Could not create pull request: ${result.error}`);
      return toolOk(pullJson(repo, result.pull, ctx.origin));
    }
    case 'merge_pull_request': {
      const repo = findRepo(args);
      if (!repo) return toolError('Repository not found');
      const number = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number);
      const entry = Number.isFinite(number) ? repo.entries.get(number) : undefined;
      if (!entry || entry.kind !== 'pull') return toolError('Pull request not found');
      const result = mergePullRecord(repo, entry);
      if (!result.ok) return toolError('Pull request is not mergeable');
      return toolOk({
        merged: true,
        sha: result.sha,
        message: 'Pull Request successfully merged',
        number: entry.number,
        state: 'closed',
      });
    }
    default:
      return toolError(`Unknown tool: ${name}`);
  }
}

function dispatch(ctx: Ctx, request: Record<string, unknown>): RpcResult {
  const method = typeof request.method === 'string' ? request.method : '';
  const id = request.id;
  const params = isRecord(request.params) ? request.params : {};

  if (method === 'server/discover') {
    return rpcOk(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const protocolVersion = SUPPORTED_VERSIONS.includes(requested) ? requested : '2025-11-25';
    return rpcOk(id, {
      protocolVersion,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'ping') return rpcOk(id, {});
  if (method === 'tools/list') {
    return {
      status: 200,
      payload: { jsonrpc: '2.0', id: id ?? null, result: { tools: JSON.parse(TOOLS_LIST_JSON) as unknown } },
    };
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    if (!TOOLS.some((tool) => tool.name === name)) {
      return rpcError(id, -32602, `Unknown tool: ${name}`);
    }
    const args = isRecord(params.arguments) ? params.arguments : {};
    return rpcOk(id, callTool(ctx, name, args));
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}

export function handleMcp(ctx: Ctx, authorized: boolean): void {
  // Parse first: an unparseable body is a -32700 before auth is even considered.
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    ctx.sendJson(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  if (!authorized) {
    ctx.sendJson(401, { jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized' } });
    return;
  }
  if (Array.isArray(parsed)) {
    const responses: unknown[] = [];
    for (const entry of parsed) {
      if (isRecord(entry)) {
        if (entry.id === undefined) continue; // notification inside a batch: no response
        const result = dispatch(ctx, entry);
        if (result.payload !== undefined) responses.push(result.payload);
      }
    }
    ctx.sendJson(200, responses);
    return;
  }
  if (!isRecord(parsed)) {
    ctx.sendJson(400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid request' } });
    return;
  }
  if (parsed.id === undefined) {
    // A notification (e.g. notifications/initialized): accepted, no body.
    ctx.send(202, 'application/json; charset=utf-8', '');
    return;
  }
  const result = dispatch(ctx, parsed);
  if (result.payload === undefined) {
    ctx.send(202, 'application/json; charset=utf-8', '');
    return;
  }
  ctx.sendJson(result.status, result.payload);
}
