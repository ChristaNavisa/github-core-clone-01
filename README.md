# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ChristaNavisa/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ChristaNavisa/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge, built for the make.fun `github-core-clone-01`
bounty. Node 22, standard library only, no build step, no git binary, no
outside services. Run with `npm start` (`node --experimental-strip-types
main.ts`); the entry registers its port with the env and exits when the env
says done.

## Layout

- `main.ts` — the kit starter's entry <-> env lifecycle (verbatim, plus one
  comment line). Binds an ephemeral port, POSTs it to `$MAKEFUN_ENV_URL`,
  polls `/done`.
- `app.ts` — the HTTP server: routing, bearer auth, `X-GitHub-Request-Id`
  on every reply, JSON body parsing.
- `store.ts` — the forge itself: an in-memory object store with SHA-1,
  pack reading/writing (plain objects out, OFS_DELTA and REF_DELTA in),
  tree/commit/tag parsing, reachability, merge-base, three-way file diffs
  and squash merges, plus the repo/issue/pull model.
- `git.ts` — git smart HTTP: `info/refs` advertisements (protocol v2 and
  v0), `ls-refs`, upload-pack fetch (v2 and v0, with shallow/deepen and
  incremental fetches) and receive-pack push with fast-forward checks and
  `report-status`.
- `rest.ts` — REST v3: repos, refs, branches, commits, contents, labels,
  issues, comments, pull requests.
- `graphql.ts` — GraphQL v4: a small parser and executor for the
  repository/issue/pull-request documents, introspection of the types the
  suite checks, batch requests, and opaque global ids.
- `web.ts` — shared HTTP plumbing: the response shape, JSON replies and
  the pkt-line codec.
- `mcp.ts` — MCP over Streamable HTTP: the `2026-07-28` discovery
  handshake, the legacy `initialize` handshake, `tools/list` and six tools
  (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`).

## Behaviour notes

- One viewer, `adaiversen` (id 1000); the bearer token is
  `MAKEFUN_SHOP_TOKEN` when set, otherwise any non-empty bearer token.
- All server-built timestamps are fixed (`2025-01-01T00:00:00Z`) and request
  ids come from a counter, so replays are deterministic.
- Objects pushed by clients are stored byte-identical; commits and tags
  round-trip exactly.
