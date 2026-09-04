package fibre

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	shardMarkerVersion = 1
	shardMarkerSize    = 10
	localBackendTag    = byte(0x01)
	objectBackendTag   = byte(0x02)
)

// encodeShardMarker encodes a local shard size.
func encodeShardMarker(size int64) []byte {
	return encodeShardMarkerForBackend(localBackendTag, size)
}

func encodeShardMarkerForBackend(backend byte, size int64) []byte {
	marker := make([]byte, shardMarkerSize)
	marker[0] = shardMarkerVersion
	marker[1] = backend
	binary.BigEndian.PutUint64(marker[2:], uint64(size))
	return marker
}

// decodeShardMarker returns the encoded shard size. An empty marker returns
// zero without an error and identifies a legacy local shard.
func decodeShardMarker(data []byte) (int64, error) {
	_, size, err := decodeShardMarkerBackend(data)
	return size, err
}

func decodeShardMarkerBackend(data []byte) (byte, int64, error) {
	if len(data) == 0 {
		return localBackendTag, 0, nil
	}
	if len(data) != shardMarkerSize {
		return 0, 0, fmt.Errorf("%w: shard marker length %d, want %d", ErrStoreIntegrity, len(data), shardMarkerSize)
	}
	if data[0] != shardMarkerVersion {
		return 0, 0, fmt.Errorf("%w: unsupported shard marker version %d", ErrStoreIntegrity, data[0])
	}
	if data[1] != localBackendTag && data[1] != objectBackendTag {
		return 0, 0, fmt.Errorf("%w: unsupported shard backend tag 0x%02x", ErrStoreIntegrity, data[1])
	}

	size := binary.BigEndian.Uint64(data[2:])
	if size == 0 || size > math.MaxInt64 {
		return 0, 0, fmt.Errorf("%w: invalid shard size %d", ErrStoreIntegrity, size)
	}
	return data[1], int64(size), nil
}
