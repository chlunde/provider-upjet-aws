// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/pkg/errors"
)

const (
	errGetCallerIdentityFailed = "GetCallerIdentity query failed"
)

// GlobalCallerIdentityCache is a global cache to be used by all controllers.
var GlobalCallerIdentityCache = NewCallerIdentityCache()

// CallerIdentityCacheOption lets you configure *CallerIdentityCache.
type CallerIdentityCacheOption func(*CallerIdentityCache)

// GetCallerIdentityFn is the function type to call GetCallerIdentity API.
type GetCallerIdentityFn func(ctx context.Context, cfg aws.Config) (*sts.GetCallerIdentityOutput, error)

// WithGetCallerIdentityFn lets you override the default GetCallerIdentityFn.
func WithGetCallerIdentityFn(f GetCallerIdentityFn) CallerIdentityCacheOption {
	return func(c *CallerIdentityCache) {
		c.getCallerIdentityFn = f
	}
}

// WithMaxSize lets you override the default MaxSize.
func WithMaxSize(n int) CallerIdentityCacheOption {
	return func(c *CallerIdentityCache) {
		c.maxSize = n
	}
}

// WithCache lets you bootstrap with your own cache.
func WithCache(cache map[string]*callerIdentityCacheEntry) CallerIdentityCacheOption {
	return func(c *CallerIdentityCache) {
		c.cache = cache
	}
}

// NewCallerIdentityCache returns a new empty *CallerIdentityCache.
func NewCallerIdentityCache(opts ...CallerIdentityCacheOption) *CallerIdentityCache {
	c := &CallerIdentityCache{
		cache:               map[string]*callerIdentityCacheEntry{},
		maxSize:             100,
		getCallerIdentityFn: AWSGetCallerIdentity,
		mu:                  &sync.RWMutex{},
	}
	for _, f := range opts {
		f(c)
	}
	return c
}

// CallerIdentityCache holds GetCallerIdentityOutput objects in memory so that
// we don't need to make API calls to AWS in every reconciliation of every
// resource. It has a maximum size that when it's reached, the entry that has
// the oldest access time will be removed from the cache, i.e. FIFO on last access
// time.
// Note that there is no need to invalidate the values in the cache because they
// never change so we don't need concurrency-safety to prevent access to an
// invalidated entry.
type CallerIdentityCache struct {
	// cache holds caller identity with a key whose format is the following:
	// <access_key>:<secret_key>:<token>
	// Any of the variables could be empty.
	cache map[string]*callerIdentityCacheEntry

	// maxSize is the maximum number of elements this cache can ever have.
	maxSize int

	// newClientFn returns a client that we can call GetCallerIdentity function
	/// of. You need to override the default only in the tests.
	getCallerIdentityFn GetCallerIdentityFn

	// mu is used to make sure the cache map is concurrency-safe.
	mu *sync.RWMutex
}

type callerIdentityCacheEntry struct {
	*sts.GetCallerIdentityOutput
	// accessedAt stores a time.Time and is read on the hot path without
	// holding any lock, so it must be accessed atomically.
	accessedAt atomic.Value
}

// newCallerIdentityCacheEntry returns a new *callerIdentityCacheEntry with
// its access time initialized.
func newCallerIdentityCacheEntry(o *sts.GetCallerIdentityOutput, accessedAt time.Time) *callerIdentityCacheEntry {
	e := &callerIdentityCacheEntry{GetCallerIdentityOutput: o}
	e.accessedAt.Store(accessedAt)
	return e
}

// lastAccess returns the entry's access time. An entry that was constructed
// without one reports the zero time, which makes it the first candidate for
// eviction rather than a panic on the reconciliation hot path.
func (e *callerIdentityCacheEntry) lastAccess() time.Time {
	t, _ := e.accessedAt.Load().(time.Time)
	return t
}

// GetCallerIdentity returns the identity of the caller.
func (c *CallerIdentityCache) GetCallerIdentity(ctx context.Context, cfg aws.Config, creds aws.Credentials) (*sts.GetCallerIdentityOutput, error) {
	key := fmt.Sprintf("%s:%s:%s",
		creds.AccessKeyID,
		creds.SecretAccessKey,
		creds.SessionToken,
	)
	c.mu.RLock()
	o, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		// Because this is in the hot path of the execution, i.e. all CRs get
		// here in every reconciliation, we don't want to block with a lock.
		// The access time is stored atomically so that concurrent readers do
		// not race with its refresh, and we don't refresh it on every hit so
		// that the entry is not written in every reconciliation. Concurrent
		// refreshes may overwrite each other, which is fine because any of
		// the stored values is recent enough for the LRU eviction to be
		// meaningful.
		if time.Since(o.lastAccess()) > 10*time.Minute {
			o.accessedAt.Store(time.Now())
		}
		return o.GetCallerIdentityOutput, nil
	}
	i, err := c.getCallerIdentityFn(ctx, cfg)
	if err != nil {
		return nil, errors.Wrap(err, errGetCallerIdentityFailed)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.makeRoom()
	c.cache[key] = newCallerIdentityCacheEntry(i, time.Now())
	return i, nil
}

// makeRoom ensures that there is at most maxSize-1 elements in the cache map
// so that a new entry can be added. It deletes the object that was last accessed
// before all others.
func (c *CallerIdentityCache) makeRoom() {
	if 1+len(c.cache) <= c.maxSize {
		return
	}
	var dustiest string
	for key, val := range c.cache {
		if dustiest == "" {
			dustiest = key
		}
		if val.lastAccess().Before(c.cache[dustiest].lastAccess()) {
			dustiest = key
		}
	}
	delete(c.cache, dustiest)
}

// AWSGetCallerIdentity makes sends a request to AWS to get the caller identity.
func AWSGetCallerIdentity(ctx context.Context, cfg aws.Config) (*sts.GetCallerIdentityOutput, error) {
	i, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}) //nolint:contextcheck
	return i, errors.Wrap(err, errGetCallerIdentityFailed)
}
