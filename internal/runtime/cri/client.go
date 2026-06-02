// Package cri implements a runtime.PodBackend over the containerd CRI gRPC API.
package cri

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Client wraps the CRI RuntimeService + ImageService gRPC clients on one connection.
type Client struct {
	conn    *grpc.ClientConn
	runtime runtimeapi.RuntimeServiceClient
	image   runtimeapi.ImageServiceClient
}

// NewClient builds a Client from an existing gRPC connection (used by tests with bufconn).
func NewClient(conn *grpc.ClientConn) *Client {
	return &Client{
		conn:    conn,
		runtime: runtimeapi.NewRuntimeServiceClient(conn),
		image:   runtimeapi.NewImageServiceClient(conn),
	}
}

// Dial connects to a CRI endpoint, e.g. "unix:///run/containerd/containerd.sock".
func Dial(endpoint string) (*Client, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cri dial %s: %w", endpoint, err)
	}
	return NewClient(conn), nil
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }
