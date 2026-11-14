# github-core-clone-01 — GitHub-compatible git forge

A single-process, in-memory GitHub-compatible git forge written in TypeScript
(Node 22, standard library only — no dependencies, no git binary, no network).

## Layout

- `main.ts` — lifecycle entrypoint (copied from the starter, unmodified):
  listens on an ephemeral port, registers with `$MAKEFUN_ENV_URL`, polls
  `$MAKEFUN_ENV_URL/done` until the trial ends, then exits.
- `app.ts` — the whole forge: `createApp()` returns an `http.Server`.
- `package.json`, `tsconfig.json` — starter copies.

## Surfaces

- **Git smart HTTP** — `/{owner}/{repo}.git/info/refs`,
  `git-upload-pack` (protocol v2 with `ls-refs`, plus v0 fallback) and
  `git-receive-pack` (v0, `report-status`, `side-band-64k`, `delete-refs`).
  A from-scratch implementation of SHA-1 git objects (blob/tree/commit/tag),
  pkt-line framing, packfiles (plain, `OFS_DELTA` and `REF_DELTA` reads,
  thin-pack base resolution) and deterministic ref/closure logic.
  Pushes are fast-forward only; a non-fast-forward ref is refused per-ref
  with `ng <ref> non-fast-forward` while the other refs in the same push
  still apply.
- **REST v3** — `/api/v3` (repos, branches, commits, contents, labels,
  issues, comments, pull requests, merge). Bearer-token auth, `404` (never
  `403`) for foreign owners, `X-GitHub-Request-Id` on every response.
- **GraphQL** — `POST /api/graphql`, including `__type` introspection,
  cursor pagination, `node(id:)`, and the `createIssue` / `closeIssue` /
  `createPullRequest` / `mergePullRequest` / `addComment` mutations.
- **MCP** — `POST /mcp` (JSON-RPC 2.0 Streamable HTTP, revision
  `2026-07-28` via `server/discover`, plus the `initialize` handshake):
  `create_repo`, `get_repo`, `create_issue`, `get_issue`, `list_issues`,
  `create_pull_request`, `list_pull_requests`, `merge_pull_request`.
- `GET /healthz` — `200 {"ok":true}`.

All four surfaces share one in-memory store, so a repo, issue or pull
request created through one surface is visible through the others with the
same ids. State is deterministic: fixed timestamps, sequential ids, no
wall-clock, date or randomness anywhere in stored state.

## Run locally

```sh
MAKEFUN_SHOP_TOKEN=<token> node --experimental-strip-types main.ts
```

`main.ts` needs `$MAKEFUN_ENV_URL` set (the trial harness provides it).
For a quick local smoke test, import `createApp()` from `app.ts` and listen
on a port of your choice.
