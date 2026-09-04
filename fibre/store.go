package fibre

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/celestiaorg/celestia-app/v10/x/fibre/types"
	pebbledb "github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	gogoproto "github.com/cosmos/gogoproto/proto"
)

// Bulk shard data is kept off pebble because pebble serializes large-value
// commits through a single goroutine, which becomes the upload bottleneck at
// concurrency. Pebble only holds the small metadata.
const (
	// maxPruneBatchSize bounds each Pebble commit. The server drains full
	// batches during one prune pass.
	maxPruneBatchSize = 1000
)

// ErrStoreNotFound is returned when no shard is found for a [Commitment] in the [Store].
var ErrStoreNotFound = errors.New("no shard found in store")

// ErrStoreIntegrity is returned when stored metadata is invalid.
var ErrStoreIntegrity = errors.New("store integrity error")

// StoreConfig contains configuration options for the [Store].
type StoreConfig struct {
	// Path is the path to the store directory.
	Path string `toml:"-"`
	// Log defaults to [slog.Default] when nil.
	Log *slog.Logger `toml:"-"`
}

// DefaultStoreConfig returns a [StoreConfig] with default values.
func DefaultStoreConfig() StoreConfig {
	return StoreConfig{}
}

// Validate checks that the StoreConfig is valid and fills in defaults for
// unset fields.
func (cfg *StoreConfig) Validate() error {
	if cfg.Path == "" {
		return fmt.Errorf("store path is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return nil
}

// Store manages persistent storage of [PaymentPromise] and row data.
// It provides indexed access by [Commitment], promise hash, and timestamp.
type Store struct {
	db            *pebbledb.DB
	log           *slog.Logger
	shards        shardStorage
	shardsBackend byte
	local         *localBackend
	object        shardStorage
}

// memStorePath is an arbitrary location inside the in-memory FS used by
// [NewMemoryStore]; both pebble's files and our shards/staging subdirs live
// under it so the layout matches the on-disk store.
const memStorePath = "/store"

// NewMemoryStore creates a [Store] backed entirely by [vfs.NewMem]; both the
// pebble metadata and the flat shard files live in memory and are dropped
// when the Store is garbage collected.
func NewMemoryStore(cfg StoreConfig) *Store {
	cfg.Path = memStorePath
	s, err := openStore(cfg, vfs.NewMem())
	if err != nil {
		panic(fmt.Sprintf("opening in-memory store: %v", err))
	}
	return s
}

// NewStore opens a [Store] backed by an on-disk pebble database and flat
// shard files at cfg.Path. On open, [Store.reconcile] drops leftover staging
// files and shard files without markers.
func NewStore(cfg StoreConfig) (*Store, error) {
	return openStore(cfg, vfs.Default)
}

func openStore(cfg StoreConfig, filesystem vfs.FS) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating store config: %w", err)
	}

	local, err := newLocalBackend(cfg.Path, filesystem)
	if err != nil {
		return nil, fmt.Errorf("opening local shard storage: %w", err)
	}

	opts := &pebbledb.Options{FS: filesystem}
	// Values in pebble are sub-1KB metadata only; tuning is light.
	opts.MemTableSize = 16 << 20
	opts.L0CompactionThreshold = 4
	opts.L0StopWritesThreshold = 12
	opts.LBaseMaxBytes = 64 << 20

	db, err := pebbledb.Open(cfg.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("opening pebble database: %w", err)
	}

	s := &Store{db: db, log: cfg.Log, shards: local, shardsBackend: localBackendTag, local: local}
	if err := s.reconcile(); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("reconciling store: %w", err)
	}
	return s, nil
}

// Put stores a [PaymentPromise] and [types.BlobShard] in the write backend,
// then commits Pebble metadata. Puts for the same commitment but different
// promises are stored independently without deduplication.
func (s *Store) Put(ctx context.Context, promise *PaymentPromise, shard *types.BlobShard, pruneAt time.Time) error {
	// Respect a client that has already gone away: skip the staging write
	// entirely rather than doing work whose result nobody is waiting for.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("aborting store put: %w", err)
	}

	promiseHash, err := promise.Hash()
	if err != nil {
		return fmt.Errorf("getting promise hash: %w", err)
	}

	marker := encodeShardMarkerForBackend(s.shardsBackend, shardBinarySize(shard))
	return s.commitAndStore(ctx, promise, promiseHash, shard, marker, pruneAt)
}

// commitAndStore writes the shard payload, then commits its Pebble metadata.
func (s *Store) commitAndStore(ctx context.Context, promise *PaymentPromise, promiseHash []byte, shard *types.BlobShard, marker []byte, pruneAt time.Time) error {
	promiseProto, err := promise.ToProto()
	if err != nil {
		return fmt.Errorf("converting payment promise to proto: %w", err)
	}
	ppData, err := gogoproto.Marshal(promiseProto)
	if err != nil {
		return fmt.Errorf("marshaling payment promise: %w", err)
	}

	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(promiseKey(promiseHash), ppData, pebbledb.NoSync); err != nil {
		return fmt.Errorf("putting payment promise: %w", err)
	}
	if err := batch.Set(shardKey(promise.Commitment, promiseHash), marker, pebbledb.NoSync); err != nil {
		return fmt.Errorf("putting shard marker: %w", err)
	}
	if err := batch.Set(pruneKey(pruneAt, promise.Commitment, promiseHash), nil, pebbledb.NoSync); err != nil {
		return fmt.Errorf("putting prune index: %w", err)
	}

	// Last safe point to honour cancellation before the durable payload write.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("aborting store commit: %w", err)
	}

	created, err := s.shards.Put(ctx, promise.Commitment, promiseHash, shard)
	if err != nil {
		return fmt.Errorf("storing shard payload: %w", err)
	}
	if err := batch.Commit(pebbledb.NoSync); err != nil {
		if created && !s.hasShardMarker(promise.Commitment, promiseHash) {
			if rmErr := s.shards.Delete(context.Background(), promise.Commitment, promiseHash); rmErr != nil {
				s.log.Warn("failed to remove orphaned shard after commit failure",
					"commitment", promise.Commitment.String(), "error", rmErr)
			}
		}
		return fmt.Errorf("committing metadata: %w", err)
	}

	return nil
}

// Get returns the first [types.BlobShard] found for the given [Commitment].
// When multiple promises exist for the same commitment, returning only the
// first prevents unbounded message sizes; pebble's deterministic key order
// makes the choice consistent across validators.
//
// Get removes a local marker when its payload is missing. It preserves a
// missing object marker and reports an integrity error.
func (s *Store) Get(ctx context.Context, commitment Commitment) (*types.BlobShard, error) {
	prefix := fmt.Appendf(nil, "/shard/%s/", commitment.String())
	iter, err := s.db.NewIter(&pebbledb.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("creating iterator: %w", err)
	}
	defer iter.Close()

	var rerr error
	for valid := iter.First(); valid; valid = iter.Next() {
		backendTag, _, err := decodeShardMarkerBackend(iter.Value())
		if err != nil {
			rerr = errors.Join(rerr, err)
			continue
		}
		backend, err := s.shardBackend(backendTag)
		if err != nil {
			rerr = errors.Join(rerr, err)
			continue
		}
		promiseHashHex := string(iter.Key()[len(prefix):])
		promiseHash, err := hex.DecodeString(promiseHashHex)
		if err != nil {
			rerr = errors.Join(rerr, fmt.Errorf("decoding promise hash from shard key: %w", err))
			continue
		}

		shard, err := backend.Get(ctx, commitment, promiseHash)
		if err == nil {
			return shard, nil
		}
		if errors.Is(err, ErrStoreNotFound) {
			if backendTag == objectBackendTag {
				rerr = errors.Join(rerr, fmt.Errorf("%w: object shard payload is missing", ErrStoreIntegrity))
				continue
			}
			// Orphan marker — drop it. The /prune/ entry self-cleans at TTL.
			if delErr := s.db.Delete(shardKey(commitment, promiseHash), pebbledb.NoSync); delErr != nil {
				s.log.Warn("failed to clean orphan shard marker",
					"commitment", commitment.String(),
					"error", delErr,
				)
			}
			continue
		}
		rerr = errors.Join(rerr, fmt.Errorf("reading shard payload: %w", err))
	}

	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("iterating shards: %w", err)
	}
	if rerr != nil {
		return nil, rerr
	}
	return nil, ErrStoreNotFound
}

// Has verifies that shard exists without reading the whole file
func (s *Store) Has(ctx context.Context, commitment Commitment, promiseHash []byte) (bool, error) {
	var (
		backendTag              byte
		markerData, closer, err = s.db.Get(shardKey(commitment, promiseHash))
	)
	switch {
	case errors.Is(err, pebbledb.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("checking if shard exists failed: %w", err)
	default:
		backendTag, _, err = decodeShardMarkerBackend(markerData)
		_ = closer.Close()
		if err != nil {
			return false, err
		}
	}

	backend, err := s.shardBackend(backendTag)
	if err != nil {
		return false, err
	}
	has, err := backend.Has(ctx, commitment, promiseHash)
	if err != nil {
		return false, err
	}
	if !has && backendTag == objectBackendTag {
		return false, fmt.Errorf("%w: object shard payload is missing", ErrStoreIntegrity)
	}
	return has, nil
}

func (s *Store) shardBackend(tag byte) (shardStorage, error) {
	switch tag {
	case localBackendTag:
		return s.local, nil
	case objectBackendTag:
		if s.object == nil {
			return nil, fmt.Errorf("%w: object shard backend is unavailable", ErrStoreIntegrity)
		}
		return s.object, nil
	default:
		return nil, fmt.Errorf("%w: unsupported shard backend tag 0x%02x", ErrStoreIntegrity, tag)
	}
}

// hasAccountedShardMarker reports whether a marker validated by [Store.Has]
// already counts the shard towards occupancy.
func (s *Store) hasAccountedShardMarker(commitment Commitment, promiseHash []byte) bool {
	markerData, closer, err := s.db.Get(shardKey(commitment, promiseHash))
	if err != nil {
		return false
	}
	_ = closer.Close()
	return len(markerData) > 0
}

// hasShardMarker reports whether a committed shard marker exists,
// checking only the pebble metadata.
func (s *Store) hasShardMarker(commit Commitment, promiseHash []byte) bool {
	_, closer, err := s.db.Get(shardKey(commit, promiseHash))
	switch {
	case err == nil:
		_ = closer.Close()
		return true
	case errors.Is(err, pebbledb.ErrNotFound):
		return false
	default:
		s.log.Warn("failed to check shard marker", "commitment", commit.String(), "error", err)
		return false
	}
}

// Size returns the marker-accounted encoded size of stored shards. It stats
// existing local payloads for empty legacy markers. If invalid metadata is
// skipped, it returns the usable partial total with [ErrStoreIntegrity]. All
// other errors return zero.
func (s *Store) Size(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	prefix := []byte("/shard/")
	iter, err := s.db.NewIter(&pebbledb.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return 0, fmt.Errorf("creating iterator: %w", err)
	}
	defer iter.Close()

	var (
		totalSize      int64
		integrityErr   error
		invalidEntries int
	)
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		size, err := decodeShardMarker(iter.Value())
		if err != nil {
			invalidEntries++
			if integrityErr == nil {
				integrityErr = fmt.Errorf("decoding shard marker %q: %w", iter.Key(), err)
			}
			continue
		}
		if size == 0 {
			commitment, promiseHash, ok := parseShardKey(string(iter.Key()))
			if !ok {
				invalidEntries++
				if integrityErr == nil {
					integrityErr = fmt.Errorf("%w: invalid shard key %q", ErrStoreIntegrity, iter.Key())
				}
				continue
			}
			size, err = s.local.size(commitment, promiseHash)
			if err != nil {
				return 0, fmt.Errorf("stat legacy shard file: %w", err)
			}
			if size == 0 {
				continue
			}
		}
		if size > math.MaxInt64-totalSize {
			return 0, fmt.Errorf("%w: total shard size overflows int64", ErrStoreIntegrity)
		}
		totalSize += size
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("iterating shards: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, ctx.Err()
	}
	if invalidEntries > 1 {
		integrityErr = fmt.Errorf("%w (%d invalid shard metadata entries)", integrityErr, invalidEntries)
	}
	return totalSize, integrityErr
}

// DiskAvailable returns the free bytes on the filesystem backing the store.
func (s *Store) DiskAvailable() (int64, error) {
	return s.local.diskAvailable()
}

// GetPaymentPromise retrieves a [PaymentPromise] by its hash.
func (s *Store) GetPaymentPromise(_ context.Context, promiseHash []byte) (*PaymentPromise, error) {
	data, closer, err := s.db.Get(promiseKey(promiseHash))
	if err != nil {
		return nil, fmt.Errorf("getting payment promise: %w", err)
	}
	defer closer.Close()

	var ppProto types.PaymentPromise
	if err := gogoproto.Unmarshal(data, &ppProto); err != nil {
		return nil, fmt.Errorf("unmarshaling payment promise: %w", err)
	}

	var promise PaymentPromise
	if err := promise.FromProto(&ppProto); err != nil {
		return nil, fmt.Errorf("converting from proto: %w", err)
	}

	return &promise, nil
}

// PruneBefore deletes all shards and payment promises with pruneAt before the given time
// and returns the number of pruned entries and the freed bytes.
//
// It deletes at most [maxPruneBatchSize] expired entries per call. If invalid
// markers are skipped, it commits valid deletions and returns their count and
// freed bytes with [ErrStoreIntegrity]. Invalid markers remain unchanged and
// do not consume deletion capacity.
func (s *Store) PruneBefore(ctx context.Context, before time.Time) (int, int64, error) {
	prefix := []byte("/prune/")
	iter, err := s.db.NewIter(&pebbledb.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("creating iterator: %w", err)
	}
	defer iter.Close()

	batch := s.db.NewBatch()
	defer batch.Close()
	var (
		pruned         int
		prunedBytes    int64
		integrityErr   error
		corruptMarkers int
	)
	beforeStr := formatTimestamp(before.UTC())
	for valid := iter.First(); valid && pruned < maxPruneBatchSize; valid = iter.Next() {
		key := iter.Key()

		// Keys are sorted; once the timestamp reaches the cutoff we're done.
		keyStr := string(key)
		timestampStr := keyStr[7:19] // skip "/prune/" (7 chars), take YYYYMMDDHHmm
		if timestampStr >= beforeStr {
			break
		}
		commitment, promiseHash, ok := parsePruneKey(keyStr)
		if !ok {
			continue
		}

		markerData, closer, err := s.db.Get(shardKey(commitment, promiseHash))
		var size int64
		switch {
		case errors.Is(err, pebbledb.ErrNotFound):
		case err != nil:
			return pruned, prunedBytes, fmt.Errorf("getting shard marker: %w", err)
		default:
			size, err = decodeShardMarker(markerData)
			_ = closer.Close()
			if err != nil {
				corruptMarkers++
				if integrityErr == nil {
					integrityErr = fmt.Errorf("decoding shard marker %q: %w", key, err)
				}
				continue
			}
		}

		if size == 0 {
			size, err = s.local.size(commitment, promiseHash)
			if err != nil {
				return pruned, prunedBytes, fmt.Errorf("getting shard file stats: %w", err)
			}
		}
		if size > math.MaxInt64-prunedBytes {
			return pruned, prunedBytes, fmt.Errorf("%w: pruned shard size overflows int64", ErrStoreIntegrity)
		}

		// Missing file is fine (orphan marker from a crashed Put).
		if err := s.shards.Delete(ctx, commitment, promiseHash); err != nil {
			return pruned, prunedBytes, err
		}
		if err := batch.Delete(key, pebbledb.NoSync); err != nil {
			return pruned, prunedBytes, fmt.Errorf("deleting prune index: %w", err)
		}
		if err := batch.Delete(shardKey(commitment, promiseHash), pebbledb.NoSync); err != nil {
			return pruned, prunedBytes, fmt.Errorf("deleting shard marker: %w", err)
		}
		if err := batch.Delete(promiseKey(promiseHash), pebbledb.NoSync); err != nil {
			return pruned, prunedBytes, fmt.Errorf("deleting payment promise: %w", err)
		}
		pruned++
		prunedBytes += size
	}

	if err := iter.Error(); err != nil {
		return pruned, prunedBytes, fmt.Errorf("iterating prune index: %w", err)
	}

	if err := batch.Commit(pebbledb.NoSync); err != nil {
		return pruned, prunedBytes, fmt.Errorf("committing batch: %w", err)
	}
	if corruptMarkers > 1 {
		integrityErr = fmt.Errorf("%w (%d corrupt shard markers)", integrityErr, corruptMarkers)
	}
	return pruned, prunedBytes, integrityErr
}

// reconcile drops incomplete staging files and canonical shard files without
// markers. Markers without files self-heal in [Store.Get] and at pruneAt.
func (s *Store) reconcile() error {
	start := time.Now()
	stagingRemoved, err := s.local.resetStaging()
	if err != nil {
		s.log.Error("store reconcile failed", "error", err, "elapsed_ms", time.Since(start).Milliseconds())
		return err
	}
	orphansRemoved, err := s.removeOrphanShards()
	if err != nil {
		s.log.Error("store reconcile failed", "error", err, "elapsed_ms", time.Since(start).Milliseconds())
		return err
	}
	s.log.Info("store reconcile complete", "staging_files_removed", stagingRemoved,
		"orphan_files_removed", orphansRemoved, "elapsed_ms", time.Since(start).Milliseconds())
	return nil
}

func (s *Store) removeOrphanShards() (int, error) {
	return s.local.removeOrphans(func(commitment Commitment, promiseHash []byte) (bool, error) {
		_, closer, err := s.db.Get(shardKey(commitment, promiseHash))
		switch {
		case err == nil:
			_ = closer.Close()
			return true, nil
		case errors.Is(err, pebbledb.ErrNotFound):
			return false, nil
		default:
			return false, fmt.Errorf("checking shard marker: %w", err)
		}
	})
}

// Close closes the underlying pebble database. For [NewMemoryStore] the
// in-memory FS is dropped when the Store is garbage collected.
func (s *Store) Close() error {
	return s.db.Close()
}

// formatTimestamp formats t with minute precision (YYYYMMDDHHmm) for
// lexicographic ordering in the prune index.
func formatTimestamp(timestamp time.Time) string {
	return timestamp.Format("200601021504")
}

func promiseKey(promiseHash []byte) []byte {
	return fmt.Appendf(nil, "/pp/%s", hex.EncodeToString(promiseHash))
}

func shardKey(commitment Commitment, promiseHash []byte) []byte {
	return fmt.Appendf(nil, "/shard/%s/%s", commitment.String(), hex.EncodeToString(promiseHash))
}

func parseShardKey(key string) (Commitment, []byte, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[1] != "shard" {
		return Commitment{}, nil, false
	}

	commitment, err := CommitmentFromString(parts[2])
	if err != nil {
		return Commitment{}, nil, false
	}
	promiseHash, err := hex.DecodeString(parts[3])
	if err != nil {
		return Commitment{}, nil, false
	}
	return commitment, promiseHash, true
}

// pruneKey is keyed by the pruneAt computed during upload (see shardPruneAt)
// so [Store.PruneBefore] scans in timestamp order. Re-puts of the same
// (commit, promiseHash) are idempotent because pruneAt is deterministic given
// CreationTimestamp, which is part of the hash.
func pruneKey(pruneAt time.Time, commitment Commitment, promiseHash []byte) []byte {
	return fmt.Appendf(nil, "/prune/%s/%s/%s", formatTimestamp(pruneAt.UTC()), commitment.String(), hex.EncodeToString(promiseHash))
}

// prefixUpperBound returns the upper bound for a prefix scan.
// It increments the last byte of the prefix to create an exclusive upper bound.
// For example, "/shard/abc" returns "/shard/abd".
func prefixUpperBound(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := range slices.Backward(upper) {
		upper[i]++
		if upper[i] != 0 {
			return upper
		}
	}
	// all 0xff bytes - return nil to indicate no upper bound
	return nil
}

// parsePruneKey extracts commitment and promise hash from a prune index key.
// Key format: /prune/<timestamp>/<commitment>/<promise-hash>
func parsePruneKey(key string) (Commitment, []byte, bool) {
	// split: ["", "prune", "<timestamp>", "<commitment>", "<promise-hash>"]
	parts := strings.Split(key, "/")
	if len(parts) != 5 {
		return Commitment{}, nil, false
	}

	commitment, err := CommitmentFromString(parts[3])
	if err != nil {
		return Commitment{}, nil, false
	}

	promiseHash, err := hex.DecodeString(parts[4])
	if err != nil {
		return Commitment{}, nil, false
	}

	return commitment, promiseHash, true
}
