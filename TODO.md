# Spawnery — Post-MVP TODO

Items deliberately deferred past the MVP. Each should graduate into its own epic/spec when picked up.

## Personalization
- Typed personalization config in `spawneryapp.yml` (the "what" the user fills at spawn:
  name, topics, tone, etc.) and the filled values in `spawn.yml`.
- Spawn-wizard UI to collect them; injection into the agent session.
- Deferred from E0 (manifest + spawn.yml) and the system design.

## Permissions / consent / egress enforcement
- Manifest `permissions` block (storage scope + egress domain allowlist).
- Consent capture at spawn (snapshot into `spawn.yml` + CP).
- **Network-level enforcement:** per-spawn egress proxy/firewall configured by the node agent
  from the consent record (hard boundary).
- **Interactive layer:** map ACP `session/request_permission` to user consent/escalation UX.
- Re-consent on App-version permission escalation (ties into auto-upgrade guardrails).
- Deferred from E0 §9/§10. MVP relies on first-party launch Apps + open-source inspectability
  + audit (on Spawnery-operated infra) instead of enforced per-spawn permissions.

## Self-hosted control plane (own CP)
- Allow running your own CP instance (not just node agents attaching to the central CP).
- **Tradeoff:** a self-hosted CP cannot be driven from Spawnery's hosted web UI (the UI points
  at the central CP); you'd need your own UI or a way to repoint the client.
- MVP is central-CP only; this is deferred.
