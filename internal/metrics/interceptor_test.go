// Package metrics (internal test): we access unexported vars directly so we can snapshot
// metric values and compare deltas without exporting the vecs.
//
// Tests are NOT t.Parallel() because the collectors are process-global and monotonic.
// Counter and histogram assertions use before/after deltas (testutil.ToFloat64,
// histSampleCount) to tolerate accumulation across other tests in the same binary.
// Each test uses a distinct procedure label string to avoid cross-test interference.
package metrics

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/types/known/emptypb"
)

// histSampleCount returns the accumulated sample count of a histogram Observer.
// Uses the dto wire representation because testutil.ToFloat64 panics for histograms.
func histSampleCount(obs prometheus.Observer) uint64 {
	ch := make(chan prometheus.Metric, 1)
	obs.(prometheus.Collector).Collect(ch)
	close(ch)
	m := <-ch
	pb := &dto.Metric{}
	_ = m.Write(pb)
	return pb.GetHistogram().GetSampleCount()
}

// fakeUnaryRequest is a minimal connect.AnyRequest for interceptor unit tests.
// It overrides Spec() to return a configurable procedure; all other methods fall through
// to the nil embedded interface (safe as long as the test handler does not call them).
type fakeUnaryRequest struct {
	connect.AnyRequest
	proc string
}

func (f *fakeUnaryRequest) Spec() connect.Spec { return connect.Spec{Procedure: f.proc} }

// stubMetricsConn is a minimal connect.StreamingHandlerConn for interceptor tests.
// It returns a Spec with the given procedure name.
type stubMetricsConn struct {
	connect.StreamingHandlerConn
	proc string
}

func (s *stubMetricsConn) Spec() connect.Spec { return connect.Spec{Procedure: s.proc} }

func TestRPCInterceptor_UnaryOK(t *testing.T) {
	const proc = "/metrics_test.v1.Service/UnaryOK"
	ic := RPCInterceptor()

	beforeReqs := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "ok"))
	beforeCount := histSampleCount(rpcDuration.WithLabelValues(proc))

	handler := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	wrapped := ic.WrapUnary(handler)

	resp, err := wrapped(context.Background(), &fakeUnaryRequest{proc: proc})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	if d := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "ok")) - beforeReqs; d != 1 {
		t.Errorf("requests{ok} delta = %v, want 1", d)
	}
	if d := histSampleCount(rpcDuration.WithLabelValues(proc)) - beforeCount; d != 1 {
		t.Errorf("duration sample count delta = %v, want 1", d)
	}
}

func TestRPCInterceptor_UnaryError(t *testing.T) {
	const proc = "/metrics_test.v1.Service/UnaryError"
	ic := RPCInterceptor()

	beforeReqs := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "invalid_argument"))

	handler := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad input"))
	})
	wrapped := ic.WrapUnary(handler)

	_, err := wrapped(context.Background(), &fakeUnaryRequest{proc: proc})
	if err == nil {
		t.Fatal("expected non-nil error from handler")
	}

	if d := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "invalid_argument")) - beforeReqs; d != 1 {
		t.Errorf("requests{invalid_argument} delta = %v, want 1", d)
	}
}

func TestRPCInterceptor_Streaming(t *testing.T) {
	const proc = "/metrics_test.v1.Service/StreamOK"
	ic := RPCInterceptor()

	beforeReqs := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "ok"))

	handler := connect.StreamingHandlerFunc(func(_ context.Context, _ connect.StreamingHandlerConn) error {
		return nil
	})
	wrapped := ic.WrapStreamingHandler(handler)

	if err := wrapped(context.Background(), &stubMetricsConn{proc: proc}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if d := testutil.ToFloat64(rpcRequestsTotal.WithLabelValues(proc, "ok")) - beforeReqs; d != 1 {
		t.Errorf("streaming requests{ok} delta = %v, want 1", d)
	}
}

func TestRPCInterceptor_InFlightBalance(t *testing.T) {
	const proc = "/metrics_test.v1.Service/InFlight"
	ic := RPCInterceptor()

	before := testutil.ToFloat64(rpcInFlight.WithLabelValues(proc))

	handler := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	wrapped := ic.WrapUnary(handler)

	_, _ = wrapped(context.Background(), &fakeUnaryRequest{proc: proc})

	if d := testutil.ToFloat64(rpcInFlight.WithLabelValues(proc)) - before; d != 0 {
		t.Errorf("in-flight gauge delta = %v after call, want 0 (Inc then deferred Dec)", d)
	}
}
