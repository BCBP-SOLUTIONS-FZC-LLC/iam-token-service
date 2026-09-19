package realmprovisioner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type spyLogger struct {
	warnCalls []map[string]interface{}
}

func (s *spyLogger) Debug(string, map[string]interface{}) {}
func (s *spyLogger) Info(string, map[string]interface{})  {}
func (s *spyLogger) Warn(_ string, fields map[string]interface{}) {
	s.warnCalls = append(s.warnCalls, fields)
}
func (s *spyLogger) Error(string, map[string]interface{}) {}

// erroringCloseBody makes Close return an error so closeBody's log.Warn
// branch (line 148-150) can be exercised — a real httptest response body
// never fails to close.
type erroringCloseBody struct {
	io.Reader
}

func (erroringCloseBody) Close() error { return errors.New("close boom") }

type fakeRoundTripper struct{}

func (fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       erroringCloseBody{Reader: strings.NewReader("")},
		Header:     make(http.Header),
	}, nil
}

// TestRefreshKeysOnce_InvalidURLReturnsError covers the
// http.NewRequestWithContext error branch — a baseURL containing an ASCII
// control character fails net/url.Parse before any network call is made.
func TestRefreshKeysOnce_InvalidURLReturnsError(t *testing.T) {
	c := &Client{baseURL: "http://example.com/\n", http: http.DefaultClient}
	retryable, err := c.refreshKeysOnce(context.Background(), uuid.New())
	require.Error(t, err)
	assert.False(t, retryable)
}

// TestCloseBody_LogsWarnOnCloseError covers closeBody's log.Warn branch,
// reached when RP-17's response body fails to close.
func TestCloseBody_LogsWarnOnCloseError(t *testing.T) {
	log := &spyLogger{}
	c := &Client{baseURL: "http://example.com", http: &http.Client{Transport: fakeRoundTripper{}}, log: log}

	err := c.RefreshKeys(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Len(t, log.warnCalls, 1)
	assert.Contains(t, log.warnCalls[0]["error"], "close boom")
}
