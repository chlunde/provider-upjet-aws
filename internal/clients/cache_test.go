// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pkg/errors"
	"k8s.io/utils/ptr"
)

var errBoom = errors.New("a")

func TestGetCallerIdentity(t *testing.T) {
	type args struct {
		creds               aws.Credentials
		getCallerIdentityFn GetCallerIdentityFn
		cache               map[string]*callerIdentityCacheEntry
		maxSize             int
	}
	type want struct {
		id    *sts.GetCallerIdentityOutput
		err   error
		cache map[string]*callerIdentityCacheEntry
	}

	sample := &sts.GetCallerIdentityOutput{
		Account: ptr.To("123456789"),
		Arn:     ptr.To("arn:aws:iam::123456789:role/S3Access"),
	}
	ti := time.Now()
	cases := map[string]struct {
		reason string
		args
		want
	}{
		"NotFoundInCacheAndFail": {
			reason: "It should make the API call if the value is not cached.",
			args: args{
				getCallerIdentityFn: func(_ context.Context, _ aws.Config) (*sts.GetCallerIdentityOutput, error) {
					return nil, errBoom
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetCallerIdentityFailed),
			},
		},
		"NotFoundInCacheAndSuccess": {
			reason: "It should make the API call if the value is not cached and return the success result.",
			args: args{
				getCallerIdentityFn: func(_ context.Context, _ aws.Config) (*sts.GetCallerIdentityOutput, error) {
					return sample, nil
				},
			},
			want: want{
				id: sample,
			},
		},
		"FoundInCache": {
			reason: "It should not make the API call if the value is cached.",
			args: args{
				creds: aws.Credentials{
					AccessKeyID:     "sampleaccess",
					SecretAccessKey: "samplesecret",
					SessionToken:    "sampletoken",
				},
				cache: map[string]*callerIdentityCacheEntry{
					"sampleaccess:samplesecret:sampletoken": newCallerIdentityCacheEntry(sample, ti),
				},
			},
			want: want{
				id: sample,
			},
		},
		"CleanCache": {
			reason: "It should make sure the size of the cache is within the limits after every call and the dustiest one is deleted.",
			args: args{
				getCallerIdentityFn: func(_ context.Context, _ aws.Config) (*sts.GetCallerIdentityOutput, error) {
					return sample, nil
				},
				creds: aws.Credentials{
					AccessKeyID:     "sampleaccess",
					SecretAccessKey: "samplesecret",
					SessionToken:    "sampletoken3",
				},
				cache: map[string]*callerIdentityCacheEntry{
					"sampleaccess:samplesecret:sampletoken":  newCallerIdentityCacheEntry(sample, ti.Add(-time.Hour*1)),
					"sampleaccess:samplesecret:sampletoken2": newCallerIdentityCacheEntry(sample, ti.Add(-time.Hour*5)), // this should be deleted
				},
				maxSize: 2,
			},
			want: want{
				id: sample,
				cache: map[string]*callerIdentityCacheEntry{
					"sampleaccess:samplesecret:sampletoken":  newCallerIdentityCacheEntry(sample, ti),
					"sampleaccess:samplesecret:sampletoken3": newCallerIdentityCacheEntry(sample, ti),
				},
			},
		},
	}

	for n, tc := range cases {
		t.Run(n, func(t *testing.T) {
			opts := []CallerIdentityCacheOption{WithGetCallerIdentityFn(tc.getCallerIdentityFn)}
			if tc.args.cache != nil {
				opts = append(opts, WithCache(tc.args.cache))
			}
			if tc.maxSize != 0 {
				opts = append(opts, WithMaxSize(tc.maxSize))
			}
			c := NewCallerIdentityCache(opts...)
			id, err := c.GetCallerIdentity(context.TODO(), aws.Config{}, tc.creds)
			if diff := cmp.Diff(tc.err, err, test.EquateErrors()); diff != "" {
				t.Fatalf("%s: GetCallerIdentity(...): err -want, +got: %s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.id, id,
				cmpopts.IgnoreUnexported(sts.GetCallerIdentityOutput{}, middleware.Metadata{})); diff != "" {
				t.Fatalf("%s: GetCallerIdentity(...): -want, +got: %s", tc.reason, diff)
			}
			if tc.want.cache != nil {
				if diff := cmp.Diff(tc.want.cache, c.cache,
					cmpopts.IgnoreUnexported(callerIdentityCacheEntry{}, sts.GetCallerIdentityOutput{}, middleware.Metadata{})); diff != "" {
					t.Fatalf("%s: GetCallerIdentity(...): -want, +got: %s", tc.reason, diff)
				}
			}
		})
	}
}

// TestGetCallerIdentityConcurrent exercises the cache-hit path with
// synchronized bursts of goroutines that all observe a stale access time, so
// that several of them refresh it while the others are still reading it.
// Before accessedAt was made atomic, the refresh mutated the shared entry
// while other goroutines were reading it without a lock, and this test failed
// under the race detector.
func TestGetCallerIdentityConcurrent(t *testing.T) {
	sample := &sts.GetCallerIdentityOutput{
		Account: ptr.To("123456789"),
	}
	creds := aws.Credentials{
		AccessKeyID:     "sampleaccess",
		SecretAccessKey: "samplesecret",
		SessionToken:    "sampletoken",
	}
	for round := 0; round < 500; round++ {
		c := NewCallerIdentityCache(
			WithGetCallerIdentityFn(func(_ context.Context, _ aws.Config) (*sts.GetCallerIdentityOutput, error) {
				return sample, nil
			}),
			WithCache(map[string]*callerIdentityCacheEntry{
				"sampleaccess:samplesecret:sampletoken": newCallerIdentityCacheEntry(sample, time.Now().Add(-time.Hour)),
			}),
		)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := c.GetCallerIdentity(context.Background(), aws.Config{}, creds); err != nil {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
}
