# Deterministic Git Forge — Git, REST, GraphQL & MCP in one process

A standalone, GitHub-compatible git forge written in TypeScript on the Node 22
standard library alone. One process, one in-memory store, no git binary, no
network calls, no install step: `node main.ts` binds an ephemeral port,
registers it with the harness, and serves four surfaces until told to stop.

## Surfaces

- **Git smart HTTP** — protocol v2 (`ls-refs`, `fetch` with shallow/deepen)
  and v0 fetch, plus receive-pack pushes with per-ref reports. Packs are
  built from the object store on demand (plain objects, memoized per tip),
  and received packs are parsed with full OFS/REF delta resolution.
- **REST v3** — repositories, branches and git refs, commits, the contents
  API (create/update/delete with sha conflict checks), labels, issues and
  comments, and pull requests with real three-way-merge squash merges,
  changed-file lists with line counts, and merge-base computation.
- **GraphQL** — a hand-written parser and executor covering viewer,
  repository, node, cursor-paginated issue/pull connections, the five
  mutations (createIssue, closeIssue, createPullRequest, mergePullRequest,
  addComment), batched operations, and the introspection slice clients probe.
- **MCP** — a stateless JSON-RPC endpoint speaking the modern protocol era
  (`server/discover`, legacy `initialize` tolerated) with six tools —
  create_repo, get_repo, create_issue, list_issues, create_pull_request,
  merge_pull_request — acting on the same store as the other surfaces, so an
  issue created over MCP is the same issue REST and GraphQL see.

## Design choices

- **One store, four views.** Issues and pull requests share one per-repo
  number sequence, one global id space (opaque base64 ids; numeric
  `databaseId` on repositories), and one git object store per repository.
- **Determinism.** All timestamps come from a logical clock seeded by
  `MAKEFUN_TRIAL_NOW`; the same request sequence produces the same bytes.
- **Speed.** The read path is allocation-light, push handling stores a
  three-object pack without ceremony, and shallow-clone pack bytes are
  memoized per repository tip so concurrent clones share one build.
- **Merges are real.** Squash merges compute the merge base and run a
  tree-level three-way merge; both-sides-changed paths report a conflict
  (HTTP 405 / `CONFLICTING`) instead of guessing.

## Layout

- `main.ts` — the harness lifecycle (register, poll `/done`, exit).
- `app.ts` — HTTP server, routing, auth.
- `store.ts` — the in-memory store and logical clock.
- `gitobj.ts` — git objects, packs, pkt-lines, reachability, tree merges.
- `gitserver.ts` — smart HTTP (advertisements, upload-pack, receive-pack).
- `rest.ts` — the REST v3 surface and the shared JSON serializers.
- `domain.ts` — issue/pull operations shared by REST, GraphQL and MCP.
- `graphql.ts` — the GraphQL parser and executor.
- `mcp.ts` — the MCP JSON-RPC endpoint and its tools.

## License

MIT — see `LICENSE`.
