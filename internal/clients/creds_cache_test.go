// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

// backdate moves the resolution time of every cached entry d into the past,
// so that TTL expiry can be exercised without sleeping or a fake clock.
func backdate(c *AWSCredentialsProviderCache, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.cache {
		e.resolvedAt = e.resolvedAt.Add(-d)
	}
}

func cachePC(uid string, generation int64, source xpv2.CredentialsSource) *v1beta1.ClusterProviderConfig {
	return &v1beta1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sample",
			UID:        types.UID(uid),
			Generation: generation,
		},
		Spec: v1beta1.ProviderConfigSpec{
			Credentials: v1beta1.ProviderCredentials{
				Source: source,
			},
		},
	}
}

// countingResolver returns an AWSConfigResolverFn that counts its
// invocations and resolves to a config whose credentials carry the given
// marker as access key ID.
func countingResolver(calls *atomic.Int32, marker string) AWSConfigResolverFn {
	return func(context.Context) (*aws.Config, error) {
		calls.Add(1)
		return &aws.Config{
			Region: "us-east-1",
			Credentials: aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: marker}, nil
			})),
		}, nil
	}
}

func countingAccountIDFn(calls *atomic.Int32, accountID string) AccountIDFn {
	return func(context.Context, *aws.Config, aws.Credentials) (string, error) {
		calls.Add(1)
		return accountID, nil
	}
}

// TestRetrieveConfigCachesSecretRead exercises the cache with the real
// resolution path for `source: Secret`: the second reconcile with the same
// ProviderConfig and region must not read the credential Secret from the
// API server, and must not call the account ID (STS) function.
func TestRetrieveConfigCachesSecretRead(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var secretGets, accountIDCalls atomic.Int32
	kube := &test.MockClient{
		MockGet: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
			s, ok := obj.(*corev1.Secret)
			if !ok {
				return errors.Errorf("unexpected GET for %T %s", obj, key)
			}
			secretGets.Add(1)
			s.Data = map[string][]byte{
				"creds": []byte("[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = examplesecret\n"),
			}
			return nil
		},
	}
	pc := cachePC("uid-secret", 1, xpv2.CredentialsSourceSecret)
	pc.Spec.Credentials.SecretRef = &xpv2.SecretKeySelector{
		SecretReference: xpv2.SecretReference{Name: "aws-creds", Namespace: "crossplane-system"},
		Key:             "creds",
	}
	mg := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
	}}

	c := NewAWSCredentialsProviderCache()
	resolver := func(ctx context.Context) (*aws.Config, error) {
		return GetAWSConfigWithoutTracking(ctx, kube, mg, pc)
	}
	accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

	for i := 0; i < 2; i++ {
		cfg, creds, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn)
		if err != nil {
			t.Fatalf("RetrieveConfig(...) call %d: %v", i+1, err)
		}
		if cfg.Region != "us-east-1" {
			t.Errorf("call %d: unexpected region %q", i+1, cfg.Region)
		}
		if creds.creds.AccessKeyID != "AKIAEXAMPLE" {
			t.Errorf("call %d: unexpected access key ID %q", i+1, creds.creds.AccessKeyID)
		}
		if creds.accountID != "123456789012" {
			t.Errorf("call %d: unexpected account ID %q", i+1, creds.accountID)
		}
		// The returned config must be a per-call copy: mutating it (as the
		// caller does when defaulting the region of global resources) must
		// not leak into the cache.
		cfg.Region = "mutated-by-caller"
	}
	if got := secretGets.Load(); got != 1 {
		t.Errorf("credential Secret was read %d times over two reconciles, want exactly 1", got)
	}
	if got := accountIDCalls.Load(); got != 1 {
		t.Errorf("account ID was resolved %d times over two reconciles, want exactly 1", got)
	}
}

// TestRetrieveConfigNonIRSASources asserts that each non-IRSA credential
// source is cached: two retrievals resolve once, and the TTL forces a fresh
// resolution afterwards.
func TestRetrieveConfigNonIRSASources(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	for _, source := range []xpv2.CredentialsSource{
		xpv2.CredentialsSourceSecret,
		"WebIdentity",
		"PodIdentity",
		"Upbound",
	} {
		t.Run(string(source), func(t *testing.T) {
			var resolves, accountIDCalls atomic.Int32
			c := NewAWSCredentialsProviderCache()
			pc := cachePC("uid-"+string(source), 1, source)
			resolver := countingResolver(&resolves, "key-"+string(source))
			accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

			for i := 0; i < 2; i++ {
				_, creds, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn)
				if err != nil {
					t.Fatalf("RetrieveConfig(...) call %d: %v", i+1, err)
				}
				if creds.creds.AccessKeyID != "key-"+string(source) {
					t.Errorf("call %d: unexpected access key ID %q", i+1, creds.creds.AccessKeyID)
				}
			}
			if got := resolves.Load(); got != 1 {
				t.Fatalf("resolved %d times over two reconciles, want exactly 1", got)
			}
			if got := accountIDCalls.Load(); got != 1 {
				t.Fatalf("account ID was resolved %d times over two reconciles, want exactly 1", got)
			}

			// within the TTL the entry stays cached...
			backdate(c, awsConfigCacheTTL-time.Minute)
			if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
				t.Fatal(err)
			}
			if got := resolves.Load(); got != 1 {
				t.Fatalf("resolved %d times within the TTL, want exactly 1", got)
			}
			// ... and past the TTL it is resolved again.
			backdate(c, 2*time.Minute)
			if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
				t.Fatal(err)
			}
			if got := resolves.Load(); got != 2 {
				t.Fatalf("resolved %d times after TTL expiry, want exactly 2", got)
			}
		})
	}
}

// TestRetrieveConfigGenerationInvalidates asserts that a modified
// ProviderConfig (a new generation) is resolved from scratch.
func TestRetrieveConfigGenerationInvalidates(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var resolves, accountIDCalls atomic.Int32
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-gen", 1, xpv2.CredentialsSourceSecret)
	resolver := countingResolver(&resolves, "key")
	accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
	pc.Generation = 2
	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
	if got := resolves.Load(); got != 2 {
		t.Fatalf("resolved %d times across a generation change, want exactly 2", got)
	}
}

// TestRetrieveConfigRegionsDoNotCollide asserts that managed resources in
// different regions sharing a ProviderConfig get distinct cache entries and
// are served the credentials resolved for their own region.
func TestRetrieveConfigRegionsDoNotCollide(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var resolvesEast, resolvesWest, accountIDCalls atomic.Int32
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-region", 1, xpv2.CredentialsSourceSecret)
	east := countingResolver(&resolvesEast, "key-east")
	west := countingResolver(&resolvesWest, "key-west")
	accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

	for i, tt := range []struct {
		region   string
		resolver AWSConfigResolverFn
		want     string
	}{
		{"us-east-1", east, "key-east"},
		{"eu-west-1", west, "key-west"},
		{"us-east-1", east, "key-east"},
		{"eu-west-1", west, "key-west"},
	} {
		_, creds, err := c.RetrieveConfig(context.Background(), pc, tt.region, tt.resolver, accountIDFn)
		if err != nil {
			t.Fatalf("RetrieveConfig(...) call %d: %v", i+1, err)
		}
		if creds.creds.AccessKeyID != tt.want {
			t.Errorf("call %d for region %s: got access key ID %q, want %q", i+1, tt.region, creds.creds.AccessKeyID, tt.want)
		}
	}
	if got := resolvesEast.Load(); got != 1 {
		t.Errorf("us-east-1 was resolved %d times, want exactly 1", got)
	}
	if got := resolvesWest.Load(); got != 1 {
		t.Errorf("eu-west-1 was resolved %d times, want exactly 1", got)
	}
}

// TestRetrieveConfigIRSA asserts that the pre-existing IRSA caching
// behavior is preserved: entries do not expire with the TTL, and a rotation
// of the projected token file (a new content hash) yields a fresh
// resolution because the hash is part of the cache key.
func TestRetrieveConfigIRSA(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWebIdentityTokenFile, tokenFile)
	t.Setenv(envWebIdentityRoleARN, "arn:aws:iam::123456789012:role/sample")

	var resolves, accountIDCalls atomic.Int32
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-irsa", 1, authKeyIRSA)
	resolver := countingResolver(&resolves, "key-irsa")
	accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

	for i := 0; i < 2; i++ {
		if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
			t.Fatalf("RetrieveConfig(...) call %d: %v", i+1, err)
		}
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("resolved %d times over two reconciles, want exactly 1", got)
	}
	// IRSA entries have no TTL.
	backdate(c, 10*awsConfigCacheTTL)
	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("IRSA entry was resolved %d times after the TTL, want exactly 1 (IRSA entries must not expire)", got)
	}
	// A rotated token changes the key and forces a fresh resolution.
	if err := os.WriteFile(tokenFile, []byte("token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
	if got := resolves.Load(); got != 2 {
		t.Fatalf("resolved %d times after a token rotation, want exactly 2", got)
	}
}

// TestRetrieveConfigConcurrentSingleResolution asserts that a burst of
// concurrent callers for the same key performs the slow resolution exactly
// once.
func TestRetrieveConfigConcurrentSingleResolution(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var resolves, accountIDCalls atomic.Int32
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-concurrent", 1, xpv2.CredentialsSourceSecret)
	resolver := func(ctx context.Context) (*aws.Config, error) {
		// slow things down so the callers genuinely overlap
		time.Sleep(50 * time.Millisecond)
		return countingResolver(&resolves, "key")(ctx)
	}
	accountIDFn := countingAccountIDFn(&accountIDCalls, "123456789012")

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, creds, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn)
			if err != nil {
				t.Error(err)
				return
			}
			if creds.accountID != "123456789012" {
				t.Errorf("unexpected account ID %q", creds.accountID)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := resolves.Load(); got != 1 {
		t.Errorf("32 concurrent callers resolved %d times, want exactly 1", got)
	}
	if got := accountIDCalls.Load(); got != 1 {
		t.Errorf("32 concurrent callers resolved the account ID %d times, want exactly 1", got)
	}
}

// TestRetrieveConfigNoLockHeldDuringResolution asserts that the cache-wide
// lock is not held while the resolver or the account ID function performs
// its (potentially slow, network-bound) work.
func TestRetrieveConfigNoLockHeldDuringResolution(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-lock", 1, xpv2.CredentialsSourceSecret)
	assertUnlocked := func(stage string) {
		if !c.mu.TryLock() {
			t.Errorf("cache-wide write lock is held during %s", stage)
			return
		}
		c.mu.Unlock()
	}
	resolver := func(context.Context) (*aws.Config, error) {
		assertUnlocked("config resolution")
		return &aws.Config{
			Region: "us-east-1",
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				assertUnlocked("credential retrieval")
				return aws.Credentials{AccessKeyID: "key"}, nil
			}),
		}, nil
	}
	accountIDFn := func(context.Context, *aws.Config, aws.Credentials) (string, error) {
		assertUnlocked("account ID resolution")
		return "123456789012", nil
	}
	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
}

// TestRetrieveConfigInvalidatesOnRetrieveFailure asserts that a cached
// entry whose credentials stop working (e.g. the Secret was rotated and the
// old keys were deactivated before the TTL expired) is invalidated and
// resolved from scratch within the same call, instead of serving the dead
// credentials until the TTL expires.
func TestRetrieveConfigInvalidatesOnRetrieveFailure(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var resolves atomic.Int32
	var fail atomic.Bool
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-invalidate", 1, xpv2.CredentialsSourceSecret)
	resolver := func(context.Context) (*aws.Config, error) {
		n := resolves.Add(1)
		return &aws.Config{
			Region: "us-east-1",
			// The first resolution hands out a provider wired to the fail
			// switch; the re-resolution hands out a healthy one, standing in
			// for the provider built from the rotated Secret.
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				if n == 1 && fail.Load() {
					return aws.Credentials{}, errors.New("the AWS access key is deactivated")
				}
				return aws.Credentials{AccessKeyID: "key"}, nil
			}),
		}, nil
	}
	accountIDFn := countingAccountIDFn(&atomic.Int32{}, "123456789012")

	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	_, creds, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn)
	if err != nil {
		t.Fatalf("RetrieveConfig(...) after credential deactivation: %v", err)
	}
	if creds.creds.AccessKeyID != "key" {
		t.Errorf("unexpected access key ID %q after re-resolution", creds.creds.AccessKeyID)
	}
	if got := resolves.Load(); got != 2 {
		t.Errorf("resolved %d times across a credential deactivation, want exactly 2", got)
	}
}

// TestRetrieveConfigResolveErrorsAreNotCached asserts that a failed
// resolution is not cached: the next call tries again.
func TestRetrieveConfigResolveErrorsAreNotCached(t *testing.T) {
	t.Setenv(envWebIdentityTokenFile, "")
	var resolves atomic.Int32
	c := NewAWSCredentialsProviderCache()
	pc := cachePC("uid-err", 1, xpv2.CredentialsSourceSecret)
	resolver := func(ctx context.Context) (*aws.Config, error) {
		if resolves.Add(1) == 1 {
			return nil, errors.New("transient failure")
		}
		return countingResolver(&atomic.Int32{}, "key")(ctx)
	}
	accountIDFn := countingAccountIDFn(&atomic.Int32{}, "123456789012")

	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err == nil {
		t.Fatal("expected the first RetrieveConfig(...) to fail")
	}
	if _, _, err := c.RetrieveConfig(context.Background(), pc, "us-east-1", resolver, accountIDFn); err != nil {
		t.Fatalf("RetrieveConfig(...) after a transient resolution failure: %v", err)
	}
	if got := resolves.Load(); got != 2 {
		t.Errorf("resolver was called %d times, want exactly 2", got)
	}
}
