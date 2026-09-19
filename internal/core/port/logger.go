// Package port declares the interfaces the service layer requires from its
// adapters. Everything in this package depends only on core/domain and
// stdlib/third-party value types — never on a concrete adapter, Gin, pgx,
// or an AWS/OpenBao SDK package (§3.2, enforced by .go-arch-lint.yml).
package port

// Logger is the structured logging port this service funnels through: HTTP
// middleware (via platform-gincommon), core services, the SQS consumer,
// the OpenBao client, and every composition root all end up writing through
// the same sink. It matches platform-gincommon's own port.Logger shape
// exactly (Debug/Info/Warn/Error(msg, fields map[string]interface{})), so
// the single Zap-backed logger built once via platform-gincommon's
// logger.NewLogger (cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler)
// satisfies it directly — no adapter needed at the composition root. No
// credential field is ever passed as a value here — the CI secret-logging
// gate (§3.2/§11.4) fails the build if one is.
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}
