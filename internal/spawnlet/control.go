package spawnlet

// SidecarControlTokenEnv is the sidecar env var carrying the per-pod bearer for the sidecar's
// /control/model endpoint (the runtime model-switch control plane, sp-bp9w). The sidecar container
// never stops across a spawnlet restart, so re-adoption reads this back out of the still-running
// sidecar's own env (internal/runtime.PodBackend.ContainerEnv) rather than minting a new one — a
// fresh token here would not match what the sidecar is actually enforcing.
const SidecarControlTokenEnv = "SIDECAR_CONTROL_TOKEN"

// SidecarSpawnIDEnv is the sidecar env var carrying THIS spawn's id. The sidecar tags its own
// telemetry/log lines with it and uses it to sanity-check the credentials the node PUSHES to
// /control/github (sp-2tx8.9). Injected for every spawn with a github control server configured.
const SidecarSpawnIDEnv = "SIDECAR_SPAWN_ID"

// SidecarCABundleMountPath is the container DIRECTORY where a node-provided merged CA bundle (system
// roots + an extra trusted CA) is bind-mounted into the SIDECAR, read-only, when
// ManagerConfig.SidecarCABundleFile is set (sp-wwtc.3). It is its OWN directory — never
// /etc/spawnery/pki, which also holds host PKI private keys the sidecar must never see.
const SidecarCABundleMountPath = "/run/spawnery/sidecar-ca-bundle"

// SidecarCABundleFileEnv is the sidecar env var pointing Go's x509.SystemCertPool at the merged CA
// bundle (SSL_CERT_FILE REPLACES the pool rather than appending — the mounted file must already be a
// system-roots+extra-CA merge, not the extra CA alone). Only injected when
// ManagerConfig.SidecarCABundleFile is set; empty/unset leaves the sidecar's own image system roots
// untouched (the production default — see ManagerConfig.SidecarCABundleFile).
const SidecarCABundleFileEnv = "SSL_CERT_FILE"
