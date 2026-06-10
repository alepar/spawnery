package main

import (
	"context"
	"fmt"
	"log"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// formatSetModelResult renders the CP's SetSpawnModel response: the active model plus whether the
// change reached the live agent (applied) or is only persisted pending reconciliation.
func formatSetModelResult(model string, applied bool) string {
	if applied {
		return fmt.Sprintf("model set to %s (applied)", model)
	}
	return fmt.Sprintf("model set to %s (saved; pending — agent not yet switched)", model)
}

// setSpawnModel calls the CP SetSpawnModel RPC and returns the authoritative active model and whether
// it applied to the live agent. Mirrors list.go's listSpawns: builds the gRPC client with the bearer
// interceptor and log.Fatalf on RPC error.
func setSpawnModel(cpAddr, token, spawnID, model string) (string, bool) {
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))
	resp, err := client.SetSpawnModel(context.Background(), connect.NewRequest(&cpv1.SetSpawnModelRequest{
		SpawnId: spawnID,
		Model:   model,
	}))
	if err != nil {
		log.Fatalf("set model: %v", err)
	}
	return resp.Msg.GetModel(), resp.Msg.GetApplied()
}

// setModelCmd sets the inference model of a running spawn: persists it on the CP and (best-effort)
// switches the live agent mid-session via the sidecar override.
func setModelCmd() *cli.Command {
	return &cli.Command{
		Name:      "set-model",
		Usage:     "set the inference model of a spawn (persist + live-switch)",
		ArgsUsage: "<spawn-id> <openrouter-model-id>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token (CP)"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			args := c.Args()
			if args.Len() != 2 {
				return cli.Exit("usage: spawnctl set-model <spawn-id> <openrouter-model-id>", 2)
			}
			model, applied := setSpawnModel(c.String("cp"), c.String("token"), args.Get(0), args.Get(1))
			fmt.Println(formatSetModelResult(model, applied))
			return nil
		},
	}
}
