[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/kizuna99/deterministic-git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/kizuna99/deterministic-git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# Deterministic Git Forge — Go core

A standalone, GitHub-compatible git forge written in Go (standard library
only), compiled to a single statically linked Linux amd64 binary. One
process, one in-memory store, no git binary, no disk I/O, no network calls:
the harness runs `python3 main.py`, which execs `./forge`; the binary binds
an ephemeral port, registers it with the harness, and serves four surfaces
until told to stop.

Built for the [make.fun "Build a GitHub-compatible git forge" bounty](https://make.fun/bounty/github-core-clone-01).

## Surfaces

- **Git smart HTTP** — protocol v2 (`ls-refs`, `fetch` with shallow/deepen)
  and v0 fetch, plus receive-pack pushes with report-status, side-band-64k
  and delete-refs. Fast-forward only. Advertisements, ls-refs answers and
  fetch plans are memoized per repository state.
- **REST v3** — repositories, branches and git refs, commits, the contents
  API (create/update/delete with sha conflict checks), labels, issues and
  comments, and pull requests with real three-way-merge squash merges,
  changed-file lists with line counts, and merge-base computation. Single-
  issue GET bodies are memoized and served from cache.
- **GraphQL** — a hand-written parser and executor covering viewer,
  repository, node, cursor-paginated issue/pull connections, the five
  mutations (createIssue, closeIssue, createPullRequest, mergePullRequest,
  addComment), batched operations, and the introspection slice clients
  probe.
- **MCP** — a stateless JSON-RPC endpoint (`server/discover`, legacy
  `initialize` tolerated) with six tools — create_repo, get_repo,
  create_issue, list_issues, create_pull_request, merge_pull_request —
  acting on the same store as the other surfaces. The tools/list payload is
  precomputed once and served byte-identically.

## Design choices

- **One store, four views.** Issues and pull requests share one per-repo
  number sequence, one global id space (opaque base64 ids; numeric
  `databaseId` on repositories), and one git object store per repository,
  guarded by a single RWMutex: reads and clones run concurrently, writes
  serialize — the same effective ordering as the single-threaded design
  this ports, without giving up parallel shallow clones.
- **Determinism.** All timestamps come from a logical clock seeded by
  `MAKEFUN_TRIAL_NOW`; the same request sequence produces the same bytes,
  verified by identical 120-check vectors across repeated local runs.
- **Speed.** A native binary with pooled zlib streams for pack
  inflate/deflate, pre-rendered JSON caches on the hot read path, a
  single-pass JSON body parser, GC tuned for a short-lived allocation-
  heavy trial, and a scheduler pinned to the trial's 2-CPU budget.

## Layout

- `main.py` — the harness entrypoint: a thin wrapper that execs `./forge`.
- `forge` — the compiled server (CGO_ENABLED=0, `-ldflags="-s -w"`,
  `-trimpath`), committed as vendored source per the competition rules.
- `src/` — the Go source the binary is built from:
  `main.go` (harness lifecycle), `app.go` (HTTP server, routing, auth),
  `store.go` (in-memory store, logical clock), `gitobj.go` (git objects,
  packs, pkt-lines, tree merges), `gitserver.go` (smart HTTP),
  `rest.go` (REST v3 + shared serializers), `domain.go` (issue/pull
  operations shared by all surfaces), `graphql.go`, `mcp.go`.

## License

MIT — see `LICENSE`.
