package eventbus

import (
	"embed"
	"fmt"
	"path"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// schemasFS embeds the JSON Schema Draft-07 files under this package's
// sibling schemas/ directory. schema-gov extract 0.4 writes one snake_case
// file per AsyncAPI *Payload (produced and consumed) — see schemaFileNames
// below for the produced/consumed split. Mirrors iam-org-membership's
// eventbus package layout.
//
//go:embed schemas/*.json
var schemasFS embed.FS

// schemaFileNames maps every embedded schemas/*.json filename to its
// PascalCase event / Glue schema name (§25) — the same hand-maintained
// filename→PascalCase-name convention as iam-org-membership's eventbus
// package. A new schema file needs one entry added here.
var schemaFileNames = map[string]string{
	"service_account_registered.json":         domain.EventServiceAccountRegistered,
	"service_account_credential_issued.json":  domain.EventServiceAccountCredentialIssued,
	"service_account_credential_rotated.json": domain.EventServiceAccountCredentialRotated,
	"service_account_credential_revoked.json": domain.EventServiceAccountCredentialRevoked,
	"service_account_revoked.json":            domain.EventServiceAccountRevoked,
	// TenantMembershipsPurged is this service's one *consumed* event
	// (api/asyncapi.yaml) — kept here only for schema-gov coverage (LLD
	// §16), never produced/registered in Glue by this service (see
	// isProducedEvent / ProducedSchemas below).
	"tenant_memberships_purged.json": "TenantMembershipsPurged",
}

// eventTypeFromSchemaFile returns the PascalCase event/Glue schema name for
// an embedded schema filename via schemaFileNames, falling back to the
// filename stem for any file not listed there.
func eventTypeFromSchemaFile(filename string) string {
	if name, ok := schemaFileNames[filename]; ok {
		return name
	}
	return strings.TrimSuffix(filename, ".json")
}

// isProducedEvent reports whether name is one of this service's 5 frozen
// published events (§25) — as opposed to TenantMembershipsPurged, its one
// consumed event.
func isProducedEvent(name string) bool {
	switch name {
	case domain.EventServiceAccountRegistered,
		domain.EventServiceAccountCredentialIssued,
		domain.EventServiceAccountCredentialRotated,
		domain.EventServiceAccountCredentialRevoked,
		domain.EventServiceAccountRevoked:
		return true
	default:
		return false
	}
}

// ProducedSchemas returns the raw JSON Schema bytes for every produced
// event, keyed by PascalCase event/Glue schema name (§25) — used for Glue
// prefetch (cmd/server/main.go's schemaNames) and the event-contract test
// suite (test/contract). Excludes TenantMembershipsPurged (consumed, not
// produced) so Glue prefetch can never request a schema this service does
// not register.
func ProducedSchemas() (map[string][]byte, error) {
	entries, err := schemasFS.ReadDir("schemas")
	if err != nil {
		return nil, fmt.Errorf("eventbus: read embedded schemas dir: %w", err)
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := eventTypeFromSchemaFile(e.Name())
		if !isProducedEvent(name) {
			continue
		}
		data, rerr := schemasFS.ReadFile(path.Join("schemas", e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("eventbus: read embedded schema %s: %w", e.Name(), rerr)
		}
		out[name] = data
	}
	return out, nil
}
