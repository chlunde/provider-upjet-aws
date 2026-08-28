// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

// fakeAssumeRoleClient records the sts:AssumeRole requests made against it and
// replies with the queued errors, then with a working session.
type fakeAssumeRoleClient struct {
	// errs[i] is returned for the i'th call. A nil entry, or a call beyond the
	// end of the slice, returns credentials.
	errs []error

	calls []*sts.AssumeRoleInput
}

func (f *fakeAssumeRoleClient) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.calls = append(f.calls, in)
	if i := len(f.calls) - 1; i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	return &sts.AssumeRoleOutput{
		Credentials: &ststypes.Credentials{
			AccessKeyId:     aws.String("AKIAEXAMPLE"),
			SecretAccessKey: aws.String("secret"),
			SessionToken:    aws.String("token"),
			Expiration:      aws.Time(time.Now().Add(time.Hour)),
		},
	}, nil
}

func (f *fakeAssumeRoleClient) durations() []int32 {
	got := make([]int32, 0, len(f.calls))
	for _, c := range f.calls {
		got = append(got, aws.ToInt32(c.DurationSeconds))
	}
	return got
}

// durationTooLongErr is what STS replies with when DurationSeconds exceeds the
// role's MaxSessionDuration. It is not a modelled STS error type, it arrives as
// a generic ValidationError.
var durationTooLongErr = &smithy.GenericAPIError{
	Code:    "ValidationError",
	Message: "The requested DurationSeconds exceeds the MaxSessionDuration set for this role.",
}

// TestAssumeRoleProviderSessionDuration covers the session duration asked for
// when assuming the roles in spec.assumeRoleChain. The retrieved credentials
// are frozen into the Terraform AWS client, which cannot refresh them, and that
// client is then used by an asynchronous operation that upjet lets run for up
// to an hour -- so a session has to last as long as we can make it, not the 15
// minutes the AWS SDK asks for by default.
func TestAssumeRoleProviderSessionDuration(t *testing.T) {
	role := v1beta1.AssumeRoleOptions{RoleARN: aws.String("arn:aws:iam::123456789012:role/test")}

	cases := map[string]struct {
		aro           v1beta1.AssumeRoleOptions
		errs          []error
		retrieves     int
		wantDurations []int32
		wantErr       bool
	}{
		"AsksForAnHourLongSession": {
			aro:           role,
			retrieves:     1,
			wantDurations: []int32{int32(assumeRoleSessionDuration / time.Second)},
		},
		"FallsBackWhenTheRoleCapsTheSessionDuration": {
			aro:       role,
			errs:      []error{durationTooLongErr},
			retrieves: 1,
			wantDurations: []int32{
				int32(assumeRoleSessionDuration / time.Second),
				int32(stscreds.DefaultDuration / time.Second),
			},
		},
		"KeepsUsingTheShorterSessionOnceItHasFallenBack": {
			aro:       role,
			errs:      []error{durationTooLongErr},
			retrieves: 3,
			wantDurations: []int32{
				int32(assumeRoleSessionDuration / time.Second),
				int32(stscreds.DefaultDuration / time.Second),
				int32(stscreds.DefaultDuration / time.Second),
				int32(stscreds.DefaultDuration / time.Second),
			},
		},
		"DoesNotRetryUnrelatedErrors": {
			aro:           role,
			errs:          []error{&smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform: sts:AssumeRole"}},
			retrieves:     1,
			wantDurations: []int32{int32(assumeRoleSessionDuration / time.Second)},
			wantErr:       true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := &fakeAssumeRoleClient{errs: tc.errs}
			p := NewAssumeRoleProvider(c, tc.aro)

			var lastErr error
			for i := 0; i < tc.retrieves; i++ {
				creds, err := p.Retrieve(context.Background())
				lastErr = err
				if err == nil && creds.AccessKeyID != "AKIAEXAMPLE" {
					t.Errorf("Retrieve(): unexpected credentials %+v", creds)
				}
			}
			if tc.wantErr && lastErr == nil {
				t.Error("Retrieve(): expected an error, got none")
			}
			if !tc.wantErr && lastErr != nil {
				t.Errorf("Retrieve(): unexpected error: %v", lastErr)
			}
			if diff := cmp.Diff(tc.wantDurations, c.durations()); diff != "" {
				t.Errorf("requested DurationSeconds: -want, +got:\n%s", diff)
			}
		})
	}
}

// TestAssumeRoleProviderOptions guards the options SetAssumeRoleOptions plumbs
// through, which the session duration is now layered on top of.
func TestAssumeRoleProviderOptions(t *testing.T) {
	c := &fakeAssumeRoleClient{}
	p := NewAssumeRoleProvider(c, v1beta1.AssumeRoleOptions{
		RoleARN:           aws.String("arn:aws:iam::123456789012:role/test"),
		ExternalID:        aws.String("external"),
		Tags:              []v1beta1.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
		TransitiveTagKeys: []string{"k"},
	})
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve(): unexpected error: %v", err)
	}
	if len(c.calls) != 1 {
		t.Fatalf("expected exactly one sts:AssumeRole call, got %d", len(c.calls))
	}
	got := c.calls[0]
	if diff := cmp.Diff("arn:aws:iam::123456789012:role/test", aws.ToString(got.RoleArn)); diff != "" {
		t.Errorf("RoleArn: -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff("external", aws.ToString(got.ExternalId)); diff != "" {
		t.Errorf("ExternalId: -want, +got:\n%s", diff)
	}
	tags := make(map[string]string, len(got.Tags))
	for _, tag := range got.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if diff := cmp.Diff(map[string]string{"k": "v"}, tags); diff != "" {
		t.Errorf("Tags: -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff([]string{"k"}, got.TransitiveTagKeys); diff != "" {
		t.Errorf("TransitiveTagKeys: -want, +got:\n%s", diff)
	}
}

// TestAssumeRoleProviderReportsItsSource covers the aws.CredentialProviderSource
// forwarding. aws.CredentialsCache asks the provider it wraps where the
// credentials came from, and reports it in the user agent; a wrapper that does
// not forward the call makes assume role credentials look sourceless.
func TestAssumeRoleProviderReportsItsSource(t *testing.T) {
	p := NewAssumeRoleProvider(&fakeAssumeRoleClient{}, v1beta1.AssumeRoleOptions{
		RoleARN: aws.String("arn:aws:iam::123456789012:role/test"),
	})
	src, ok := p.(aws.CredentialProviderSource)
	if !ok {
		t.Fatalf("NewAssumeRoleProvider(...) returned %T, which does not implement aws.CredentialProviderSource", p)
	}
	want := []aws.CredentialSource{aws.CredentialSourceSTSAssumeRole}
	if diff := cmp.Diff(want, src.ProviderSources()); diff != "" {
		t.Errorf("ProviderSources(): -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff(want, aws.NewCredentialsCache(p).ProviderSources()); diff != "" {
		t.Errorf("through aws.CredentialsCache, ProviderSources(): -want, +got:\n%s", diff)
	}
}
