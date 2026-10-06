package eventbus

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A schema file not listed in schemaFileNames falls back to its filename
// stem, and a listed one maps to its PascalCase event name.
func TestEventTypeFromSchemaFile_ListedAndFallback(t *testing.T) {
	assert.Equal(t, "ServiceAccountRevoked", eventTypeFromSchemaFile("service_account_revoked.json"))
	assert.Equal(t, "some_future_event", eventTypeFromSchemaFile("some_future_event.json"))
}

const validSchemaJSON = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`

// Sub-directories and non-.json files under schemas/ are skipped; every
// .json file is compiled and validates under its event-type name.
func TestNewValidatingCodecFromFS_SkipsDirsAndNonJSON(t *testing.T) {
	fsys := fstest.MapFS{
		"schemas/thing.json":        {Data: []byte(validSchemaJSON)},
		"schemas/README.md":         {Data: []byte("not a schema")},
		"schemas/nested/other.json": {Data: []byte(validSchemaJSON)},
	}
	c, err := newValidatingCodecFromFS(nil, fsys)
	require.NoError(t, err)
	assert.Len(t, c.schemas, 1, "only the top-level .json file is compiled")
	require.Contains(t, c.schemas, "thing")

	require.NoError(t, c.Validate("thing", []byte(`{"a":"x"}`)))
	assert.Error(t, c.Validate("thing", []byte(`{}`)))

	_, _, err = c.Encode(context.Background(), "thing", []byte(`{"a":"x"}`))
	require.NoError(t, err, "a nil inner became events.NoopCodec")
}

func TestNewValidatingCodecFromFS_MissingSchemasDirErrors(t *testing.T) {
	_, err := newValidatingCodecFromFS(nil, fstest.MapFS{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read embedded schemas dir")
}

func TestNewValidatingCodecFromFS_InvalidJSONErrors(t *testing.T) {
	_, err := newValidatingCodecFromFS(nil, fstest.MapFS{"schemas/broken.json": {Data: []byte("{not json")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse embedded schema broken")
}

// Two files resolving to the same event-type name (a schemaFileNames entry
// and a stray file named after its PascalCase target) cannot both be
// registered with the compiler.
func TestNewValidatingCodecFromFS_DuplicateResourceErrors(t *testing.T) {
	fsys := fstest.MapFS{
		"schemas/ServiceAccountRevoked.json":   {Data: []byte(validSchemaJSON)},
		"schemas/service_account_revoked.json": {Data: []byte(validSchemaJSON)},
	}
	_, err := newValidatingCodecFromFS(nil, fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add schema resource ServiceAccountRevoked")
}

func TestNewValidatingCodecFromFS_UncompilableSchemaErrors(t *testing.T) {
	_, err := newValidatingCodecFromFS(nil, fstest.MapFS{"schemas/bad.json": {Data: []byte(`{"type":5}`)}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile schema bad")
}

// unreadableFS lists its entries normally but fails every file read, so the
// ReadFile branch can be hit (fstest.MapFS can't list a file it can't read).
type unreadableFS struct{ fstest.MapFS }

var errUnreadable = errors.New("unreadable")

func (u unreadableFS) ReadFile(string) ([]byte, error) { return nil, errUnreadable }

var _ fs.ReadFileFS = unreadableFS{}

func TestNewValidatingCodecFromFS_ReadFileErrors(t *testing.T) {
	_, err := newValidatingCodecFromFS(nil, unreadableFS{fstest.MapFS{"schemas/x.json": {Data: []byte(validSchemaJSON)}}})
	require.ErrorIs(t, err, errUnreadable)
	assert.Contains(t, err.Error(), "read embedded schema x.json")
}

// A definition that is not valid JSON fails before any Glue call (the nil
// client would panic if one were made).
func TestGlueCodec_fetchVersionID_InvalidEmbeddedDefinitionErrors(t *testing.T) {
	g := &GlueCodec{registryName: "iam-serviceaccount-events", definitions: map[string][]byte{"X": []byte("{not json")}}
	_, err := g.fetchVersionID(context.Background(), "X")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "X"`)
}
