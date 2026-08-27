// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"

	"github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

const (
	errGetAccountID    = "cannot retrieve the AWS account ID"
	errRetrieveCreds   = "cannot retrieve the AWS credentials"
	errResolveAWSConf  = "cannot resolve the AWS config"
	errTokenFileHash   = "cannot calculate the hash for the credentials file"
	errCacheKeyCompute = "cannot compute the credentials cache key"
)

// awsConfigCacheTTL is how long a resolved AWS config -- and thus the
// credential Secret contents behind it -- is served before it is resolved
// again from scratch. It is the upper bound on how long a rotated credential
// Secret can go unnoticed, because rotating a Secret does not change the
// ProviderConfig's generation and so cannot be observed by the cache key.
//
// It is deliberately below the minimum STS session duration of 15 minutes
// (stscreds.DefaultDuration), so that a cached assume-role session provider
// is replaced well before the SDK would have to refresh it in the background
// from a reconcile context that may already be canceled.
const awsConfigCacheTTL = 5 * time.Minute

// AWSCredentialsProviderCacheOption lets you configure
// a *GlobalAWSCredentialsProviderCache.
type AWSCredentialsProviderCacheOption func(cache *AWSCredentialsProviderCache)

// WithCacheMaxSize lets you override the default MaxSize for
// AWS CredentialsProvider cache.
func WithCacheMaxSize(n int) AWSCredentialsProviderCacheOption {
	return func(c *AWSCredentialsProviderCache) {
		c.maxSize = n
	}
}

// WithCacheStore lets you bootstrap AWS CredentialsProvider Cache with
// your own cache.
func WithCacheStore(cache map[string]*awsCredentialsProviderCacheEntry) AWSCredentialsProviderCacheOption {
	return func(c *AWSCredentialsProviderCache) {
		c.cache = cache
	}
}

// WithCacheLogger lets you configure the logger for the cache.
func WithCacheLogger(l logging.Logger) AWSCredentialsProviderCacheOption {
	return func(c *AWSCredentialsProviderCache) {
		c.logger = l
	}
}

// NewAWSCredentialsProviderCache returns a new empty
// *AWSCredentialsProviderCache with the default GetAWSConfig method.
func NewAWSCredentialsProviderCache(opts ...AWSCredentialsProviderCacheOption) *AWSCredentialsProviderCache {
	c := &AWSCredentialsProviderCache{
		cache:   map[string]*awsCredentialsProviderCacheEntry{},
		maxSize: 100,
		mu:      &sync.RWMutex{},
		logger:  logging.NewNopLogger(),
	}
	for _, f := range opts {
		f(c)
	}
	return c
}

// AWSCredentialsProviderCache caches resolved AWS configs per ProviderConfig
// and region, so that credential resolution happens once per ProviderConfig
// instead of once per reconcile of every managed resource using it. On a
// cache hit no Kubernetes API request and no STS call is made: the
// credential Secret is not re-read and the cached config's own
// aws.CredentialsCache serves the session credentials it already holds.
//
// It has a maximum size that when it's reached, the entry that has the
// oldest access time will be removed from the cache, i.e. FIFO on last
// access time. Entries expire after awsConfigCacheTTL, except for the IRSA
// source; see expired.
type AWSCredentialsProviderCache struct {
	// cache holds the resolved AWS config and account ID with a unique
	// cache key per provider configuration. Key content includes the
	// ProviderConfig's UUID and Generation, the region, the credential
	// source and, for token file based sources, the token file path and a
	// hash of its content.
	cache map[string]*awsCredentialsProviderCacheEntry

	// maxSize is the maximum number of elements this cache can ever have.
	maxSize int

	// resolveGroup deduplicates concurrent resolutions of the same cache
	// key, so that a burst of reconciles sharing one ProviderConfig
	// resolves it once. Resolution runs outside mu: the cache-wide lock is
	// never held across a network call.
	resolveGroup singleflight.Group

	// mu is used to make sure the cache map is concurrency-safe.
	mu *sync.RWMutex

	// logger is the logger for cache operations.
	logger logging.Logger
}

// awsCredentialsProviderCacheEntry is a resolved AWS config together with
// the AWS account ID for its credentials. Entries are fully constructed
// before they are published to the cache map and all fields except
// accessedAt are read-only afterwards.
type awsCredentialsProviderCacheEntry struct {
	cfg        *aws.Config
	accountID  string
	resolvedAt time.Time
	// accessedAt stores a time.Time and is read and refreshed on the hot
	// path without holding any lock, so it must be accessed atomically.
	accessedAt atomic.Value
}

// lastAccess returns the entry's access time. An entry constructed without
// one reports the zero time, which makes it the first candidate for
// eviction rather than a panic on the reconciliation hot path.
func (e *awsCredentialsProviderCacheEntry) lastAccess() time.Time {
	t, _ := e.accessedAt.Load().(time.Time)
	return t
}

// AWSConfigResolverFn resolves an *aws.Config for a ProviderConfig. It is
// only invoked on a cache miss and it is the expensive work this cache
// exists to avoid: it may read the credential Secret from the API server
// and it constructs the STS assume role providers.
type AWSConfigResolverFn func(ctx context.Context) (*aws.Config, error)

// AccountIDFn retrieves the AWS account ID for a resolved AWS config and
// its retrieved credentials. It is only invoked on a cache miss.
type AccountIDFn func(ctx context.Context, cfg *aws.Config, creds aws.Credentials) (string, error)

// Credentials holds the aws.Credentials and the associated AWS account ID for
// these credentials.
type Credentials struct {
	creds     aws.Credentials
	accountID string
}

// RetrieveConfig returns the resolved AWS config and credentials for the
// given ProviderConfig and region, from the cache when possible. On a cache
// miss -- or an expired or failing entry -- resolverFn and accountIDFn are
// invoked outside the cache-wide lock and the result is cached. On a cache
// hit neither is invoked and the credentials come from the cached config's
// own credential provider, an aws.CredentialsCache for the STS-backed
// sources, which refreshes them based on expiry.
//
// The returned *aws.Config is a shallow copy of the cached one, so that a
// caller cannot mutate state shared with concurrent callers; the credential
// provider inside it is shared on purpose.
func (c *AWSCredentialsProviderCache) RetrieveConfig(ctx context.Context, pc *v1beta1.ClusterProviderConfig, region string, resolverFn AWSConfigResolverFn, accountIDFn AccountIDFn) (*aws.Config, Credentials, error) {
	cacheKey, err := c.cacheKey(pc, region)
	if err != nil {
		return nil, Credentials{}, errors.Wrap(err, errCacheKeyCompute)
	}

	c.mu.RLock()
	entry, ok := c.cache[cacheKey]
	c.mu.RUnlock()
	if ok && !expired(pc, entry) {
		creds, err := c.entryCredentials(ctx, entry)
		if err == nil {
			c.logger.Debug("Cache hit", "cacheKey", cacheKey, "pc", pc.GroupVersionKind().String())
			cfg := *entry.cfg
			return &cfg, creds, nil
		}
		// The cached credentials no longer work, e.g. the Secret was
		// rotated and the old keys were deactivated before the TTL
		// expired. Invalidate the entry and resolve from scratch below.
		c.logger.Debug("Cached credentials failed to retrieve, invalidating", "cacheKey", cacheKey, "error", err)
		c.invalidate(cacheKey, entry)
	}

	// Cache miss, expired or invalidated entry. Resolve outside the
	// cache-wide lock. The singleflight group makes concurrent callers for
	// the same key share one resolution instead of each reading the Secret
	// and calling STS.
	v, err, _ := c.resolveGroup.Do(cacheKey, func() (any, error) {
		// A concurrent caller may have just resolved and cached this key.
		c.mu.RLock()
		entry, ok := c.cache[cacheKey]
		c.mu.RUnlock()
		if ok && !expired(pc, entry) {
			return entry, nil
		}
		c.logger.Debug("Cache miss", "cacheKey", cacheKey, "pc", pc.GroupVersionKind().String(), "cacheSize", len(c.cache))
		cfg, err := resolverFn(ctx)
		if err != nil {
			return nil, errors.Wrap(err, errResolveAWSConf)
		}
		// Retrieve the credentials once so that a broken configuration
		// fails here instead of being cached, and so that accountIDFn can
		// use them.
		var creds aws.Credentials
		if cfg.Credentials != nil {
			if creds, err = cfg.Credentials.Retrieve(ctx); err != nil {
				return nil, errors.Wrap(err, errRetrieveCreds)
			}
		}
		accountID, err := accountIDFn(ctx, cfg, creds)
		if err != nil {
			return nil, errors.Wrap(err, errGetAccountID)
		}
		entry = &awsCredentialsProviderCacheEntry{
			cfg:        cfg,
			accountID:  accountID,
			resolvedAt: time.Now(),
		}
		entry.accessedAt.Store(time.Now())
		c.store(cacheKey, entry)
		return entry, nil
	})
	if err != nil {
		return nil, Credentials{}, err
	}
	entry = v.(*awsCredentialsProviderCacheEntry) //nolint:forcetypeassert // only entries are stored in the group
	creds, err := c.entryCredentials(ctx, entry)
	if err != nil {
		c.invalidate(cacheKey, entry)
		return nil, Credentials{}, err
	}
	cfg := *entry.cfg
	return &cfg, creds, nil
}

// cacheKey computes the cache key for the given ProviderConfig and region.
// Everything about the ProviderConfig itself -- credential source, secret
// references, assume role chain, endpoint config -- is covered by its UID
// and Generation, which change whenever it is replaced or modified. Inputs
// that change the resolved config without touching the ProviderConfig have
// to be included separately: for the token file based sources that is the
// token file path, a hash of its content and the role ARN from the AWS
// environment variables. The contents of a credential Secret are
// deliberately not part of the key -- reading them per reconcile is exactly
// what this cache avoids -- which is why non-IRSA entries carry a TTL.
func (c *AWSCredentialsProviderCache) cacheKey(pc *v1beta1.ClusterProviderConfig, region string) (string, error) {
	cacheKeyParams := []string{
		string(pc.UID),
		strconv.FormatInt(pc.Generation, 10),
		region,
		string(pc.Spec.Credentials.Source),
	}
	tokenFile := os.Getenv(envWebIdentityTokenFile)
	// IRSA requires the projected token file, so for IRSA an empty path is
	// an error, as before. For the other sources these environment
	// variables are optional and only included when present.
	if pc.Spec.Credentials.Source == authKeyIRSA || tokenFile != "" {
		tokenHash, err := hashTokenFile(tokenFile)
		if err != nil {
			return "", errors.Wrap(err, errTokenFileHash)
		}
		cacheKeyParams = append(cacheKeyParams, tokenHash, tokenFile, os.Getenv(envWebIdentityRoleARN))
	}
	return strings.Join(cacheKeyParams, ":"), nil
}

// expired reports whether the given cache entry is past awsConfigCacheTTL.
// IRSA entries never expire: their only external input, the projected token
// file, is part of the cache key, so a token rotation shows up as a new key
// instead. This matches the pre-existing IRSA caching behavior.
func expired(pc *v1beta1.ClusterProviderConfig, entry *awsCredentialsProviderCacheEntry) bool {
	if pc.Spec.Credentials.Source == authKeyIRSA {
		return false
	}
	return time.Since(entry.resolvedAt) > awsConfigCacheTTL
}

// entryCredentials retrieves the credentials of a cached entry and refreshes
// its LRU access time. For the STS-backed sources entry.cfg.Credentials is
// an aws.CredentialsCache, so this is a local operation unless the session
// credentials approach expiry.
func (c *AWSCredentialsProviderCache) entryCredentials(ctx context.Context, entry *awsCredentialsProviderCacheEntry) (Credentials, error) {
	var creds aws.Credentials
	if entry.cfg.Credentials != nil {
		var err error
		if creds, err = entry.cfg.Credentials.Retrieve(ctx); err != nil {
			return Credentials{}, errors.Wrap(err, errRetrieveCreds)
		}
	}
	// since this is a hot-path in the execution, do not always update
	// the last access times, it is fine to evict the LRU entry on a less
	// granular precision. accessedAt is atomic, so concurrent refreshes
	// simply overwrite each other.
	if time.Since(entry.lastAccess()) > 10*time.Minute {
		entry.accessedAt.Store(time.Now())
	}
	return Credentials{creds: creds, accountID: entry.accountID}, nil
}

// store publishes a fully constructed entry, evicting the least recently
// used entry if the cache is full.
func (c *AWSCredentialsProviderCache) store(key string, entry *awsCredentialsProviderCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cache[key]; !ok {
		c.makeRoom()
	}
	c.cache[key] = entry
}

// invalidate removes the given entry from the cache, unless the entry stored
// under the key has already been replaced by a newer one.
func (c *AWSCredentialsProviderCache) invalidate(key string, entry *awsCredentialsProviderCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache[key] == entry {
		delete(c.cache, key)
	}
}

// makeRoom ensures that there is at most maxSize-1 elements in the cache map
// so that a new entry can be added. It deletes the object that
// was last accessed before all others.
// This implementation is not thread safe. Callers must properly synchronize.
func (c *AWSCredentialsProviderCache) makeRoom() {
	if 1+len(c.cache) <= c.maxSize {
		return
	}
	var dustiest string
	for key, val := range c.cache {
		if dustiest == "" {
			dustiest = key
			continue
		}
		if val.lastAccess().Before(c.cache[dustiest].lastAccess()) {
			dustiest = key
		}
	}
	delete(c.cache, dustiest)
}

// hashTokenFile calculates the sha256 checksum of the token file content at
// the supplied file path
func hashTokenFile(filename string) (string, error) {
	if filename == "" {
		return "", errors.New("token file name cannot be empty")
	}
	file, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()

	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}

	checksum := hash.Sum(nil)
	return fmt.Sprintf("%x", checksum), nil
}
