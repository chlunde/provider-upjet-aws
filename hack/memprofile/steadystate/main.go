// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command steadystate answers what happens to the provider's anonymous
// footprint *after* startup: whether Go's background scavenger returns the
// idle startup heap on its own and how fast, and what sustained
// reconcile-shaped work re-grows it to once an explicit debug.FreeOSMemory()
// has returned it.
//
// Every other program in hack/memprofile reports the instant after startup.
// This one runs the same startup path and then holds the process open for an
// observation window, sampling /proc/self/smaps_rollup and runtime.MemStats
// together on a fixed interval. It never calls runtime.GC() while sampling:
// the point is to observe what the runtime does unprompted.
//
// See hack/memprofile/README.md.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/crossplane/upjet/v2/pkg/config"
	upjetresource "github.com/crossplane/upjet/v2/pkg/resource"
	upjson "github.com/crossplane/upjet/v2/pkg/resource/json"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	tf "github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	clusterapis "github.com/upbound/provider-aws/v2/apis/cluster"
	ec2v1beta2 "github.com/upbound/provider-aws/v2/apis/cluster/ec2/v1beta2"
	iamv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/iam/v1beta1"
	namespacedapis "github.com/upbound/provider-aws/v2/apis/namespaced"
	awsconfig "github.com/upbound/provider-aws/v2/config"
	"github.com/upbound/provider-aws/v2/hack/memprofile/meminfo"
)

// live holds everything a running provider keeps reachable for its whole
// lifetime. It is a package-level variable on purpose: in the idle workload
// nothing else references these, and if the collector were allowed to free
// the configured provider the measurement would be meaningless.
var live struct {
	scheme    *k8sruntime.Scheme
	fw        any
	sdk       *schema.Provider
	cluster   *config.Provider
	namespace *config.Provider
}

// reconciles counts completed workload iterations, reported with each sample.
var reconciles uint64

func main() {
	var (
		workload      = env("WORKLOAD", "idle")
		duration      = envDur("DURATION", 600*time.Second)
		interval      = envDur("INTERVAL", 15*time.Second)
		scavengeAfter = os.Getenv("SCAVENGE_AFTER_STARTUP") != ""
		scavengeEvery = envDur("SCAVENGE_EVERY", 0)
		qps           = envFloat("QPS", 0)
	)

	fmt.Printf("=== steadystate: WORKLOAD=%s DURATION=%s INTERVAL=%s SCAVENGE_AFTER_STARTUP=%v SCAVENGE_EVERY=%s QPS=%g\n",
		workload, duration, interval, scavengeAfter, scavengeEvery, qps)
	fmt.Printf("=== GOGC=%q GOMEMLIMIT=%q GOMAXPROCS=%d NumCPU=%d go=%s\n",
		os.Getenv("GOGC"), os.Getenv("GOMEMLIMIT"), runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.Version())

	ctx := context.Background()
	t0 := time.Now()

	// --- the startup path, exactly as hack/memprofile/startup runs it ---
	live.scheme = k8sruntime.NewScheme()
	must(clusterapis.AddToScheme(live.scheme))
	must(namespacedapis.AddToScheme(live.scheme))
	must(apiextensionsv1.AddToScheme(live.scheme))
	fwProvider, sdkProvider, err := xpprovider.GetProvider(ctx)
	must(err)
	live.fw, live.sdk = fwProvider, sdkProvider
	live.cluster, err = awsconfig.GetProvider(ctx, fwProvider, sdkProvider, false, false)
	must(err)
	live.namespace, err = awsconfig.GetProviderNamespaced(ctx, fwProvider, sdkProvider, false, false)
	must(err)
	fmt.Printf("=== startup path complete in %s: %d cluster resources, %d namespaced, %d scheme GVKs\n",
		time.Since(t0).Round(time.Millisecond), len(live.cluster.Resources), len(live.namespace.Resources), len(live.scheme.AllKnownTypes()))

	// A single GC so the first sample is comparable with the other harnesses,
	// which all report after runtime.GC(). GC collects; it does not release.
	runtime.GC()
	sample("startup+GC", t0)

	if scavengeAfter {
		s := time.Now()
		debug.FreeOSMemory()
		fmt.Printf("=== debug.FreeOSMemory() took %s\n", time.Since(s).Round(time.Millisecond))
		sample("post-FreeOSMemory", t0)
	}

	// --- the observation window ---
	var work func()
	if workload == "reconcile" {
		work = buildReconcileWorkload(ctx)
	}

	fmt.Printf("=== observation window: %s, sampling every %s, workload=%s\n", duration, interval, workload)
	fmt.Printf("%-8s %-18s %9s %9s %9s | %9s %9s %9s %9s %9s %9s %11s %6s | %10s\n",
		"t", "label", "Anon", "Rss", "PrivDirty",
		"HeapAlloc", "HeapSys", "HeapIdle", "HeapInuse", "HeapRel", "Sys", "TotalAlloc", "NumGC", "reconciles")

	windowStart := time.Now()
	deadline := windowStart.Add(duration)
	nextSample := windowStart
	nextScavenge := time.Time{}
	if scavengeEvery > 0 {
		nextScavenge = windowStart.Add(scavengeEvery)
	}
	// Throttle: when QPS>0 each iteration is spaced to hit that rate; QPS=0
	// runs the workload flat out.
	var period time.Duration
	if qps > 0 {
		period = time.Duration(float64(time.Second) / qps)
	}
	nextWork := windowStart

	for time.Now().Before(deadline) {
		now := time.Now()
		if !now.Before(nextSample) {
			row(windowStart)
			for !now.Before(nextSample) {
				nextSample = nextSample.Add(interval)
			}
		}
		if scavengeEvery > 0 && !now.Before(nextScavenge) {
			s := time.Now()
			debug.FreeOSMemory()
			d := time.Since(s)
			for !now.Before(nextScavenge) {
				nextScavenge = nextScavenge.Add(scavengeEvery)
			}
			rowLabel(windowStart, fmt.Sprintf("scavenge(%s)", d.Round(time.Millisecond)))
		}
		switch {
		case work == nil:
			// Idle: sleep until the next thing is due. A real provider with
			// nothing to do is not spinning either.
			until := nextSample
			if scavengeEvery > 0 && nextScavenge.Before(until) {
				until = nextScavenge
			}
			if d := time.Until(until); d > 0 {
				time.Sleep(d)
			}
		case period > 0:
			if d := time.Until(nextWork); d > 0 {
				time.Sleep(d)
			}
			work()
			reconciles++
			nextWork = nextWork.Add(period)
			if nextWork.Before(time.Now().Add(-time.Second)) {
				// Falling behind the requested rate; do not build a backlog.
				nextWork = time.Now()
			}
		default:
			work()
			reconciles++
		}
	}
	row(windowStart)
	fmt.Printf("=== window done: %d workload iterations in %s\n", reconciles, time.Since(windowStart).Round(time.Second))

	// Final explicit scavenge, so every run reports the floor it could have
	// reached at the end of the window as well as where it actually sat.
	s := time.Now()
	debug.FreeOSMemory()
	fmt.Printf("=== final debug.FreeOSMemory() took %s\n", time.Since(s).Round(time.Millisecond))
	rowLabel(windowStart, "final-scavenge")

	runtime.KeepAlive(live)
}

func row(windowStart time.Time)                { rowLabel(windowStart, "-") }
func rowLabel(windowStart time.Time, l string) { emit(time.Since(windowStart), l) }
func sample(label string, t0 time.Time)        { emitFull(label, time.Since(t0)) }

func emit(t time.Duration, label string) {
	sm := meminfo.SmapsFields()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mib := func(v uint64) float64 { return float64(v) / (1 << 20) }
	fmt.Printf("%-8s %-18s %9.1f %9.1f %9.1f | %9.1f %9.1f %9.1f %9.1f %9.1f %9.1f %11.1f %6d | %10d\n",
		t.Round(time.Second), label, sm["Anonymous"], sm["Rss"], sm["Private_Dirty"],
		mib(m.HeapAlloc), mib(m.HeapSys), mib(m.HeapIdle), mib(m.HeapInuse), mib(m.HeapReleased),
		mib(m.Sys), mib(m.TotalAlloc), m.NumGC, reconciles)
}

// emitFull prints the pre-window checkpoints in the same shape the other
// harnesses use, so they can be compared against docs/memory-footprint.md.
func emitFull(label string, since time.Duration) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mib := func(v uint64) float64 { return float64(v) / (1 << 20) }
	fmt.Printf("\n%s (t+%s)\n", label, since.Round(time.Millisecond))
	fmt.Printf("   MemStats: HeapAlloc=%.1f HeapSys=%.1f HeapIdle=%.1f HeapInuse=%.1f HeapReleased=%.1f Sys=%.1f TotalAlloc=%.1f NumGC=%d\n",
		mib(m.HeapAlloc), mib(m.HeapSys), mib(m.HeapIdle), mib(m.HeapInuse), mib(m.HeapReleased), mib(m.Sys), mib(m.TotalAlloc), m.NumGC)
	fmt.Printf("   RSS=%.1f MiB  PeakRSS(VmHWM)=%.1f MiB\n", meminfo.RSS(), meminfo.PeakRSS())
	fmt.Printf("   smaps: %s\n", meminfo.Smaps())
}

// --- the reconcile-shaped workload -----------------------------------------
//
// This is a PROXY, not a real reconcile: there is no cluster and no AWS
// account here. It replays, per iteration, the parts of the Connect+Observe
// path that are pure CPU and allocation:
//
//   - Resource.CoreConfigSchema(), which calls SchemaFunc and rebuilds the
//     resource schema from scratch (hack/memprofile/reconcile section 3/4
//     found this is done several times per Connect and is the single largest
//     per-reconcile allocator);
//   - the params -> cty -> InstanceState -> InstanceDiff -> JSON-map round
//     trip the shim performs (section 8);
//   - the typed-MR JSON round trips and DeepCopy the status write-back and
//     the state-metrics recorder pay (section 9).
//
// It does NOT model: the AWS SDK request/response cycle and its JSON/XML
// decoding, controller-runtime's informer cache (which grows with the number
// of MRs and is a genuine steady-state heap consumer this proxy has none of),
// client-go, the workqueue, or the per-Connect AWS client construction. So it
// is a lower bound on steady-state churn per reconcile and has no growing
// live set. What it is good for is the question actually being asked: given
// sustained allocation churn on the same shape of objects, does the anonymous
// footprint climb back to the startup high-water mark?
func buildReconcileWorkload(ctx context.Context) func() {
	type probe struct {
		name       string
		tr         *schema.Resource
		state, cfg map[string]any
		typed      upjetresource.Terraformed
		typedState map[string]any
		typedBuf   []byte
	}
	var probes []*probe

	add := func(name string, state, cfg map[string]any, typed upjetresource.Terraformed, typedState map[string]any) {
		r := live.cluster.Resources[name]
		if r == nil || r.TerraformResource == nil {
			fmt.Printf("=== workload: %s not configured, skipping\n", name)
			return
		}
		buf, err := upjson.TFParser.Marshal(typedState)
		must(err)
		probes = append(probes, &probe{name, r.TerraformResource, state, cfg, typed, typedState, buf})
	}

	instState := instanceStateMap()
	typedInstState := instanceStateMap()
	if l, ok := typedInstState["root_block_device"].([]any); ok && len(l) == 1 {
		// v1beta2.Instance has the singleton list converted to an embedded
		// object, as ApplyTFConversions(FromTerraform) would have reshaped it.
		typedInstState["root_block_device"] = l[0]
	}
	add("aws_iam_role", roleStateMap(), roleCfgMap(), &iamv1beta1.Role{}, roleStateMap())
	add("aws_instance", instState, instanceCfgMap(), &ec2v1beta2.Instance{}, typedInstState)

	// Schema-rebuild-only probes, to spread the churn over more resource
	// shapes than the two with hand-written state maps.
	var schemaOnly []*schema.Resource
	for _, n := range []string{"aws_s3_bucket", "aws_lb", "aws_vpc", "aws_sns_topic_subscription", "aws_security_group"} {
		if r := live.cluster.Resources[n]; r != nil && r.TerraformResource != nil {
			schemaOnly = append(schemaOnly, r.TerraformResource)
		}
	}

	// meta for the CustomizeDiff chain. Offline this may not be constructible;
	// if it is not, the diff step is skipped and said so.
	meta := buildMeta(ctx)
	diffOK := meta != nil
	if diffOK {
		// Probe once: a CustomizeDiff that needs a real AWS client would fail
		// or panic here rather than in the middle of the window.
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("=== workload: InstanceDiff panics offline (%v); diff step disabled\n", r)
					diffOK = false
				}
			}()
			// Build the same InstanceState the workload builds, so the probe
			// exercises the real call rather than an empty state.
			p := probes[0]
			block := p.tr.CoreConfigSchema()
			rawConfig, err := schema.JSONMapToStateValue(p.cfg, block)
			must(err)
			stateVal, err := schema.JSONMapToStateValue(p.state, block)
			must(err)
			st, err := p.tr.ShimInstanceStateFromValue(stateVal)
			must(err)
			st.RawPlan = stateVal
			st.RawConfig = rawConfig
			rc := tf.NewResourceConfigRaw(p.cfg)
			d, err := schema.InternalMap(p.tr.Schema).Diff(ctx, st, rc, p.tr.CustomizeDiff, meta, false)
			if err != nil {
				fmt.Printf("=== workload: InstanceDiff errors offline (%v); diff step disabled\n", err)
				diffOK = false
				return
			}
			fmt.Printf("=== workload: InstanceDiff probe ok (empty diff=%v)\n", d == nil || d.Empty())
		}()
	} else {
		fmt.Println("=== workload: no AWS client could be built offline; diff step disabled")
	}
	fmt.Printf("=== workload: %d full probes, %d schema-rebuild probes, diff step enabled=%v\n", len(probes), len(schemaOnly), diffOK)

	i := 0
	return func() {
		p := probes[i%len(probes)]
		i++

		// Connect: the schema rebuilds.
		block := p.tr.CoreConfigSchema()
		_ = p.tr.CoreConfigSchema()
		for _, r := range schemaOnly {
			_ = r.CoreConfigSchema()
		}

		// Connect: params -> cty, and cty -> InstanceState.
		rawConfig, err := schema.JSONMapToStateValue(p.cfg, block)
		must(err)
		stateVal, err := schema.JSONMapToStateValue(p.state, block)
		must(err)
		st, err := p.tr.ShimInstanceStateFromValue(stateVal)
		must(err)
		st.RawPlan = stateVal
		st.RawConfig = rawConfig

		// Observe: InstanceState -> cty -> JSON map.
		impliedType := block.ImpliedType()
		v, err := st.AttrsAsObjectValue(impliedType)
		must(err)
		stateMap, err := schema.StateValueToJSONMap(v, impliedType)
		must(err)

		// Observe: the InstanceDiff.
		if diffOK {
			rc := tf.NewResourceConfigRaw(p.cfg)
			if _, err := schema.InternalMap(p.tr.Schema).Diff(ctx, st, rc, p.tr.CustomizeDiff, meta, false); err != nil {
				panic(err)
			}
		}

		// Observe: the late-initialization buffer.
		if _, err := upjson.TFParser.Marshal(stateMap); err != nil {
			panic(err)
		}

		// Typed MR: status write-back, late-init merge, and the DeepCopy the
		// state-metrics recorder performs per object per poll.
		must(p.typed.SetObservation(p.typedState))
		if _, err := p.typed.LateInitialize(p.typedBuf); err != nil {
			panic(err)
		}
		if _, err := p.typed.GetObservation(); err != nil {
			panic(err)
		}
		if _, err := p.typed.GetParameters(); err != nil {
			panic(err)
		}
		_ = p.typed.DeepCopyObject()
	}
}

// buildMeta constructs the AWS client the SDK passes as meta to CustomizeDiff,
// with every network-touching validation skipped. Returns nil if it cannot.
func buildMeta(ctx context.Context) (out any) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("=== workload: building the AWS client panicked (%v)\n", r)
			out = nil
		}
	}()
	m, ok := live.sdk.Meta().(*xpprovider.AWSClient)
	if !ok {
		return nil
	}
	cfg := xpprovider.AWSConfig{
		AccessKey:               "AKIAIOSFODNN7EXAMPLE",
		SecretKey:               "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:                  "us-east-1",
		Endpoints:               map[string]string{},
		SkipCredsValidation:     true,
		SkipRegionValidation:    true,
		SkipRequestingAccountId: true,
	}
	c := &xpprovider.AWSClient{}
	c.SetServicePackagesField(m.GetServicePackages())
	built, diags := cfg.GetClient(ctx, c)
	if diags.HasError() {
		fmt.Printf("=== workload: AWSConfig.GetClient failed offline (%v)\n", diags)
		return nil
	}
	return built
}

// --- fixtures, copied from hack/memprofile/reconcile ------------------------

func roleStateMap() map[string]any {
	return map[string]any{
		"id":                    "app-role",
		"arn":                   "arn:aws:iam::123456789012:role/app-role",
		"assume_role_policy":    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
		"create_date":           "2024-01-01T00:00:00Z",
		"description":           "role for the steady-state probe",
		"force_detach_policies": false,
		"max_session_duration":  float64(3600),
		"name":                  "app-role",
		"path":                  "/",
		"tags":                  map[string]any{"env": "prod", "team": "platform"},
		"tags_all":              map[string]any{"env": "prod", "team": "platform"},
		"unique_id":             "AROAEXAMPLEID",
	}
}

func roleCfgMap() map[string]any {
	return map[string]any{
		"assume_role_policy":    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
		"description":           "role for the steady-state probe",
		"force_detach_policies": false,
		"max_session_duration":  float64(3600),
		"name":                  "app-role",
		"path":                  "/",
		"tags":                  map[string]any{"env": "prod", "team": "platform"},
		"tags_all":              map[string]any{"env": "prod", "team": "platform"},
	}
}

func instanceStateMap() map[string]any {
	return map[string]any{
		"id":                          "i-0123456789abcdef0",
		"ami":                         "ami-0abcdef1234567890",
		"arn":                         "arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0",
		"associate_public_ip_address": false,
		"availability_zone":           "us-east-1a",
		"ebs_optimized":               true,
		"instance_state":              "running",
		"instance_type":               "m5.large",
		"key_name":                    "ops",
		"monitoring":                  false,
		"private_dns":                 "ip-10-0-0-10.ec2.internal",
		"private_ip":                  "10.0.0.10",
		"region":                      "us-east-1",
		"root_block_device": []any{map[string]any{
			"delete_on_termination": true,
			"device_name":           "/dev/xvda",
			"encrypted":             true,
			"iops":                  float64(3000),
			"throughput":            float64(125),
			"volume_id":             "vol-0123456789abcdef0",
			"volume_size":           float64(50),
			"volume_type":           "gp3",
			"tags":                  map[string]any{"env": "prod"},
		}},
		"source_dest_check":      true,
		"subnet_id":              "subnet-0123456789abcdef0",
		"tags":                   map[string]any{"env": "prod", "team": "platform", "app": "web"},
		"tags_all":               map[string]any{"env": "prod", "team": "platform", "app": "web"},
		"tenancy":                "default",
		"vpc_security_group_ids": []any{"sg-0123456789abcdef0", "sg-0123456789abcdef1"},
	}
}

func instanceCfgMap() map[string]any {
	return map[string]any{
		"ami":                         "ami-0abcdef1234567890",
		"associate_public_ip_address": false,
		"ebs_optimized":               true,
		"instance_type":               "m5.large",
		"key_name":                    "ops",
		"monitoring":                  false,
		"region":                      "us-east-1",
		"root_block_device": []any{map[string]any{
			"delete_on_termination": true,
			"encrypted":             true,
			"iops":                  float64(3000),
			"throughput":            float64(125),
			"volume_size":           float64(50),
			"volume_type":           "gp3",
			"tags":                  map[string]any{"env": "prod"},
		}},
		"source_dest_check":      true,
		"subnet_id":              "subnet-0123456789abcdef0",
		"tags":                   map[string]any{"env": "prod", "team": "platform", "app": "web"},
		"tags_all":               map[string]any{"env": "prod", "team": "platform", "app": "web"},
		"tenancy":                "default",
		"vpc_security_group_ids": []any{"sg-0123456789abcdef0", "sg-0123456789abcdef1"},
	}
}

// --- small helpers ----------------------------------------------------------

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	must(err)
	return d
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	must(err)
	return f
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
