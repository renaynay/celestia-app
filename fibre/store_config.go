package fibre

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	pebbledb "github.com/cockroachdb/pebble/v2"
	"golang.org/x/time/rate"
)

// ObjectStorageConfig configures S3-compatible storage. Credentials use the AWS SDK credential chain.
type ObjectStorageConfig struct {
	Endpoint string  `toml:"endpoint"`
	Region   string  `toml:"region" comment:"Use auto for Cloudflare R2."`
	Bucket   string  `toml:"bucket"`
	Prefix   string  `toml:"prefix"`
	ReadRPS  float64 `toml:"read_rps" comment:"Maximum object reads per second, with burst 1. Must be positive."`
}

// Validate checks the settings required to access object storage.
func (cfg ObjectStorageConfig) Validate() error {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("object_storage.endpoint must be an absolute HTTP or HTTPS URL without credentials")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return fmt.Errorf("object_storage.region is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return fmt.Errorf("object_storage.bucket is required")
	}
	if strings.Trim(strings.TrimSpace(cfg.Prefix), "/") == "" {
		return fmt.Errorf("object_storage.prefix is required")
	}
	if cfg.ReadRPS <= 0 || math.IsNaN(cfg.ReadRPS) || math.IsInf(cfg.ReadRPS, 0) {
		return fmt.Errorf("object_storage.read_rps must be finite and positive")
	}
	return nil
}

func (s *Store) openObjectStorage(cfg StoreConfig) error {
	needsObject := cfg.StorageBackend == "object"
	if !needsObject {
		var err error
		needsObject, err = s.hasObjectMarkers()
		if err != nil {
			return err
		}
	}
	if !needsObject {
		return nil
	}
	if err := cfg.ObjectStorage.Validate(); err != nil {
		return err
	}
	if cfg.ChainID == "" || cfg.ValidatorAddress == "" {
		return fmt.Errorf("chain ID and validator address are required for object storage")
	}

	awsConfig, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(cfg.ObjectStorage.Region))
	if err != nil {
		return fmt.Errorf("loading AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.ObjectStorage.Endpoint)
	})
	s.object = newObjectBackend(client, cfg.ObjectStorage.Bucket, cfg.ObjectStorage.Prefix,
		cfg.ChainID, cfg.ValidatorAddress, rate.NewLimiter(rate.Limit(cfg.ObjectStorage.ReadRPS), 1))
	if cfg.StorageBackend == "object" {
		s.shards = s.object
		s.shardsBackend = objectBackendTag
	}
	return nil
}

func (s *Store) hasObjectMarkers() (bool, error) {
	prefix := []byte("/shard/")
	iter, err := s.db.NewIter(&pebbledb.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return false, fmt.Errorf("creating shard iterator: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		backend, _, err := decodeShardMarkerBackend(iter.Value())
		if err == nil && backend == objectBackendTag {
			return true, nil
		}
	}
	return false, iter.Error()
}
