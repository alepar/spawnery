package metrics

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
)

// RPC server-side Prometheus metrics registered on Registry.
//
// Metric names:
//   - spawnery_rpc_server_requests_total{procedure, code}     — total completed requests.
//   - spawnery_rpc_server_request_duration_seconds{procedure} — request latency histogram.
//   - spawnery_rpc_server_in_flight_requests{procedure}       — currently executing requests.
//
// Label cardinality: "code" is present on the counter but NOT on the duration histogram.
// Adding "code" to the histogram would multiply series by the number of distinct status codes
// per RPC method; keeping it off the histogram bounds cardinality to one series-group per
// procedure. The procedure set is finite (one value per RPC method), so cardinality is safe.
//
// Ordering: RPCInterceptor must be wired OUTERMOST (first in connect.WithInterceptors, before
// RecoverInterceptor). RecoverInterceptor converts panics to *connect.Error values with
// CodeInternal and returns them normally; with metrics outermost that error is observed and
// counted as code="internal". If metrics were placed inside RecoverInterceptor, a panicking
// handler would propagate the panic past the metrics next() call, leaving it uncounted.
// The counter and histogram are recorded only after next() returns normally (not in a defer),
// so an http.ErrAbortHandler re-panic from RecoverInterceptor is simply uncounted rather than
// mislabelled as code="ok". The gauge Dec stays in a defer to keep in-flight always balanced.
var (
	rpcRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spawnery_rpc_server_requests_total",
		Help: "Total RPC server requests completed, by procedure and Connect status code.",
	}, []string{"procedure", "code"})

	rpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "spawnery_rpc_server_request_duration_seconds",
		Help:    "Latency of RPC server requests in seconds, by procedure.",
		Buckets: prometheus.DefBuckets,
	}, []string{"procedure"})

	rpcInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_rpc_server_in_flight_requests",
		Help: "Number of RPC server requests currently in flight, by procedure.",
	}, []string{"procedure"})
)

func init() {
	Registry.MustRegister(rpcRequestsTotal, rpcDuration, rpcInFlight)
}

// RPCInterceptor returns a Connect server-side interceptor that records the three RPC
// Prometheus metrics declared in this file. Wire it OUTERMOST (first argument to
// connect.WithInterceptors, before RecoverInterceptor) so panicked handlers are counted
// as code="internal" after RecoverInterceptor converts the panic to a *connect.Error.
func RPCInterceptor() connect.Interceptor {
	return &rpcInterceptor{}
}

type rpcInterceptor struct{}

func (r *rpcInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		proc := req.Spec().Procedure
		rpcInFlight.WithLabelValues(proc).Inc()
		defer rpcInFlight.WithLabelValues(proc).Dec()
		start := time.Now()
		resp, err := next(ctx, req)
		code := "ok"
		if err != nil {
			code = connect.CodeOf(err).String()
		}
		rpcRequestsTotal.WithLabelValues(proc, code).Inc()
		rpcDuration.WithLabelValues(proc).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// WrapStreamingClient is a no-op: this interceptor records server-side metrics only.
func (r *rpcInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r *rpcInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		proc := conn.Spec().Procedure
		rpcInFlight.WithLabelValues(proc).Inc()
		defer rpcInFlight.WithLabelValues(proc).Dec()
		start := time.Now()
		err := next(ctx, conn)
		code := "ok"
		if err != nil {
			code = connect.CodeOf(err).String()
		}
		rpcRequestsTotal.WithLabelValues(proc, code).Inc()
		rpcDuration.WithLabelValues(proc).Observe(time.Since(start).Seconds())
		return err
	}
}
