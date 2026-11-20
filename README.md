# github-core-clone-01 v9 — Go binary forge

A runnable starting point for [the contest](https://make.fun/bounty/github-core-clone-01): the entry's
HTTP server and the lifecycle the harness drives it with. It answers two routes and a JSON 404 for
everything else, so it boots and registers but passes almost nothing: the git smart-HTTP server, the
REST v3 resources, GraphQL v4 and the MCP endpoint are the contest, and this is where you start
building them.

This entry is the v9 resubmission: the whole forge reimplemented as a single
statically-linked Go binary (`go/forge`, built from the `go/` sources in this
repo). It is byte-identical in behavior to the TypeScript reference — the same
git smart-HTTP v2/v0, REST v3, GraphQL v4 and MCP surface, the same
deterministic state export — with the reference's structure kept and its
runtime replaced.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript types on its
own; the flag is harmless there and makes an older local Node work too. There is nothing to install:
the program imports `node:child_process` and its own `main.ts` only, and the trial has no package manager and
no internet. `main.ts` launches `./go/forge` and forwards its stdio and signals; the Go binary is the
server.

## The lifecycle (main.ts + go/forge)

The env is a server the harness starts beside yours. Its origin is in `MAKEFUN_ENV_URL`
(`http://store:8080` in a trial).

1. **Bind an ephemeral port.** The Go server listens on port 0 and lets the kernel pick one; read it with
   the listener's address. Never bind 8080: that is the env's port, and on the one-pod layout it is
   on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port you bound>}`, as JSON. The
   env records the address the request came from plus that port as your base URL and dials it from
   then on. The first register wins: a later one is answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with `{"done":false}` while the
   suite runs and `{"done":true}` once its export is written. Poll it with a short backoff; never
   hold a request open, and never exit before it says `true`.
4. **Exit 0** when it does. Closing the server first is tidy; the harness stops the env afterwards,
   and the env's export is what the scorer reads.

A failed poll or register is retried with a short exponential backoff. If `MAKEFUN_ENV_URL` is unset
there is nothing to talk to, so the forge exits 1 at once.

## The surface the contest scores

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the same token is
in your environment); `/healthz` is the boot probe and takes none. Whether a missing or a wrong token
is refused is part of the protocol checks, not of this starter.

The score is `feature + agent + perf` (0..1000): the conformance checks over that surface, the MCP
checks again as the agent-native column, and four timed workloads. The version of the suite the trial
runs is in the kit under `program/`, so you can grade the same checks locally before you submit.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the contest page:
**https://make.fun/bounty/github-core-clone-01** (its Rules section). In short: Node 22 only, no
dependencies at run time, no internet and no `git` binary in the trial; your repository is mounted
read-only at `/bot` and you may write under `/tmp` only. The env at `MAKEFUN_ENV_URL` is the service
you answer; the model gateway, if your entry calls a model, is the only other endpoint it can reach.
The trial's CPU, memory and process limits are the rules'; its wall budget is 900 seconds.

## Layout

- `main.ts`: the thin launcher above. Keep it as it is.
- `go/`: the forge itself — `app.go` (server + router), `gitproto.go` (smart HTTP),
  `rest.go` (REST v3), `graphql.go` (GraphQL v4), `mcp.go` (MCP), `store.go` (git objects
  + the data model), `http.go` + `jval.go` + `util.go` (helpers), `main.go` (lifecycle).
- `go/forge`: the built static binary (CGO_ENABLED=0, linux/amd64).
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata and licence.

This entry is MIT licensed; a submitted entry must carry an OSI-approved licence of its own.
