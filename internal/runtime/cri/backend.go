package cri

import (
	"context"
	"fmt"
	"sync"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRIPodBackend runs a spawn pod as one CRI sandbox (handler=runsc) with two containers
// (sidecar + agent) sharing the pod network namespace. Implements runtime.PodBackend.
type CRIPodBackend struct {
	c              *Client
	runtimeHandler string // e.g. "runsc"

	mu          sync.Mutex
	sandboxCfgs map[string]*runtimeapi.PodSandboxConfig // sandboxID -> config (CreateContainer needs it)
}

// NewCRIPodBackend builds a backend over a Client, running pods under runtimeHandler.
func NewCRIPodBackend(c *Client, runtimeHandler string) *CRIPodBackend {
	return &CRIPodBackend{c: c, runtimeHandler: runtimeHandler, sandboxCfgs: map[string]*runtimeapi.PodSandboxConfig{}}
}

// Ping checks the CRI runtime is reachable.
func (b *CRIPodBackend) Ping(ctx context.Context) error {
	_, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	return err
}

// Preflight asserts the runtime + network are ready (caught at startup, not first spawn).
func (b *CRIPodBackend) Preflight(ctx context.Context) error {
	resp, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return fmt.Errorf("cri status: %w", err)
	}
	for _, cond := range resp.GetStatus().GetConditions() {
		if (cond.Type == "RuntimeReady" || cond.Type == "NetworkReady") && !cond.Status {
			return fmt.Errorf("cri not ready: %s (%s)", cond.Type, cond.Reason)
		}
	}
	return nil
}
