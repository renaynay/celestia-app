package fibre

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/celestiaorg/celestia-app/v10/x/fibre/types"
	pebbledb "github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func testObjectStorageConfig() ObjectStorageConfig {
	return ObjectStorageConfig{
		Endpoint: "https://account.r2.cloudflarestorage.com",
		Region:   "auto",
		Bucket:   "fibre-shards",
		Prefix:   "fibre",
		ReadRPS:  10,
	}
}

func TestObjectStorageConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*ObjectStorageConfig)
	}{
		{"endpoint missing", func(c *ObjectStorageConfig) { c.Endpoint = "" }},
		{"endpoint relative", func(c *ObjectStorageConfig) { c.Endpoint = "/r2" }},
		{"endpoint scheme", func(c *ObjectStorageConfig) { c.Endpoint = "ftp://r2.example" }},
		{"endpoint malformed", func(c *ObjectStorageConfig) { c.Endpoint = "https://%" }},
		{"endpoint credentials", func(c *ObjectStorageConfig) { c.Endpoint = "https://user:secret@r2.example" }},
		{"region", func(c *ObjectStorageConfig) { c.Region = "" }},
		{"bucket", func(c *ObjectStorageConfig) { c.Bucket = "" }},
		{"prefix", func(c *ObjectStorageConfig) { c.Prefix = "" }},
		{"prefix slashes", func(c *ObjectStorageConfig) { c.Prefix = "///" }},
		{"zero RPS", func(c *ObjectStorageConfig) { c.ReadRPS = 0 }},
		{"negative RPS", func(c *ObjectStorageConfig) { c.ReadRPS = -1 }},
		{"NaN RPS", func(c *ObjectStorageConfig) { c.ReadRPS = math.NaN() }},
		{"infinite RPS", func(c *ObjectStorageConfig) { c.ReadRPS = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testObjectStorageConfig()
			tc.modify(&cfg)
			require.Error(t, cfg.Validate())
		})
	}
	cfg := testObjectStorageConfig()
	require.NoError(t, cfg.Validate())
	cfg.Endpoint = "http://localhost:9000"
	require.NoError(t, cfg.Validate())
}

func TestStoreConfigValidateBackend(t *testing.T) {
	cfg := DefaultStoreConfig()
	cfg.Path = t.TempDir()
	cfg.StorageBackend = "unknown"
	require.ErrorContains(t, cfg.Validate(), "storage_backend")
	cfg.StorageBackend = "object"
	require.ErrorContains(t, cfg.Validate(), "object_storage.endpoint")
	cfg.ObjectStorage = testObjectStorageConfig()
	require.NoError(t, cfg.Validate())
}

func TestStoreOpensObjectStorage(t *testing.T) {
	// Dummy credentials keep the test independent of the operator's AWS profile.
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	var (
		cfg         = DefaultStoreConfig()
		commitment  = generateCommitment()
		promiseHash = []byte{1}
	)
	cfg.Path = t.TempDir()
	cfg.StorageBackend = "object"
	cfg.ObjectStorage = testObjectStorageConfig()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "PUT", r.Method)
		assert.Equal(t, "*", r.Header.Get("If-None-Match"))
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Len(t, data, int(r.ContentLength))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg.ObjectStorage.Endpoint = server.URL
	cfg.ChainID = "test-chain"
	cfg.ValidatorAddress = "test-validator"

	store, err := NewStore(cfg)
	require.NoError(t, err)
	require.Same(t, store.object, store.shards)
	require.Equal(t, objectBackendTag, store.shardsBackend)
	object := store.object.(*objectBackend)
	require.Equal(t, rate.Limit(10), object.readLimiter.Limit())
	require.Equal(t, 1, object.readLimiter.Burst())
	require.Equal(t, "fibre/test-chain/test-validator/shards/"+commitment.String()+"-01", object.objectKey(commitment, promiseHash))
	created, err := object.Put(t.Context(), commitment, promiseHash, &types.BlobShard{
		Rows: []*types.BlobRow{{Index: 1, Data: []byte("data")}},
	})
	require.NoError(t, err)
	require.True(t, created)
	options := object.client.(*s3.Client).Options()
	require.Equal(t, cfg.ObjectStorage.Endpoint, *options.BaseEndpoint)
	require.Equal(t, "auto", options.Region)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), encodeShardMarkerForBackend(objectBackendTag, 1), pebbledb.Sync))
	require.NoError(t, store.Close())

	cfg.StorageBackend = "local"
	retainedConfig := cfg.ObjectStorage
	cfg.ObjectStorage = ObjectStorageConfig{}
	_, err = NewStore(cfg)
	require.ErrorContains(t, err, "object_storage.endpoint")
	cfg.ObjectStorage = retainedConfig
	store, err = NewStore(cfg)
	require.NoError(t, err)
	require.NotNil(t, store.object)
	require.Same(t, store.local, store.shards)
	require.Equal(t, localBackendTag, store.shardsBackend)
	require.NoError(t, store.db.Delete(shardKey(commitment, promiseHash), pebbledb.Sync))
	require.NoError(t, store.Close())

	cfg.ObjectStorage = ObjectStorageConfig{}
	store, err = NewStore(cfg)
	require.NoError(t, err)
	require.Nil(t, store.object)
	require.NoError(t, store.Close())
}

func TestStoreLocalDoesNotLoadAWSConfig(t *testing.T) {
	t.Setenv("AWS_PROFILE", "missing-profile")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))
	cfg := DefaultStoreConfig()
	cfg.Path = t.TempDir()
	store, err := NewStore(cfg)
	require.NoError(t, err)
	require.Nil(t, store.object)
	require.NoError(t, store.Close())
}
