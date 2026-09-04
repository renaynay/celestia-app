package fibre

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
)

func TestPruneBeforeMixedBackends(t *testing.T) {
	var (
		store      = newMarkerTestStore(t)
		object     = &batchShardStorageStub{}
		commitment = generateCommitment()
		pruneAt    = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		localHash  = []byte{1}
		objectHash = []byte{2}
		localSize  = writeMarkerTestShard(t, store, commitment, localHash)
	)
	store.object = object
	setPruneEntry(t, store, pruneAt, commitment, localHash, encodeShardMarker(localSize))
	setPruneEntry(t, store, pruneAt, commitment, objectHash, encodeShardMarkerForBackend(objectBackendTag, 7))

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, pruned)
	require.Equal(t, localSize+7, freed)
	require.Equal(t, []int{1}, object.batchSizes)
	requirePruneEntry(t, store, pruneAt, commitment, localHash, false)
	requirePruneEntry(t, store, pruneAt, commitment, objectHash, false)
}

func TestPruneBeforeTreatsMissingObjectAsDeleted(t *testing.T) {
	var (
		store       = newMarkerTestStore(t)
		object      = &batchShardStorageStub{}
		commitment  = generateCommitment()
		pruneAt     = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		promiseHash = []byte{1}
	)
	store.object = object
	setPruneEntry(t, store, pruneAt, commitment, promiseHash, encodeShardMarkerForBackend(objectBackendTag, 7))

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, pruned)
	require.Equal(t, int64(7), freed)
	requirePruneEntry(t, store, pruneAt, commitment, promiseHash, false)
}

func TestPruneBeforeKeepsObjectMetadataAfterPerKeyFailure(t *testing.T) {
	var (
		store     = newMarkerTestStore(t)
		deleteErr = errors.New("access denied")
		object    = &batchShardStorageStub{deleteObjects: func(_ context.Context, ids []shardID) ([]error, error) {
			return []error{nil, deleteErr}, nil
		}}
		commitment = generateCommitment()
		pruneAt    = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		firstHash  = []byte{1}
		secondHash = []byte{2}
		marker     = encodeShardMarkerForBackend(objectBackendTag, 7)
	)
	store.object = object
	setPruneEntry(t, store, pruneAt, commitment, firstHash, marker)
	setPruneEntry(t, store, pruneAt, commitment, secondHash, marker)

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, deleteErr)
	require.Equal(t, 1, pruned)
	require.Equal(t, int64(7), freed)
	requirePruneEntry(t, store, pruneAt, commitment, firstHash, false)
	requirePruneEntry(t, store, pruneAt, commitment, secondHash, true)
}

func TestPruneBeforeCommitsLocalSuccessAfterObjectRequestFailure(t *testing.T) {
	var (
		store      = newMarkerTestStore(t)
		requestErr = errors.New("request failed")
		object     = &batchShardStorageStub{deleteObjects: func(context.Context, []shardID) ([]error, error) {
			return nil, requestErr
		}}
		commitment = generateCommitment()
		pruneAt    = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		localHash  = []byte{1}
		objectHash = []byte{2}
		localSize  = writeMarkerTestShard(t, store, commitment, localHash)
	)
	store.object = object
	setPruneEntry(t, store, pruneAt, commitment, localHash, encodeShardMarker(localSize))
	setPruneEntry(t, store, pruneAt, commitment, objectHash, encodeShardMarkerForBackend(objectBackendTag, 7))

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, requestErr)
	require.Equal(t, 1, pruned)
	require.Equal(t, localSize, freed)
	requirePruneEntry(t, store, pruneAt, commitment, localHash, false)
	requirePruneEntry(t, store, pruneAt, commitment, objectHash, true)
}

func TestPruneBeforeDrainsObjectBatches(t *testing.T) {
	var (
		store      = newMarkerTestStore(t)
		object     = &batchShardStorageStub{}
		commitment = generateCommitment()
		pruneAt    = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		marker     = encodeShardMarkerForBackend(objectBackendTag, 1)
	)
	store.object = object
	for i := range maxObjectDeleteBatchSize + 1 {
		promiseHash := binary.BigEndian.AppendUint64(nil, uint64(i))
		setPruneEntry(t, store, pruneAt, commitment, promiseHash, marker)
	}

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, maxObjectDeleteBatchSize, pruned)
	require.Equal(t, int64(maxObjectDeleteBatchSize), freed)

	pruned, freed, err = store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, pruned)
	require.Equal(t, int64(1), freed)
	require.Equal(t, []int{maxObjectDeleteBatchSize, 1}, object.batchSizes)
}

func TestPruneBeforeBoundsCorruptMarkerErrors(t *testing.T) {
	var (
		store      = newMarkerTestStore(t)
		object     = &batchShardStorageStub{}
		commitment = generateCommitment()
		pruneAt    = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	)
	store.object = object
	setPruneEntry(t, store, pruneAt, commitment, []byte{1}, []byte{1, 2})
	setPruneEntry(t, store, pruneAt, commitment, []byte{2}, []byte{1, 2})
	setPruneEntry(t, store, pruneAt, commitment, []byte{3}, encodeShardMarkerForBackend(objectBackendTag, 7))

	pruned, freed, err := store.PruneBefore(t.Context(), pruneAt.Add(time.Hour))
	require.ErrorIs(t, err, ErrStoreIntegrity)
	require.ErrorContains(t, err, "2 prune errors")
	require.Equal(t, 1, pruned)
	require.Equal(t, int64(7), freed)
	require.Equal(t, []int{1}, object.batchSizes)
}

func setPruneEntry(t *testing.T, store *Store, pruneAt time.Time, commitment Commitment, promiseHash, marker []byte) {
	t.Helper()
	require.NoError(t, store.db.Set(shardKey(commitment, promiseHash), marker, pebbledb.NoSync))
	require.NoError(t, store.db.Set(pruneKey(pruneAt, commitment, promiseHash), nil, pebbledb.NoSync))
}

func requirePruneEntry(t *testing.T, store *Store, pruneAt time.Time, commitment Commitment, promiseHash []byte, present bool) {
	t.Helper()
	for _, key := range [][]byte{shardKey(commitment, promiseHash), pruneKey(pruneAt, commitment, promiseHash)} {
		_, closer, err := store.db.Get(key)
		if present {
			require.NoError(t, err)
			require.NoError(t, closer.Close())
			continue
		}
		require.ErrorIs(t, err, pebbledb.ErrNotFound)
		require.Nil(t, closer)
	}
}

type batchShardStorageStub struct {
	shardStorageStub
	deleteObjects func(context.Context, []shardID) ([]error, error)
	batchSizes    []int
}

func (s *batchShardStorageStub) DeleteObjects(ctx context.Context, ids []shardID) ([]error, error) {
	s.batchSizes = append(s.batchSizes, len(ids))
	if s.deleteObjects != nil {
		return s.deleteObjects(ctx, ids)
	}
	return make([]error, len(ids)), nil
}
