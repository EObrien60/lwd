# lwd Phase 9b — dumb agent + node health + node UX (v2)

**Status:** Design (decisions resolved)
**Date:** 2026-07-05
**Builds on:** P9a (merged). North star: `docs/VISION.md`.

## Goal

Add the **dumb `lwd-agent`** as the preferred node transport (docker-over-SSH from 9a
stays the fallback), report node **health/reachability**, and surface **node UX** in
`lwd-web` and `lwd-mcp` (currently CLI/API-only). Capacity reporting is deferred to
P11 (the scheduler consumes it).

## Decisions

1. **Agent = dumb HTTP server** exposing the Node *primitives* over an authed API
   (bearer `LWD_AGENT_TOKEN`), bound to the node's mesh interface. Talks to the node's
   local Docker (`node.NewLocal()`). No build/compose/scheduling/orchestration.
2. **Transport selection (per node):** if the node has an agent URL and its `/healthz`
   pings OK → `agentNode`; else fall back to 9a's `sshDocker`. `lwd node add … [--agent
   <url>]`.
3. **Capacity → P11.** P9b reports only health/reachability (up/down + docker reachable).
4. **Full node UX:** lwd-web Nodes view (list, reachability, add/remove) + node picker on
   deploy; lwd-mcp `lwd_node_list/add/remove` + a `node` arg on `lwd_apply`/`lwd_deploy_git`.

## Components

- **`cmd/lwd-agent/main.go` + `internal/agent/server.go`** — HTTP server. Config (env):
  `LWD_AGENT_TOKEN` (required), `LWD_AGENT_ADDR` (default `:8078`, operator binds the
  mesh IP). Endpoints (all authed, constant-time bearer check; JSON, + a log/stream and
  an image-load stream): `GET /healthz` (docker reachable? → 200/503), and one endpoint
  per Node primitive the controller calls (`ensure-image`, `run`, `remove`, `list`,
  `logs` (stream), `image-present`, `load` (tar stream in), `ensure-network`,
  `connect-network`, `container-health`, `health`). Delegates to a local `node.Node`
  (NewLocal). Dumb: it just executes.
- **`internal/node/agent.go`** — `agentNode` implementing `node.Node` as an HTTP client
  to the agent (mirrors the endpoints; `LoadImage` streams the tar to `/load`;
  `ContainerLogs` proxies the stream). `NewAgentNode(baseURL, token) *agentNode`.
- **`internal/node` resolver** — a node record gains `AgentURL`. `RegistryResolver`
  picks `agentNode` when `AgentURL != ""` and `NewAgentNode(...).Ping()` succeeds
  (cached), else `sshDocker` (9a). A small `Reachable(name) (transport string, ok bool)`
  for status.
- **`internal/store`** — `nodes` table gains an `agent_url TEXT NOT NULL DEFAULT ''`
  column (idempotent migration); `store.Node.AgentURL`.
- **API/CLI** — `lwd node add <name> <ssh-host> <mesh-addr> [--agent <url>]`; `POST
  /nodes` accepts `agent_url`; `GET /nodes` / `lwd node ls` show transport +
  reachability (ping each node: agent `/healthz` or ssh `docker version`).
- **lwd-web** — a Nodes view (list with reachability + transport, add form incl. optional
  agent URL, remove) and a node `<select>` in the deploy modal (from `GET /nodes`, plus
  `local`); the generated app config includes `node = "..."`.
- **lwd-mcp** — `lwd_node_list` / `lwd_node_add` / `lwd_node_remove` tools; a `node`
  arg on `lwd_apply`/`lwd_deploy_git` (validated by the daemon).

## Security

- Agent API: constant-time bearer token; bound to the mesh interface (never public);
  the mesh provides encryption + isolation. The agent executes Docker ops = effectively
  root-on-the-node — so the token + mesh-only binding are the trust boundary (documented).
- The image-load stream carries image layers, not secrets; secret values reach the node
  only as container env at `run` time (over the authed API). No tool/endpoint returns a
  secret value.
- SSH fallback unchanged (box ssh auth).

## Testing

- `internal/agent` handlers tested against a fake local `node.Node` (auth required; each
  endpoint delegates correctly; `/healthz`).
- `internal/node` `agentNode` tested against an `httptest` server mimicking the agent
  (each Node op round-trips; load/logs streams).
- Resolver: agent-preferred-when-healthy, ssh-fallback-when-agent-absent/unreachable
  (fake ping).
- API/CLI/web/MCP node UX: node add with agent_url; ls reachability; web nodes list +
  deploy picker generate `node=`; MCP node tools + `node` arg.
- e2e (guarded): run a real `lwd-agent` bound to loopback, register a node pointing at
  it, deploy an app through the agent transport, assert reachable; ssh fallback path
  covered by 9a's e2e.

## Out of scope (later)

- Capacity/resource reporting (P11). Continuous reconciliation (P10). Agent-run build
  (stays on the controller). mTLS for the agent (token+mesh for now).
