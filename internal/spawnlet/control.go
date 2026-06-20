package spawnlet

// SidecarControlMountPath is the container path where the GetToken UDS directory is bind-mounted
// into the SIDECAR (not the agent) in the userns-remap lane (sp-n7iy.3 §2.4). The sidecar
// dials gettoken.sock within this dir to pull real GitHub tokens from the node's credential server.
const SidecarControlMountPath = "/run/spawnery/control"

// SidecarControlSocketName is the filename of the GetToken unix-domain socket within
// SidecarControlMountPath (and the host-side control dir). Full sidecar path:
// SidecarControlMountPath + "/" + SidecarControlSocketName.
const SidecarControlSocketName = "gettoken.sock"

// SidecarGetTokenUDSEnv is the sidecar env var (userns-remap lane) pointing at the GetToken UDS
// socket path inside the sidecar container.
const SidecarGetTokenUDSEnv = "SIDECAR_GETTOKEN_UDS"

// SidecarGetTokenAddrEnv is the sidecar env var (TCP lane) for the node's GetToken TCP listener
// address ("host:port").
const SidecarGetTokenAddrEnv = "SIDECAR_GETTOKEN_ADDR"

// SidecarGetTokenBearerEnv is the sidecar env var (TCP lane) for the per-spawn bearer token.
const SidecarGetTokenBearerEnv = "SIDECAR_GETTOKEN_BEARER"

// SidecarSpawnIDEnv is the sidecar env var carrying THIS spawn's id, which the sidecar puts in its
// GetSpawnCA/GetToken requests to the node credential server (internal/sidecar reads
// getenv("SIDECAR_SPAWN_ID")). Unset → the sidecar requests spawn id "", so the node mints a CA/token
// for the WRONG (empty) spawn — the CA the proxy presents won't match the agent's trusted git-env CA
// ("certificate signer not trusted") and token resolution fails. MUST be injected for github spawns.
const SidecarSpawnIDEnv = "SIDECAR_SPAWN_ID"
