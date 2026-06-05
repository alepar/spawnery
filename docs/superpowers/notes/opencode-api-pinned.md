# opencode API — pinned shapes (Task 0 grounding)

`OPENCODE_VERSION = 1.15.13` (binary at `~/.opencode/bin/opencode`). OpenAPI 3.1, 113 paths,
served at `GET /doc`. Captured 2026-06-05 against `opencode serve --port 4097`.

## Endpoints we use (all present on 1.15.13)
- `GET  /global/health` → `{"healthy":true,"version":"1.15.13"}`
- `GET  /doc` → OpenAPI 3.1
- `GET,POST /session` — list / create. Create body `{"title":...}` (optional).
- `POST /session/{sessionID}/prompt_async` — async prompt (preferred); returns 204.
- `POST /session/{sessionID}/abort` — cancel in-flight turn.
- `GET  /session/{sessionID}/message` — backfill on reconnect.
- `POST /session/{sessionID}/permissions/{permissionID}` — body `{"response":"once"|"always"|"reject"}`.
- `GET  /event` — SSE stream.
- (also exists, not used yet) `GET /permission`, `POST /permission/{requestID}/reply`,
  `POST /session/{sessionID}/shell` (← confirms TUI can shell = audit concern),
  `EventPty*` (opencode has its own PTY events — relevant to Phase 2, out of scope now).

## Session object
`POST /session {"title":"probe"}` →
```json
{"id":"ses_16964b766ffefoUxQsEbZ5oJG2","slug":"...","projectID":"global",
 "directory":"/tmp","path":"tmp","title":"probe","version":"1.15.13",
 "time":{"created":...,"updated":...},"cost":0,"tokens":{...}}
```
- Session id is `ses_…`.
- **Sessions are scoped to `directory` (cwd) + `projectID`.** `GET /session` returns ALL
  sessions across directories. So **discover-or-create MUST filter by the spawn's working
  directory** (`/app`), else it reuses an unrelated session. Set opencode's cwd to `/app`.

## Event envelope (SSE `data:` lines)
Every event is `{ "type": "<name>", "properties": { <payload> } }` (payload is nested under
`properties`, NOT flat). Relevant `type` discriminator values:
- `server.connected` — first event.
- `message.part.updated` — properties `{sessionID, part: Part, time}`. Full part snapshot.
- `message.part.delta` — properties `{sessionID, messageID, partID, field, delta}`. The
  streaming increment. `field` is the part field being appended (e.g. "text"); `delta` is the
  chunk. **delta does NOT carry the part type** → must map partID→type from a prior
  `message.part.updated` to know agent-message vs reasoning.
- `permission.asked` — properties = PermissionRequest (below).
- `permission.replied` (`EventPermissionReplied`).
- `session.idle` — properties `{...}` (turn end).
- `session.updated`, `session.created`, etc.

### Part union (message.part.updated `part`)
`TextPart{type:"text", id:"prt_…", sessionID, messageID, text, ...}`,
`ReasoningPart{type:"reasoning", ...}`, `ToolPart`, `FilePart`, `StepStart/FinishPart`,
`SubtaskPart`, `SnapshotPart`, `PatchPart`, `AgentPart`, `RetryPart`, `CompactionPart`.

### PermissionRequest (permission.asked payload)
```json
{"id":"per_…",          // the permissionID for POST .../permissions/{permissionID}
 "sessionID":"ses_…",
 "permission":"<string>",// permission kind
 "patterns":[...],"metadata":{...},"always":[...],
 "tool":{"messageID":"msg_…","callID":"..."}}
```
**No options array** — opencode's permission answer is `{"response":"once|always|reject"}`.
The adapter SYNTHESIZES the four canonical ACP option kinds and maps the chosen optionId:
`allow_once→once`, `allow_always→always`, `reject_*→reject`.

## prompt_async body
`{"parts":[{"type":"text","text":"..."}], "model":{"providerID":"...","modelID":"..."},
  "agent":"...", "noReply":bool}`. `model` required for a real run → adapter passes the
spawn's model; provider must be configured in opencode (entrypoint, via the sidecar endpoint).

## Mapping summary (opencode → canonical ACP)
| opencode | ACP |
|---|---|
| `message.part.delta` field=text, partID→TextPart | `session/update` `agent_message_chunk{content.text=delta}` |
| `message.part.delta` field=text, partID→ReasoningPart | `session/update` `agent_thought_chunk` |
| `message.part.updated` part=tool | (later) tool frames |
| `permission.asked` | `session/request_permission` + synthesized ACP options |
| `session.idle` | turn end (respond to in-flight session/prompt) |
| `session.status` busy (not node-initiated) | synthesized turn-busy session/update |

## SDK vs REST decision
Use raw REST (net/http) in `internal/opencode` against these pinned shapes — the Go SDK lags
and we need `prompt_async` + `message.part.delta`. `/doc` is the contract.
