// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

func newIRSAClusterProviderConfig(uid string) *v1beta1.ClusterProviderConfig {
	return &v1beta1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			UID:        types.UID(uid),
			Generation: 1,
		},
		Spec: v1beta1.ProviderConfigSpec{
			Credentials: v1beta1.ProviderCredentials{
				Source: authKeyIRSA,
			},
		},
	}
}

// TestRetrieveCredentialsDoesNotSerializeColdKeys asserts that a cache miss
// resolving its account ID via a slow STS round trip does not block cache
// misses for other keys. Before the STS call was moved outside the cache-wide
// write lock, the second call below could not complete until the first one's
// accountIDFn returned, and this test timed out.
func TestRetrieveCredentialsDoesNotSerializeColdKeys(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sampletoken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/sample")

	c := NewAWSCredentialsProviderCache()
	provider := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "sampleaccess"}, nil
	}))

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	blockingAccountIDFn := func(_ context.Context) (string, error) {
		close(entered)
		<-release
		return "111111111111", nil
	}
	fastAccountIDFn := func(_ context.Context) (string, error) {
		return "222222222222", nil
	}

	pc1 := newIRSAClusterProviderConfig("uid-1")
	pc2 := newIRSAClusterProviderConfig("uid-2")

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.RetrieveCredentials(context.Background(), pc1, "us-east-1", provider, blockingAccountIDFn)
		firstDone <- err
	}()
	// wait until the first call is inside its accountIDFn
	<-entered

	secondDone := make(chan error, 1)
	go func() {
		creds, err := c.RetrieveCredentials(context.Background(), pc2, "us-east-1", provider, fastAccountIDFn)
		if err == nil && creds.accountID != "222222222222" {
			t.Errorf("unexpected account ID: %s", creds.accountID)
		}
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("RetrieveCredentials(...) for the second key: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RetrieveCredentials for a different cache key blocked behind another key's in-flight account ID resolution")
	}

	releaseFn()
	if err := <-firstDone; err != nil {
		t.Fatalf("RetrieveCredentials(...) for the first key: %v", err)
	}
	got, err := c.RetrieveCredentials(context.Background(), pc1, "us-east-1", provider, fastAccountIDFn)
	if err != nil {
		t.Fatalf("RetrieveCredentials(...) cache hit for the first key: %v", err)
	}
	if got.accountID != "111111111111" {
		t.Errorf("unexpected cached account ID for the first key: %s", got.accountID)
	}
}
