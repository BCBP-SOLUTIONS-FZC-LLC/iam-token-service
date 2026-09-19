package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestValidPlatformAutomationClientID(t *testing.T) {
	tenantID := uuid.New()
	other := uuid.New()

	assert.True(t, ValidPlatformAutomationClientID("platform-automation", tenantID),
		"the literal base name (a dedicated-realm tenant) must be accepted")
	assert.True(t, ValidPlatformAutomationClientID("platform-automation-"+tenantID.String(), tenantID),
		"the tenant-scoped name (a shared-realm/trial tenant) must be accepted")

	assert.False(t, ValidPlatformAutomationClientID("platform-automation-"+other.String(), tenantID),
		"a different tenant's scoped name must be rejected")
	assert.False(t, ValidPlatformAutomationClientID("not-platform-automation", tenantID))
	assert.False(t, ValidPlatformAutomationClientID("", tenantID))
}
