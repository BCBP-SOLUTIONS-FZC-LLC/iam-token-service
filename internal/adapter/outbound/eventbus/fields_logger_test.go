package eventbus

// fakeFieldLogger implements port.Logger, recording the fields map passed to
// each call (not just the message) — needed to assert on the trace_id
// stamped by a valid span context, which fakeWarnLogger (message-only)
// cannot observe. Shared by publisher_test.go.
type fakeFieldLogger struct {
	debugCalls []map[string]interface{}
	warnCalls  []map[string]interface{}
}

func (f *fakeFieldLogger) Debug(_ string, fields map[string]interface{}) {
	f.debugCalls = append(f.debugCalls, fields)
}
func (f *fakeFieldLogger) Info(string, map[string]interface{}) {}
func (f *fakeFieldLogger) Warn(_ string, fields map[string]interface{}) {
	f.warnCalls = append(f.warnCalls, fields)
}
func (f *fakeFieldLogger) Error(string, map[string]interface{}) {}
