# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ChristaNavisa/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ChristaNavisa/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge, built for the make.fun `github-core-clone-01`
bounty. The server is a statically linked Go binary (`forge`, stdlib only,
no cgo, no external modules) built from `go/`; a thin `main.ts` launcher
spawns it, so the entry still starts as `node main.ts`. No build step at
run time, no git binary, no outside services. Run with `npm start`; the
entry registers its port with the env and exits when the env says done.

Why Go: the previous Node implementation scored 925 (feature 600 + agent
200, perf 125, production p50 568 µs). Local trials put the Go build at
p50 ~185–195 µs against Node's ~226–241 µs on the same machine — a
faster server on every workload (push, clone, read, write) with the same
120/120 conformance.

## Layout

- `main.ts` — thin launcher: spawns `./forge`, forwards SIGTERM/SIGINT to
  it, exits with its code. Defensive chmod + a temp-dir fallback if the
  exec bit was lost in transit.
- `forge` — the static Go binary (7.8 MB, linux/amd64), committed in the
  repo as vendored source. Built with
  `CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w"`.
- `go/` — the Go sources (module `forge`, Go 1.27, stdlib only):
  - `main.go` — the entry <-> env lifecycle: bind an ephemeral port, POST
    it to `$MAKEFUN_ENV_URL/register`, poll `/done`.
  - `app.go` — the HTTP server: routing, bearer auth,
    `X-GitHub-Request-Id` on every reply, JSON body parsing, one global
    mutex around dispatch (the workloads are sequential; the clone pack
    is cached after the first fetch).
  - `store.go` — the forge itself: an in-memory object store with SHA-1,
    pack reading/writing (plain objects out, OFS_DELTA and REF_DELTA in),
    tree/commit/tag parsing, reachability, merge-base, three-way file
    diffs and squash merges, plus the repo/issue/pull model.
  - `git.go` — git smart HTTP: `info/refs` advertisements (protocol v2
    and v0), `ls-refs`, upload-pack fetch (v2 and v0, with shallow/deepen
    and incremental fetches) and receive-pack push with fast-forward
    checks and `report-status`.
  - `rest.go` — REST v3: repos, refs, branches, commits, contents,
    labels, issues, comments, pull requests. Hot replies (issue JSON)
    are marshalled from structs, not maps.
  - `graphql.go` — GraphQL v4: a small parser and executor for the
    repository/issue/pull-request documents, introspection of the types
    the suite checks, batch requests, and opaque global ids.
  - `web.go` — shared HTTP plumbing: the response shape, JSON replies
    and the pkt-line codec.
  - `mcp.go` — MCP over Streamable HTTP: the `2026-07-28` discovery
    handshake, the legacy `initialize` handshake, `tools/list` and six
    tools (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
    `create_pull_request`, `merge_pull_request`).

## Behaviour notes

- One viewer, `adaiversen` (id 1000); the bearer token is
  `MAKEFUN_SHOP_TOKEN` when set, otherwise any non-empty bearer token.
- All server-built timestamps are fixed (`2025-01-01T00:00:00Z`) and request
  ids come from a counter, so replays are deterministic.
- Objects pushed by clients are stored byte-identical; commits and tags
  round-trip exactly.
- Deterministic output ordering is preserved everywhere it is observable
  (Go maps iterate in random order; insertion order is kept in slices).
