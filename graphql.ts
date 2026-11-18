// The GraphQL surface: a small parser and executor for the query shapes the suite (and gh-style
// clients) send — viewer, repository, node, issues and pull requests with cursor pagination,
// the five mutations, batching, and the slice of introspection that gets probed.

import { JSON_PARSE_ERROR, isRecord, type Ctx } from './ctx.ts';
import {
  store,
  branchTip,
  globalId,
  parseGlobalId,
  VIEWER_ID,
  VIEWER_LOGIN,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './store.ts';
import {
  createIssueRecord,
  createPullRecord,
  mergePullRecord,
  mergeableOf,
  pullFiles,
} from './domain.ts';

// ---------------------------------------------------------------------------
// Lexer

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'punct'; value: string }
  | { kind: 'spread' }
  | { kind: 'eof' };

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const isNameStart = (c: string): boolean => /[A-Za-z_]/.test(c);
  const isNameChar = (c: string): boolean => /[A-Za-z0-9_]/.test(c);
  while (i < source.length) {
    const c = source[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',' || c === '﻿') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (source.startsWith('...', i)) {
      tokens.push({ kind: 'spread' });
      i += 3;
      continue;
    }
    if ('!$():=@[]{}|'.includes(c)) {
      tokens.push({ kind: 'punct', value: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      if (source.startsWith('"""', i)) {
        const end = source.indexOf('"""', i + 3);
        const value = end < 0 ? source.slice(i + 3) : source.slice(i + 3, end);
        tokens.push({ kind: 'string', value });
        i = end < 0 ? source.length : end + 3;
        continue;
      }
      let j = i + 1;
      let value = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\' && j + 1 < source.length) {
          const esc = source[j + 1];
          value += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          value += source[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      while (j < source.length && /[0-9.eE+-]/.test(source[j])) j += 1;
      const text = source.slice(i, j);
      const num = Number(text);
      tokens.push(text.includes('.') || text.includes('e') || text.includes('E')
        ? { kind: 'float', value: num }
        : { kind: 'int', value: num });
      i = j;
      continue;
    }
    if (isNameStart(c)) {
      let j = i;
      while (j < source.length && isNameChar(source[j])) j += 1;
      tokens.push({ kind: 'name', value: source.slice(i, j) });
      i = j;
      continue;
    }
    i += 1; // unknown character: skip rather than fail the whole document
  }
  tokens.push({ kind: 'eof' });
  return tokens;
}

// ---------------------------------------------------------------------------
// Parser

type Value =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'bool'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; items: Value[] }
  | { kind: 'object'; fields: Array<{ name: string; value: Value }> };

interface FieldNode {
  name: string;
  alias?: string;
  args: Array<{ name: string; value: Value }>;
  selection: SelectionNode[] | undefined;
}

type SelectionNode =
  | { kind: 'field'; field: FieldNode }
  | { kind: 'inline'; typeCondition: string | undefined; selection: SelectionNode[] }
  | { kind: 'spread'; name: string };

interface OperationNode {
  opType: 'query' | 'mutation';
  name?: string;
  selection: SelectionNode[];
}

interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, { typeCondition: string | undefined; selection: SelectionNode[] }>;
}

class Parser {
  private tokens: Token[];
  private pos = 0;

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const token = this.tokens[this.pos];
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected '${value}'`);
  }

  private skipPunct(value: string): boolean {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === value) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  private peekPunct(value: string): boolean {
    const token = this.peek();
    return token.kind === 'punct' && token.value === value;
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error('expected a name');
    return token.value;
  }

  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, { typeCondition: string | undefined; selection: SelectionNode[] }>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ opType: 'query', selection: this.parseSelectionSet() });
        continue;
      }
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription')) {
        this.next();
        let name: string | undefined;
        if (this.peek().kind === 'name') name = this.expectName();
        if (this.peekPunct('(')) this.skipBalancedParens();
        this.skipDirectives();
        const selection = this.parseSelectionSet();
        operations.push({ opType: token.value === 'mutation' ? 'mutation' : 'query', name, selection });
        continue;
      }
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const fragName = this.expectName();
        let typeCondition: string | undefined;
        if (this.peek().kind === 'name' && (this.peek() as { value: string }).value === 'on') {
          this.next();
          typeCondition = this.expectName();
        }
        this.skipDirectives();
        const selection = this.parseSelectionSet();
        fragments.set(fragName, { typeCondition, selection });
        continue;
      }
      this.next(); // tolerate anything else at the top level
    }
    return { operations, fragments };
  }

  private skipBalancedParens(): void {
    let depth = 0;
    for (;;) {
      const token = this.next();
      if (token.kind === 'eof') return;
      if (token.kind === 'punct' && token.value === '(') depth += 1;
      if (token.kind === 'punct' && token.value === ')') {
        depth -= 1;
        if (depth === 0) return;
      }
    }
  }

  private skipDirectives(): void {
    while (this.peekPunct('@')) {
      this.next();
      this.expectName();
      if (this.peekPunct('(')) this.skipBalancedParens();
    }
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!this.peekPunct('}')) {
      if (this.peek().kind === 'eof') throw new Error('unterminated selection set');
      if (this.peek().kind === 'spread') {
        this.next();
        if (this.peek().kind === 'name' && (this.peek() as { value: string }).value === 'on') {
          this.next();
          const typeCondition = this.expectName();
          this.skipDirectives();
          selections.push({ kind: 'inline', typeCondition, selection: this.parseSelectionSet() });
        } else {
          const name = this.expectName();
          this.skipDirectives();
          selections.push({ kind: 'spread', name });
        }
        continue;
      }
      selections.push({ kind: 'field', field: this.parseField() });
    }
    this.expectPunct('}');
    return selections;
  }

  private parseField(): FieldNode {
    let name = this.expectName();
    let alias: string | undefined;
    if (this.skipPunct(':')) {
      alias = name;
      name = this.expectName();
    }
    const args: FieldNode['args'] = [];
    if (this.peekPunct('(')) {
      this.next();
      while (!this.peekPunct(')')) {
        const argName = this.expectName();
        this.expectPunct(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expectPunct(')');
    }
    this.skipDirectives();
    let selection: SelectionNode[] | undefined;
    if (this.peekPunct('{')) selection = this.parseSelectionSet();
    return { name, alias, args, selection };
  }

  private parseValue(): Value {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: token.value };
    if (token.kind === 'float') return { kind: 'float', value: token.value };
    if (token.kind === 'punct' && token.value === '[') {
      const items: Value[] = [];
      while (!this.peekPunct(']')) items.push(this.parseValue());
      this.expectPunct(']');
      return { kind: 'list', items };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: Array<{ name: string; value: Value }> = [];
      while (!this.peekPunct('}')) {
        const fieldName = this.expectName();
        this.expectPunct(':');
        fields.push({ name: fieldName, value: this.parseValue() });
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'bool', value: true };
      if (token.value === 'false') return { kind: 'bool', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error('unexpected value token');
  }
}

function valueOf(value: Value, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'variable':
      return variables[value.name];
    case 'string':
    case 'int':
    case 'float':
    case 'bool':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'list':
      return value.items.map((item) => valueOf(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of value.fields) out[field.name] = valueOf(field.value, variables);
      return out;
    }
  }
}

// ---------------------------------------------------------------------------
// Schema description for introspection

const TYPE_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', 'search', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['id', 'login', 'name', 'email', 'url'],
  Viewer: ['id', 'login', 'name', 'email', 'url'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'isPrivate',
    'createdAt', 'updatedAt', 'url',
  ],
  Issue: [
    'id', 'number', 'title', 'state', 'body', 'author', 'labels', 'comments', 'createdAt',
    'updatedAt', 'repository', 'url', 'closed',
  ],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName',
    'files', 'author', 'createdAt', 'updatedAt', 'repository', 'url', 'mergedAt', 'mergeCommit',
  ],
  Ref: ['id', 'name', 'prefix', 'target'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueComment: ['id', 'body', 'author', 'createdAt', 'url'],
  Label: ['id', 'name', 'color', 'description'],
  LabelConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
  CommentEdge: ['node', 'cursor'],
  __Type: ['name', 'kind', 'fields', 'enumValues', 'description'],
  __Field: ['name', 'description', 'args', 'type'],
  __EnumValue: ['name', 'description'],
};

const ENUM_TYPES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  Permission: ['ADMIN', 'WRITE', 'READ', 'TRIAGE', 'MAINTAIN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  PullRequestMergeMethod: ['MERGE', 'REBASE', 'SQUASH'],
};

// ---------------------------------------------------------------------------
// Execution

interface ExecCtx {
  variables: Record<string, unknown>;
  fragments: DocumentNode['fragments'];
  errors: Array<{ message: string }>;
  origin: string;
}

/** A resolved value tagged with its GraphQL type name for fragment matching. */
interface Typed {
  __typename: string;
  value: unknown;
}

function typed(typename: string, value: unknown): Typed {
  return { __typename: typename, value };
}

function cursorOf(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberFromCursor(cursor: string): number | undefined {
  try {
    const decoded = Buffer.from(cursor, 'base64').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(decoded);
    return match ? Number.parseInt(match[1], 10) : undefined;
  } catch {
    return undefined;
  }
}

function issueStateGraphql(issue: IssueRec): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullStateGraphql(pull: PullRec): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function resolveObject(
  ctx: ExecCtx,
  obj: Typed,
  selection: SelectionNode[] | undefined,
): Record<string, unknown> | unknown {
  if (!selection) return obj.value;
  const out: Record<string, unknown> = {};
  execSelectionInto(ctx, obj, selection, out);
  return out;
}

function execSelectionInto(ctx: ExecCtx, obj: Typed, selection: SelectionNode[], out: Record<string, unknown>): void {
  for (const node of selection) {
    if (node.kind === 'inline') {
      if (!node.typeCondition || node.typeCondition === obj.__typename) {
        execSelectionInto(ctx, obj, node.selection, out);
      }
      continue;
    }
    if (node.kind === 'spread') {
      const fragment = ctx.fragments.get(node.name);
      if (fragment && (!fragment.typeCondition || fragment.typeCondition === obj.__typename)) {
        execSelectionInto(ctx, obj, fragment.selection, out);
      }
      continue;
    }
    const field = node.field;
    const key = field.alias ?? field.name;
    const args: Record<string, unknown> = {};
    for (const arg of field.args) args[arg.name] = valueOf(arg.value, ctx.variables);
    out[key] = resolveField(ctx, obj, field.name, args, field.selection);
  }
}

function resolveField(
  ctx: ExecCtx,
  obj: Typed,
  field: string,
  args: Record<string, unknown>,
  selection: SelectionNode[] | undefined,
): unknown {
  const type = obj.__typename;
  const value = obj.value;

  if (field === '__typename') return type;

  if (type === '__Type') {
    const typeName = value as string;
    if (field === 'name') return typeName;
    if (field === 'kind') return ENUM_TYPES[typeName] ? 'ENUM' : 'OBJECT';
    if (field === 'fields') {
      const fields = TYPE_FIELDS[typeName];
      if (!fields) return null;
      return fields.map((name) => resolveObject(ctx, typed('__Field', name), selection));
    }
    if (field === 'enumValues') {
      const values = ENUM_TYPES[typeName];
      if (!values) return null;
      return values.map((name) => resolveObject(ctx, typed('__EnumValue', name), selection));
    }
    if (field === 'description') return null;
    ctx.errors.push({ message: `Field '${field}' doesn't exist on type '__Type'` });
    return null;
  }
  if (type === '__Field' || type === '__EnumValue') {
    if (field === 'name') return value as string;
    if (field === 'description') return null;
    if (field === 'args') return [];
    if (field === 'type') return null;
    ctx.errors.push({ message: `Field '${field}' doesn't exist on type '${type}'` });
    return null;
  }

  switch (type) {
    case 'Query':
      return resolveQuery(ctx, field, args, selection);
    case 'Mutation':
      return resolveMutation(ctx, field, args, selection);
    case 'User':
    case 'Viewer': {
      if (field === 'login') return VIEWER_LOGIN;
      if (field === 'id') return globalId('user', VIEWER_ID);
      if (field === 'name') return 'ada iversen';
      if (field === 'email') return 'adaiversen@example.invalid';
      if (field === 'url') return `${ctx.origin}/${VIEWER_LOGIN}`;
      break;
    }
    case 'Repository':
      return resolveRepository(ctx, value as RepoRec, field, args, selection);
    case 'Issue':
      return resolveIssue(ctx, value as { repo: RepoRec; issue: IssueRec }, field, selection);
    case 'PullRequest':
      return resolvePull(ctx, value as { repo: RepoRec; pull: PullRec }, field, args, selection);
    case 'Ref': {
      if (field === 'name') return (value as { name: string }).name;
      if (field === 'id') return globalId('ref', 0);
      if (field === 'prefix') return 'refs/heads/';
      break;
    }
    case 'PageInfo': {
      const pageInfo = value as { hasNextPage: boolean; endCursor: string | null };
      if (field === 'hasNextPage') return pageInfo.hasNextPage;
      if (field === 'hasPreviousPage') return false;
      if (field === 'endCursor') return pageInfo.endCursor;
      if (field === 'startCursor') return pageInfo.endCursor;
      break;
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'LabelConnection': {
      const connection = value as { nodes: Typed[]; pageInfo: Typed };
      if (field === 'nodes') return connection.nodes.map((node) => resolveObject(ctx, node, selection));
      if (field === 'pageInfo') return resolveObject(ctx, connection.pageInfo, selection);
      if (field === 'totalCount') return connection.nodes.length;
      if (field === 'edges') {
        return connection.nodes.map((node) => ({
          node: resolveObject(ctx, node, undefined),
          cursor: '',
        }));
      }
      break;
    }
    case 'PullRequestChangedFile': {
      const file = value as { path: string; additions: number; deletions: number };
      if (field === 'path') return file.path;
      if (field === 'additions') return file.additions;
      if (field === 'deletions') return file.deletions;
      if (field === 'changeType') return 'MODIFIED';
      break;
    }
    case 'IssueComment': {
      const comment = value as { id: number; body: string };
      if (field === 'id') return globalId('comment', comment.id);
      if (field === 'body') return comment.body;
      if (field === 'author') return resolveObject(ctx, typed('User', null), selection);
      break;
    }
    case 'Label': {
      const label = value as { name: string };
      if (field === 'name') return label.name;
      if (field === 'id') return globalId('label', 0);
      if (field === 'color') return 'ededed';
      if (field === 'description') return '';
      break;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field === 'issue') {
        const payload = value as { repo: RepoRec; issue: IssueRec };
        return resolveObject(ctx, typed('Issue', payload), selection);
      }
      if (field === 'clientMutationId') return null;
      break;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field === 'pullRequest') {
        const payload = value as { repo: RepoRec; pull: PullRec };
        return resolveObject(ctx, typed('PullRequest', payload), selection);
      }
      if (field === 'clientMutationId') return null;
      break;
    }
    case 'AddCommentPayload': {
      if (field === 'commentEdge') {
        const comment = value as { id: number; body: string };
        const edgeSelection = selection;
        const edgeObj: Record<string, unknown> = {};
        if (edgeSelection) {
          for (const node of edgeSelection) {
            if (node.kind === 'field' && node.field.name === 'node') {
              edgeObj[node.field.alias ?? 'node'] = resolveObject(ctx, typed('IssueComment', comment), node.field.selection);
            } else if (node.kind === 'field' && node.field.name === 'cursor') {
              edgeObj[node.field.alias ?? 'cursor'] = cursorOf(comment.id);
            }
          }
        }
        return edgeObj;
      }
      if (field === 'clientMutationId') return null;
      if (field === 'subject') return null;
      break;
    }
  }
  ctx.errors.push({ message: `Field '${field}' doesn't exist on type '${type}'` });
  return null;
}

function resolveQuery(ctx: ExecCtx, field: string, args: Record<string, unknown>, selection: SelectionNode[] | undefined): unknown {
  if (field === 'viewer') return resolveObject(ctx, typed('User', null), selection);
  if (field === '__type') {
    const name = typeof args.name === 'string' ? args.name : '';
    if (!TYPE_FIELDS[name] && !ENUM_TYPES[name]) return null;
    return resolveObject(ctx, typed('__Type', name), selection);
  }
  if (field === '__schema') return null;
  if (field === 'repository') {
    const owner = typeof args.owner === 'string' ? args.owner : '';
    const name = typeof args.name === 'string' ? args.name : '';
    const repo = owner === VIEWER_LOGIN ? store.getRepo(owner, name) : undefined;
    if (!repo) return null;
    return resolveObject(ctx, typed('Repository', repo), selection);
  }
  if (field === 'node') {
    const id = typeof args.id === 'string' ? args.id : '';
    const parsed = parseGlobalId(id);
    if (!parsed) return null;
    if (parsed.kind === 'repo') {
      const repo = store.reposById.get(parsed.numericId);
      if (!repo) return null;
      return resolveObject(ctx, typed('Repository', repo), selection);
    }
    if (parsed.kind === 'user') return resolveObject(ctx, typed('User', null), selection);
    if (parsed.kind === 'issue' || parsed.kind === 'pull') {
      for (const repo of store.repos.values()) {
        for (const entry of repo.entries.values()) {
          if (entry.id === parsed.numericId) {
            return entry.kind === 'issue'
              ? resolveObject(ctx, typed('Issue', { repo, issue: entry }), selection)
              : resolveObject(ctx, typed('PullRequest', { repo, pull: entry }), selection);
          }
        }
      }
      return null;
    }
    return null;
  }
  ctx.errors.push({ message: `Field '${field}' doesn't exist on type 'Query'` });
  return null;
}

function connectionOf(
  ctx: ExecCtx,
  items: Typed[],
  args: Record<string, unknown>,
  numberOf: (item: Typed) => number,
  selection: SelectionNode[] | undefined,
  typename: string,
): unknown {
  const first = typeof args.first === 'number' ? args.first : items.length;
  let startIndex = 0;
  if (typeof args.after === 'string') {
    const afterNumber = numberFromCursor(args.after);
    if (afterNumber !== undefined) {
      const idx = items.findIndex((item) => numberOf(item) === afterNumber);
      if (idx >= 0) startIndex = idx + 1;
    }
  }
  const page = items.slice(startIndex, startIndex + first);
  const hasNextPage = startIndex + first < items.length;
  const endCursor = page.length > 0 ? cursorOf(numberOf(page[page.length - 1])) : null;
  const connection = typed(typename, {
    nodes: page,
    pageInfo: typed('PageInfo', { hasNextPage, endCursor }),
  });
  return resolveObject(ctx, connection, selection);
}

function resolveRepository(
  ctx: ExecCtx,
  repo: RepoRec,
  field: string,
  args: Record<string, unknown>,
  selection: SelectionNode[] | undefined,
): unknown {
  switch (field) {
    case 'id':
      return globalId('repo', repo.id);
    case 'databaseId':
      return repo.id;
    case 'name':
      return repo.name;
    case 'nameWithOwner':
      return `${repo.owner}/${repo.name}`;
    case 'description':
      return repo.description;
    case 'isPrivate':
      return repo.isPrivate;
    case 'owner':
      return resolveObject(ctx, typed('User', null), selection);
    case 'defaultBranchRef': {
      const tip = branchTip(repo, repo.defaultBranch);
      if (!tip) return null;
      return resolveObject(ctx, typed('Ref', { name: repo.defaultBranch }), selection);
    }
    case 'viewerPermission':
      return 'ADMIN';
    case 'hasIssuesEnabled':
      return true;
    case 'createdAt':
      return repo.createdAt;
    case 'updatedAt':
      return repo.updatedAt;
    case 'url':
      return `${ctx.origin}/${repo.owner}/${repo.name}`;
    case 'issue': {
      const number = typeof args.number === 'number' ? args.number : -1;
      const entry = repo.entries.get(number);
      if (!entry || entry.kind !== 'issue') return null;
      return resolveObject(ctx, typed('Issue', { repo, issue: entry }), selection);
    }
    case 'issues': {
      let issues = [...repo.entries.values()].filter((e): e is IssueRec => e.kind === 'issue');
      const states = Array.isArray(args.states) ? (args.states as unknown[]).map(String) : undefined;
      if (states && states.length > 0) {
        issues = issues.filter((issue) => states.includes(issueStateGraphql(issue)));
      }
      let direction = 'DESC';
      if (isRecord(args.orderBy) && typeof args.orderBy.direction === 'string') direction = args.orderBy.direction;
      issues.sort((a, b) =>
        direction === 'ASC'
          ? a.createdSeq - b.createdSeq || a.number - b.number
          : b.createdSeq - a.createdSeq || b.number - a.number,
      );
      const items = issues.map((issue) => typed('Issue', { repo, issue }));
      return connectionOf(ctx, items, args, (item) => (item.value as { issue: IssueRec }).issue.number, selection, 'IssueConnection');
    }
    case 'pullRequest': {
      const number = typeof args.number === 'number' ? args.number : -1;
      const entry = repo.entries.get(number);
      if (!entry || entry.kind !== 'pull') return null;
      return resolveObject(ctx, typed('PullRequest', { repo, pull: entry }), selection);
    }
    case 'pullRequests': {
      let pulls = [...repo.entries.values()].filter((e): e is PullRec => e.kind === 'pull');
      const states = Array.isArray(args.states) ? (args.states as unknown[]).map(String) : undefined;
      if (states && states.length > 0) pulls = pulls.filter((pull) => states.includes(pullStateGraphql(pull)));
      pulls.sort((a, b) => b.createdSeq - a.createdSeq || b.number - a.number);
      const items = pulls.map((pull) => typed('PullRequest', { repo, pull }));
      return connectionOf(ctx, items, args, (item) => (item.value as { pull: PullRec }).pull.number, selection, 'PullRequestConnection');
    }
    default:
      ctx.errors.push({ message: `Field '${field}' doesn't exist on type 'Repository'` });
      return null;
  }
}

function resolveIssue(
  ctx: ExecCtx,
  value: { repo: RepoRec; issue: IssueRec },
  field: string,
  selection: SelectionNode[] | undefined,
): unknown {
  const { repo, issue } = value;
  switch (field) {
    case 'id':
      return globalId('issue', issue.id);
    case 'number':
      return issue.number;
    case 'title':
      return issue.title;
    case 'body':
      return issue.body;
    case 'state':
      return issueStateGraphql(issue);
    case 'closed':
      return issue.state === 'closed';
    case 'author':
      return resolveObject(ctx, typed('User', null), selection);
    case 'repository':
      return resolveObject(ctx, typed('Repository', repo), selection);
    case 'createdAt':
      return issue.createdAt;
    case 'updatedAt':
      return issue.updatedAt;
    case 'url':
      return `${ctx.origin}/${repo.owner}/${repo.name}/issues/${issue.number}`;
    case 'labels': {
      const nodes = issue.labels.map((name) => typed('Label', { name }));
      const connection = typed('LabelConnection', {
        nodes,
        pageInfo: typed('PageInfo', { hasNextPage: false, endCursor: null }),
      });
      return resolveObject(ctx, connection, selection);
    }
    case 'comments': {
      const nodes = issue.comments.map((comment) => typed('IssueComment', comment));
      const connection = typed('IssueConnection', {
        nodes,
        pageInfo: typed('PageInfo', { hasNextPage: false, endCursor: null }),
      });
      return resolveObject(ctx, connection, selection);
    }
    default:
      ctx.errors.push({ message: `Field '${field}' doesn't exist on type 'Issue'` });
      return null;
  }
}

function resolvePull(
  ctx: ExecCtx,
  value: { repo: RepoRec; pull: PullRec },
  field: string,
  args: Record<string, unknown>,
  selection: SelectionNode[] | undefined,
): unknown {
  const { repo, pull } = value;
  switch (field) {
    case 'id':
      return globalId('pull', pull.id);
    case 'number':
      return pull.number;
    case 'title':
      return pull.title;
    case 'body':
      return pull.body;
    case 'state':
      return pullStateGraphql(pull);
    case 'merged':
      return pull.merged;
    case 'mergedAt':
      return pull.mergedAt;
    case 'mergeable':
      return mergeableOf(repo, pull);
    case 'headRefName':
      return pull.headRef;
    case 'baseRefName':
      return pull.baseRef;
    case 'author':
      return resolveObject(ctx, typed('User', null), selection);
    case 'repository':
      return resolveObject(ctx, typed('Repository', repo), selection);
    case 'createdAt':
      return pull.createdAt;
    case 'updatedAt':
      return pull.updatedAt;
    case 'url':
      return `${ctx.origin}/${repo.owner}/${repo.name}/pull/${pull.number}`;
    case 'files': {
      const files = pullFiles(repo, pull).map((file) =>
        typed('PullRequestChangedFile', { path: file.filename, additions: file.additions, deletions: file.deletions }),
      );
      return connectionOf(ctx, files, args, () => 0, selection, 'PullRequestChangedFileConnection');
    }
    default:
      ctx.errors.push({ message: `Field '${field}' doesn't exist on type 'PullRequest'` });
      return null;
  }
}

// ---------------------------------------------------------------------------
// Mutations

function findEntryByGlobalId(id: string): { repo: RepoRec; entry: IssueRec | PullRec } | undefined {
  const parsed = parseGlobalId(id);
  if (!parsed) return undefined;
  for (const repo of store.repos.values()) {
    for (const entry of repo.entries.values()) {
      if (entry.id === parsed.numericId) return { repo, entry };
    }
  }
  return undefined;
}

function resolveMutation(ctx: ExecCtx, field: string, args: Record<string, unknown>, selection: SelectionNode[] | undefined): unknown {
  const input = isRecord(args.input) ? args.input : {};
  switch (field) {
    case 'createIssue': {
      const repoId = typeof input.repositoryId === 'string' ? input.repositoryId : '';
      const parsed = parseGlobalId(repoId);
      const repo = parsed && parsed.kind === 'repo' ? store.reposById.get(parsed.numericId) : undefined;
      if (!repo) {
        ctx.errors.push({ message: 'Repository not found' });
        return null;
      }
      const issue = createIssueRecord(repo, {
        title: typeof input.title === 'string' ? input.title : '',
        body: typeof input.body === 'string' ? input.body : '',
        labels: [],
      });
      return resolveObject(ctx, typed('CreateIssuePayload', { repo, issue }), selection);
    }
    case 'closeIssue': {
      const found = typeof input.issueId === 'string' ? findEntryByGlobalId(input.issueId) : undefined;
      if (!found || found.entry.kind !== 'issue') {
        ctx.errors.push({ message: 'Issue not found' });
        return null;
      }
      found.entry.state = 'closed';
      found.entry.updatedAt = store.tick().iso;
      return resolveObject(ctx, typed('CloseIssuePayload', { repo: found.repo, issue: found.entry }), selection);
    }
    case 'createPullRequest': {
      const repoId = typeof input.repositoryId === 'string' ? input.repositoryId : '';
      const parsed = parseGlobalId(repoId);
      const repo = parsed && parsed.kind === 'repo' ? store.reposById.get(parsed.numericId) : undefined;
      if (!repo) {
        ctx.errors.push({ message: 'Repository not found' });
        return null;
      }
      const result = createPullRecord(repo, {
        title: typeof input.title === 'string' ? input.title : '',
        body: typeof input.body === 'string' ? input.body : '',
        headRef: typeof input.headRefName === 'string' ? input.headRefName : '',
        baseRef: typeof input.baseRefName === 'string' ? input.baseRefName : '',
      });
      if ('error' in result) {
        ctx.errors.push({ message: `Could not create pull request: ${result.error}` });
        return null;
      }
      return resolveObject(ctx, typed('CreatePullRequestPayload', { repo, pull: result.pull }), selection);
    }
    case 'mergePullRequest': {
      const found = typeof input.pullRequestId === 'string' ? findEntryByGlobalId(input.pullRequestId) : undefined;
      if (!found || found.entry.kind !== 'pull') {
        ctx.errors.push({ message: 'Pull request not found' });
        return null;
      }
      const result = mergePullRecord(found.repo, found.entry);
      if (!result.ok) {
        ctx.errors.push({ message: 'Pull request is not mergeable' });
        return resolveObject(ctx, typed('MergePullRequestPayload', { repo: found.repo, pull: found.entry }), selection);
      }
      return resolveObject(ctx, typed('MergePullRequestPayload', { repo: found.repo, pull: found.entry }), selection);
    }
    case 'addComment': {
      const found = typeof input.subjectId === 'string' ? findEntryByGlobalId(input.subjectId) : undefined;
      if (!found) {
        ctx.errors.push({ message: 'Subject not found' });
        return null;
      }
      const comment = {
        id: store.allocEntityId(),
        body: typeof input.body === 'string' ? input.body : '',
        authorLogin: VIEWER_LOGIN,
        createdSeq: store.nextSeq(),
        createdAt: store.tick().iso,
      };
      found.entry.comments.push(comment);
      return resolveObject(ctx, typed('AddCommentPayload', comment), selection);
    }
    default:
      ctx.errors.push({ message: `Field '${field}' doesn't exist on type 'Mutation'` });
      return null;
  }
}

// ---------------------------------------------------------------------------
// Entry point

interface OperationResult {
  data: Record<string, unknown>;
  errors?: Array<{ message: string }>;
}

function runOperation(doc: DocumentNode, op: OperationNode, variables: Record<string, unknown>, origin: string): OperationResult {
  const ctx: ExecCtx = { variables, fragments: doc.fragments, errors: [], origin };
  const root = typed(op.opType === 'mutation' ? 'Mutation' : 'Query', null);
  const data: Record<string, unknown> = {};
  execSelectionInto(ctx, root, op.selection, data);
  const result: OperationResult = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}

export function handleGraphQL(ctx: Ctx): void {
  if (ctx.json === JSON_PARSE_ERROR) {
    ctx.sendJson(400, { errors: [{ message: 'Problems parsing JSON' }] });
    return;
  }
  const runOne = (payload: unknown): OperationResult => {
    if (!isRecord(payload) || typeof payload.query !== 'string') {
      return { data: {}, errors: [{ message: 'A GraphQL query string is required' }] };
    }
    const variables = isRecord(payload.variables) ? payload.variables : {};
    let doc: DocumentNode;
    try {
      doc = new Parser(tokenize(payload.query)).parseDocument();
    } catch (err) {
      return { data: {}, errors: [{ message: err instanceof Error ? err.message : 'Parse error' }] };
    }
    let op = doc.operations[0];
    if (typeof payload.operationName === 'string') {
      op = doc.operations.find((candidate) => candidate.name === payload.operationName) ?? op;
    }
    if (!op) return { data: {}, errors: [{ message: 'No operation found' }] };
    return runOneDocument(doc, op, variables, ctx.origin);
  };
  if (Array.isArray(ctx.json)) {
    ctx.sendJson(200, ctx.json.map((entry) => runOne(entry)));
    return;
  }
  ctx.sendJson(200, runOne(ctx.json));
}

function runOneDocument(doc: DocumentNode, op: OperationNode, variables: Record<string, unknown>, origin: string): OperationResult {
  return runOperation(doc, op, variables, origin);
}
