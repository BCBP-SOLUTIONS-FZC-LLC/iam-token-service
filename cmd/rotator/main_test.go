package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// noopPublisher satisfies outbox.Config.Publisher's required-field
// validation only — never actually invoked (see main.go's doc comment on
// the type), but must not panic if it ever were.
func TestNoopPublisher_PublishAndPublishBatch(t *testing.T) {
	var p noopPublisher
	assert.NoError(t, p.Publish(t.Context(), events.Envelope[json.RawMessage]{}))
	assert.NoError(t, p.PublishBatch(t.Context(), nil))
}

// serveMetricsBriefly's grace-elapses branch: the server starts, the grace
// window passes with no request, and it shuts itself down cleanly.
func TestServeMetricsBriefly_GraceElapsesThenShutsDown(t *testing.T) {
	port := freeTCPPort(t)
	t.Setenv("METRICS_PORT", port)

	done := make(chan struct{})
	go func() {
		serveMetricsBriefly(nil, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetricsBriefly did not return after its grace window elapsed")
	}

	// The listener must be closed post-shutdown — a fresh dial should fail.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected the metrics server to be shut down and unreachable")
	}
}

// serveMetricsBriefly's ListenAndServe-error branch: binding a port already
// in use fails immediately (well before the grace window), exercising the
// errCh receive branch and its logError call (nil logger — must not panic).
func TestServeMetricsBriefly_ListenErrorLogsAndReturns(t *testing.T) {
	// serveMetricsBriefly binds ":<port>" (all interfaces, see main.go) — the
	// blocking listener here must bind the same way, or the two may not
	// actually conflict on every platform (binding a specific host address
	// alongside a wildcard bind is not reliably a conflict everywhere).
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	t.Setenv("METRICS_PORT", port)

	done := make(chan struct{})
	go func() {
		serveMetricsBriefly(nil, 10*time.Second) // long grace — must still return early on bind failure
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetricsBriefly did not return promptly on a bind failure")
	}
}

// freeTCPPort returns a currently-unused local TCP port number as a string,
// by binding then immediately releasing a listener on port 0.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	require.NoError(t, l.Close())
	return port
}
