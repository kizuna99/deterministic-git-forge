// The forge's in-memory store: one viewer, their repositories, and inside each repository a private
// git object store plus the issue / pull / label records the REST, GraphQL and MCP surfaces share.
// Every timestamp comes from a logical clock seeded by MAKEFUN_TRIAL_NOW, so a replayed run of the
// same request sequence produces the same bytes.

import type { GitObject, ObjType } from './gitobj.ts';
import { hashObject } from './gitobj.ts';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';

export interface LabelRec {
  name: string;
  color: string;
  description: string;
}

export interface CommentRec {
  id: number;
  body: string;
  authorLogin: string;
  createdSeq: number;
  createdAt: string;
}

export interface IssueRec {
  kind: 'issue';
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: CommentRec[];
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
}

export interface PullRec {
  kind: 'pull';
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headShaSnapshot: string;
  baseShaSnapshot: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  comments: CommentRec[];
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
}

export type IssueOrPull = IssueRec | PullRec;

export interface RepoRec {
  id: number;
  owner: string;
  name: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  labels: Map<string, LabelRec>;
  entries: Map<number, IssueOrPull>;
  nextNumber: number;
}

/** Global ids: opaque base64 of a typed numeric id, stable for the life of the process. */
export function globalId(kind: string, numericId: number): string {
  return Buffer.from(`v1:${kind}:${numericId}`, 'utf8').toString('base64');
}

export function parseGlobalId(id: string): { kind: string; numericId: number } | undefined {
  try {
    const decoded = Buffer.from(id, 'base64').toString('utf8');
    const match = /^v1:([a-z]+):(\d+)$/.exec(decoded);
    if (!match) return undefined;
    return { kind: match[1], numericId: Number.parseInt(match[2], 10) };
  } catch {
    return undefined;
  }
}

class Store {
  repos = new Map<string, RepoRec>();
  reposById = new Map<number, RepoRec>();
  private nextRepoId = 1;
  private nextEntityId = 1;
  private seq = 0;
  private clockSec: number;

  constructor() {
    const trialNow = process.env.MAKEFUN_TRIAL_NOW ?? '';
    const parsed = Date.parse(trialNow);
    this.clockSec = Number.isFinite(parsed) ? Math.floor(parsed / 1000) : 1735689600;
  }

  /** Monotonic sequence for creation ordering across the whole store. */
  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  /** The logical clock: each call advances one second, so timestamps are unique and ordered. */
  tick(): { sec: number; iso: string } {
    this.clockSec += 1;
    return { sec: this.clockSec, iso: new Date(this.clockSec * 1000).toISOString() };
  }

  nowIso(): string {
    return new Date(this.clockSec * 1000).toISOString();
  }

  allocEntityId(): number {
    const id = this.nextEntityId;
    this.nextEntityId += 1;
    return id;
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): RepoRec {
    const now = this.tick();
    const repo: RepoRec = {
      id: this.nextRepoId,
      owner,
      name,
      description,
      isPrivate,
      defaultBranch: 'main',
      createdSeq: this.nextSeq(),
      createdAt: now.iso,
      updatedAt: now.iso,
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      entries: new Map(),
      nextNumber: 1,
    };
    this.nextRepoId += 1;
    this.repos.set(`${owner}/${name}`, repo);
    this.reposById.set(repo.id, repo);
    return repo;
  }

  getRepo(owner: string, name: string): RepoRec | undefined {
    return this.repos.get(`${owner}/${name}`);
  }

  deleteRepo(repo: RepoRec): void {
    this.repos.delete(`${repo.owner}/${repo.name}`);
    this.reposById.delete(repo.id);
  }

  listRepos(owner: string): RepoRec[] {
    return [...this.repos.values()]
      .filter((repo) => repo.owner === owner)
      .sort((a, b) => a.createdSeq - b.createdSeq);
  }
}

export const store = new Store();

// ---------------------------------------------------------------------------
// Per-repo git helpers

export function repoLookup(repo: RepoRec): (oid: string) => GitObject | undefined {
  return (oid) => repo.objects.get(oid);
}

export function repoPut(repo: RepoRec): (type: ObjType, data: Buffer) => string {
  return (type, data) => {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  };
}

/** The commit a branch ref currently names (peeling tags), or undefined when unborn/missing. */
export function branchTip(repo: RepoRec, branch: string): string | undefined {
  const oid = repo.refs.get(`refs/heads/${branch}`);
  if (!oid) return undefined;
  const obj = repo.objects.get(oid);
  if (!obj) return undefined;
  if (obj.type === 'commit') return oid;
  return undefined;
}
