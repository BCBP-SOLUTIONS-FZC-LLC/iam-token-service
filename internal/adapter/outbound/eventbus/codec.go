package eventbus

import "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"

// Codec and NoopCodec are platform-events' events.Codec — the same types
// iam-user-profile / iam-org-membership inject via events.WithCodec at
// SNS-publish time and wrap with ValidatingCodec at outbox-enqueue time.
// No local Encode-only interface: enqueue validation and Glue wire
// encoding both satisfy the library contract (Encode + Decode).
type (
	Codec     = events.Codec
	NoopCodec = events.NoopCodec
)
