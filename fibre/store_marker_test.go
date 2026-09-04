package fibre

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/celestiaorg/celestia-app/v10/x/fibre/types"
	pebbledb "github.com/cockroachdb/pebble/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestShardMarkerCodec(t *testing.T) {
	marker := encodeShardMarker(42)
	require.Equal(t, []byte{1, 1, 0, 0, 0, 0, 0, 0, 0, 42}, marker)

	size, err := decodeShardMarker(marker)
	require.NoError(t, err)
	require.Equal(t, int64(42), size)

	size, err = decodeShardMarker(nil)
	require.NoError(t, err)
	require.Zero(t, size)

	backend, size, err := decodeShardMarkerBackend(encodeShardMarkerForBackend(objectBackendTag, 42))
	require.NoError(t, err)
	require.Equal(t, objectBackendTag, backend)
	require.Equal(t, int64(42), size)
}

func TestStoreRoutesObjectShard(t *testing.T) {
	store := newMarkerTestStore(t)
	object := &shardStorageStub{}
	store.shards = object
	store.shardsBackend = objectBackendTag
	store.object = object

	commitment := generateCommitment()
	shard := &types.BlobShard{Rows: []*types.BlobRow{{Index: 1, Data: []byte("data")}}}
	promise := &PaymentPromise{
		ChainID:           "test-chain",
		SignerKey:         secp256k1.GenPrivKey().PubKey().(*secp256k1.PubKey),
		Commitment:        commitment,
		CreationTimestamp: time.Unix(1, 0),
		Signature:         []byte{1},
	}
	require.NoError(t, store.Put(t.Context(), promise, shard, promise.CreationTimestamp))

	promiseHash, err := promise.Hash()
	require.NoError(t, err)
	markerData, closer, err := store.db.Get(shardKey(promise.Commitment, promiseHash))
	require.NoError(t, err)
	require.NoError(t, closer.Close())
	require.Equal(t, objectBackendTag, markerData[1])
	require.Equal(t, 1, object.puts)

	got, err := store.Get(t.Context(), commitment)
	require.NoError(t, err)
	require.Equal(t, shard, got)
	has, err := store.Has(t.Context(), commitment, promiseHash)
	require.NoError(t, err)
	require.True(t, has)
	require.Equal(t, 1, object.gets)
	require.Equal(t, 1, object.heads)

	size, err := store.Size(t.Context())
	require.NoError(t, err)
	require.Equal(t, shardBinarySize(shard), size)
}

func TestStoreMissingObjectPreservesMetadata(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	promiseHash := []byte{1}
	marker := []byte{1, objectBackendTag, 0, 0, 0, 0, 0, 0, 0, 1}
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), marker, pebbledb.NoSync))

	_, err := store.Get(t.Context(), commitment)
	require.ErrorIs(t, err, ErrStoreIntegrity)

	store.object = &shardStorageStub{missing: true}
	_, err = store.Get(t.Context(), commitment)
	require.ErrorIs(t, err, ErrStoreIntegrity)
	has, err := store.Has(t.Context(), commitment, promiseHash)
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.False(t, has)

	data, closer, err := store.db.Get(shardKey(commitment, promiseHash))
	require.NoError(t, err)
	require.Equal(t, marker, data)
	require.NoError(t, closer.Close())
}

func TestStoreGetSkipsMissingObjectToLocalSibling(t *testing.T) {
	store := newMarkerTestStore(t)
	object := &shardStorageStub{missing: true}
	store.object = object
	commitment := generateCommitment()
	objectHash := []byte{1}
	localHash := []byte{2}
	wantSize := writeMarkerTestShard(t, store, commitment, localHash)
	objectMarker := encodeShardMarkerForBackend(objectBackendTag, 1)
	require.NoError(t, store.db.Set(shardKey(commitment, objectHash), objectMarker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, localHash), encodeShardMarker(wantSize), pebbledb.NoSync))

	shard, err := store.Get(t.Context(), commitment)
	require.NoError(t, err)
	require.Equal(t, []byte("data"), shard.Rows[0].Data)
	require.Equal(t, 1, object.gets)

	data, closer, err := store.db.Get(shardKey(commitment, objectHash))
	require.NoError(t, err)
	require.Equal(t, objectMarker, data)
	require.NoError(t, closer.Close())
}

func TestShardMarkerRejectsInvalidData(t *testing.T) {
	valid := encodeShardMarker(1)

	overflow := append([]byte(nil), valid...)
	binary.BigEndian.PutUint64(overflow[2:], uint64(math.MaxInt64)+1)
	zeroBackend := append([]byte(nil), valid...)
	zeroBackend[1] = 0
	unknownBackend := append([]byte(nil), valid...)
	unknownBackend[1] = 3
	tests := []struct {
		name string
		data []byte
	}{
		{"truncated", valid[:9]},
		{"overlong", append(valid, 0)},
		{"zero version", append([]byte{0}, valid[1:]...)},
		{"unsupported version", append([]byte{2}, valid[1:]...)},
		{"zero backend", zeroBackend},
		{"unknown backend", unknownBackend},
		{"zero size", []byte{1, 1, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"size overflow", overflow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeShardMarker(tt.data)
			require.ErrorIs(t, err, ErrStoreIntegrity)
		})
	}
}

func TestStoreSupportsLegacyShardMarker(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	promiseHash := []byte{1, 2, 3}
	pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	wantSize := writeMarkerTestShard(t, store, commitment, promiseHash)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), nil, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))

	shard, err := store.Get(t.Context(), commitment)
	require.NoError(t, err)
	require.Len(t, shard.Rows, 1)
	has, err := store.Has(t.Context(), commitment, promiseHash)
	require.NoError(t, err)
	require.True(t, has)
	size, err := store.Size(t.Context())
	require.NoError(t, err)
	require.Equal(t, wantSize, size)

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, pruned)
	require.Equal(t, wantSize, freed)
}

func TestStoreRejectsInvalidShardMarkerWithoutDeletingItsData(t *testing.T) {
	tests := []struct {
		name string
		call func(*Store, Commitment, []byte, time.Time) error
	}{
		{"Get", func(s *Store, c Commitment, _ []byte, _ time.Time) error {
			_, err := s.Get(t.Context(), c)
			return err
		}},
		{"Has", func(s *Store, c Commitment, h []byte, _ time.Time) error {
			_, err := s.Has(t.Context(), c, h)
			return err
		}},
		{"Size", func(s *Store, _ Commitment, _ []byte, _ time.Time) error {
			_, err := s.Size(t.Context())
			return err
		}},
		{"PruneBefore", func(s *Store, _ Commitment, _ []byte, pruneAt time.Time) error {
			_, _, err := s.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMarkerTestStore(t)
			commitment := generateCommitment()
			promiseHash := []byte{1, 2, 3}
			pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
			writeMarkerTestShard(t, store, commitment, promiseHash)
			invalidMarker := []byte{1, 3, 0, 0, 0, 0, 0, 0, 0, 1}
			require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), invalidMarker, pebbledb.NoSync))
			require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))

			require.ErrorIs(t, tt.call(store, commitment, promiseHash, pruneAt), ErrStoreIntegrity)
			_, err := store.local.fs.Stat(store.local.shardPath(commitment, promiseHash))
			require.NoError(t, err)
			data, closer, err := store.db.Get(shardKey(commitment, promiseHash))
			require.NoError(t, err)
			require.Equal(t, invalidMarker, data)
			require.NoError(t, closer.Close())
		})
	}
}

func TestGetSkipsInvalidMarkerAndReturnsValidShard(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	invalidHash := []byte{1}
	validHash := []byte{2}
	invalidMarker := []byte{1, 3, 0, 0, 0, 0, 0, 0, 0, 1}
	writeMarkerTestShard(t, store, commitment, invalidHash)
	validSize := writeMarkerTestShard(t, store, commitment, validHash)
	validMarker := encodeShardMarker(validSize)
	require.NoError(t, store.db.Set(shardKey(commitment, invalidHash), invalidMarker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, validHash), validMarker, pebbledb.NoSync))

	shard, err := store.Get(t.Context(), commitment)
	require.NoError(t, err)
	require.Equal(t, []byte("data"), shard.Rows[0].Data)
	data, closer, err := store.db.Get(shardKey(commitment, invalidHash))
	require.NoError(t, err)
	require.Equal(t, invalidMarker, data)
	require.NoError(t, closer.Close())
}

func TestRemoveOrphanShardsPreservesInvalidMarker(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	promiseHash := make([]byte, 32)
	writeMarkerTestShard(t, store, commitment, promiseHash)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), []byte{1, 2}, pebbledb.NoSync))

	removed, err := store.removeOrphanShards()
	require.NoError(t, err)
	require.Zero(t, removed)
	_, err = store.local.fs.Stat(store.local.shardPath(commitment, promiseHash))
	require.NoError(t, err)
}

func TestSizeReturnsValidTotalWithInvalidMarker(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	validHash := []byte{1}
	invalidHash := []byte{2}
	secondInvalidHash := []byte{3}
	validSize := writeMarkerTestShard(t, store, commitment, validHash)
	validMarker := encodeShardMarker(validSize)
	require.NoError(t, store.db.Set(shardKey(commitment, validHash), validMarker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, invalidHash), []byte{1, 2}, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, secondInvalidHash), []byte{1, 2}, pebbledb.NoSync))

	size, err := store.Size(t.Context())
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.Contains(t, err.Error(), "2 invalid shard metadata entries")
	require.Equal(t, validSize, size)
}

func TestServerSeedsPartialSizeAfterIntegrityError(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	validHash := []byte{1}
	validSize := writeMarkerTestShard(t, store, commitment, validHash)
	validMarker := encodeShardMarker(validSize)
	require.NoError(t, store.db.Set(shardKey(commitment, validHash), validMarker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, []byte{2}), []byte{1, 2}, pebbledb.NoSync))

	var logs strings.Builder
	server := &Server{
		store: store,
		occ:   newOccupancy(0),
		log:   slog.New(slog.NewTextHandler(&logs, nil)),
	}
	require.NoError(t, server.seedOccupancy(t.Context()))
	require.Equal(t, validSize, server.occ.usage())
	require.Contains(t, logs.String(), "store size may be incorrect due to corrupt shard marker")
}

func TestHasAccountedShardMarker(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	promiseHash := []byte{1}

	accounted := store.hasAccountedShardMarker(commitment, promiseHash)
	require.False(t, accounted)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), nil, pebbledb.NoSync))
	accounted = store.hasAccountedShardMarker(commitment, promiseHash)
	require.False(t, accounted)
	marker := encodeShardMarker(1)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), marker, pebbledb.NoSync))
	accounted = store.hasAccountedShardMarker(commitment, promiseHash)
	require.True(t, accounted)
}

func TestPruneBeforeSkipsInvalidMarkerAndPrunesValidEntry(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	validHash := []byte{1}
	invalidHash := []byte{2}
	validSize := writeMarkerTestShard(t, store, commitment, validHash)
	writeMarkerTestShard(t, store, commitment, invalidHash)
	validMarker := encodeShardMarker(validSize)
	require.NoError(t, store.db.Set(shardKey(commitment, validHash), validMarker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(shardKey(commitment, invalidHash), []byte{1, 3, 0, 0, 0, 0, 0, 0, 0, 1}, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, validHash), nil, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, invalidHash), nil, pebbledb.NoSync))

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.Equal(t, 1, pruned)
	require.Equal(t, validSize, freed)
	_, closer, err := store.db.Get(shardKey(commitment, validHash))
	require.ErrorIs(t, err, pebbledb.ErrNotFound)
	require.Nil(t, closer)
	_, err = store.local.fs.Stat(store.local.shardPath(commitment, validHash))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, closer, err = store.db.Get(shardKey(commitment, invalidHash))
	require.NoError(t, err)
	require.NoError(t, closer.Close())
	_, err = store.local.fs.Stat(store.local.shardPath(commitment, invalidHash))
	require.NoError(t, err)
}

func TestPruneBeforeHonoursCancellation(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	promiseHash := []byte{1}
	pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	size := writeMarkerTestShard(t, store, commitment, promiseHash)
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), encodeShardMarker(size), pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pruned, freed, err := store.PruneBefore(ctx, pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, pruned)
	require.Zero(t, freed)
	_, err = store.local.fs.Stat(store.local.shardPath(commitment, promiseHash))
	require.NoError(t, err)
}

func TestPruneBeforeLimitsBatchSize(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	marker := encodeShardMarker(1)
	require.NoError(t, store.db.Set(shardKey(commitment, nil), []byte{1, 2}, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, nil), nil, pebbledb.NoSync))
	for i := range maxPruneBatchSize + 1 {
		promiseHash := binary.BigEndian.AppendUint64(nil, uint64(i))
		require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), marker, pebbledb.NoSync))
		require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))
	}

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.Equal(t, maxPruneBatchSize, pruned)
	require.Equal(t, int64(maxPruneBatchSize), freed)

	pruned, freed, err = store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.Equal(t, 1, pruned)
	require.Equal(t, int64(1), freed)
}

func TestServerPruneDrainsBacklog(t *testing.T) {
	store := newMarkerTestStore(t)
	commitment := generateCommitment()
	pruneAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	marker := encodeShardMarker(1)
	require.NoError(t, store.db.Set(shardKey(commitment, nil), []byte{1, 2}, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, nil), nil, pebbledb.NoSync))
	for i := range maxPruneBatchSize + 1 {
		promiseHash := binary.BigEndian.AppendUint64(nil, uint64(i))
		require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), marker, pebbledb.NoSync))
		require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))
	}

	occ := newOccupancy(0)
	occ.seed(maxPruneBatchSize + 1)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := newServerMetrics(provider.Meter("prune-test"), occ)
	require.NoError(t, err)
	var logs strings.Builder
	server := &Server{store: store, occ: occ, metrics: metrics, log: slog.New(slog.NewTextHandler(&logs, nil))}
	server.prune(t.Context())

	require.Zero(t, occ.usage())
	size, err := store.Size(t.Context())
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.Zero(t, size)
	require.Contains(t, logs.String(), "prune skipped corrupt shard markers")
	require.Contains(t, logs.String(), "pruned expired entries")
	require.NotContains(t, logs.String(), "level=ERROR")

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))
	var pruneEntries int64
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "fibre.server.prune.entries" {
				pruneEntries = metric.Data.(metricdata.Sum[int64]).DataPoints[0].Value
			}
		}
	}
	require.Equal(t, int64(maxPruneBatchSize+1), pruneEntries)
}

func newMarkerTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewMemoryStore(DefaultStoreConfig())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func writeMarkerTestShard(t *testing.T, store *Store, commitment Commitment, promiseHash []byte) int64 {
	t.Helper()
	path := store.local.shardPath(commitment, promiseHash)
	f, err := store.local.fs.Create(path, shardWriteCategory)
	require.NoError(t, err)
	require.NoError(t, writeShardBinary(f, &types.BlobShard{
		Rows: []*types.BlobRow{{Index: 1, Data: []byte("data")}},
	}))
	require.NoError(t, f.Close())
	info, err := store.local.fs.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

type shardStorageStub struct {
	shard   *types.BlobShard
	missing bool
	puts    int
	gets    int
	heads   int
}

func (s *shardStorageStub) Put(_ context.Context, _ Commitment, _ []byte, shard *types.BlobShard) (bool, error) {
	s.puts++
	s.shard = shard
	return true, nil
}

func (s *shardStorageStub) Get(context.Context, Commitment, []byte) (*types.BlobShard, error) {
	s.gets++
	if s.missing {
		return nil, ErrStoreNotFound
	}
	return s.shard, nil
}

func (s *shardStorageStub) Has(context.Context, Commitment, []byte) (bool, error) {
	s.heads++
	return !s.missing, nil
}

func (*shardStorageStub) Delete(context.Context, Commitment, []byte) error {
	return nil
}
