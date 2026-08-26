// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command reconcile measures the per-reconcile costs that the Connect and
// Observe path pays for every managed resource, including a steady-state
// observation of an object nobody has modified.
//
// See hack/memprofile/README.md.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/crossplane/upjet/v2/pkg/config"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	tf "github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"

	awsconfig "github.com/upbound/provider-aws/v2/config"
)

// probes are resources chosen to span the schema-size range.
var probes = []string{
	"aws_instance",
	"aws_s3_bucket",
	"aws_iam_role",
	"aws_lb",
	"aws_vpc",
	"aws_sns_topic_subscription",
}

func allocs(n int, fn func()) (time.Duration, uint64) {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	start := time.Now()
	for range n {
		fn()
	}
	d := time.Since(start) / time.Duration(n)
	runtime.ReadMemStats(&b)
	return d, (b.TotalAlloc - a.TotalAlloc) / uint64(n) //nolint:gosec // n is a small positive constant
}

func main() {
	ctx := context.Background()
	fw, sdk, err := xpprovider.GetProvider(ctx)
	must(err)
	pc, err := awsconfig.GetProvider(ctx, fw, sdk, false, false)
	must(err)

	fmt.Println("=== 1. Is SchemaFunc left set after upjet materialises Schema? ===")
	withFunc, withSchema, both := 0, 0, 0
	for _, r := range pc.Resources {
		tr := r.TerraformResource
		if tr == nil {
			continue
		}
		if tr.SchemaFunc != nil {
			withFunc++
		}
		if tr.Schema != nil {
			withSchema++
		}
		if tr.SchemaFunc != nil && tr.Schema != nil {
			both++
		}
	}
	fmt.Printf("   configured resources        : %d\n", len(pc.Resources))
	fmt.Printf("   with .Schema set            : %d\n", withSchema)
	fmt.Printf("   with .SchemaFunc set        : %d\n", withFunc)
	fmt.Printf("   with BOTH set               : %d   <- InternalValidate calls this an error\n\n", both)

	fmt.Println("=== 2. Do the provider's schema mutations survive into SchemaMap()? ===")
	missingInLive, extraInLive, flagsDiffer, ptrOnly := 0, 0, 0, 0
	affected := map[string]bool{}
	var rows []string
	note := func(format string, a ...any) {
		rows = append(rows, fmt.Sprintf(format, a...))
	}
	names := make([]string, 0, len(pc.Resources))
	for name := range pc.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tr := pc.Resources[name].TerraformResource
		if tr == nil || tr.SchemaFunc == nil || tr.Schema == nil {
			continue
		}
		live := tr.SchemaMap() // what every TF SDK code path actually uses
		keys := make([]string, 0, len(tr.Schema))
		for k := range tr.Schema {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ms := tr.Schema[k]
			ls, ok := live[k]
			if !ok {
				extraInLive++ // present in mutated map, absent from live
				affected[name] = true
				note("ONLY-IN-DIFF   %s.%s diff{req=%v opt=%v comp=%v sens=%v type=%v} (absent from apply schema)",
					name, k, ms.Required, ms.Optional, ms.Computed, ms.Sensitive, ms.Type)
				continue
			}
			if ms == ls {
				continue // shared pointer: the edit reached both schemas
			}
			diffs := diffAttrsDeep(ms, ls, "")
			switch {
			case len(diffs) > 0:
				flagsDiffer++
				affected[name] = true
				note("FLAGS-DIFFER   %s.%s %s", name, k, strings.Join(diffs, " "))
			default:
				ptrOnly++ // replaced *schema.Schema, all visible fields equal recursively
			}
		}
		liveKeys := make([]string, 0, len(live))
		for k := range live {
			liveKeys = append(liveKeys, k)
		}
		sort.Strings(liveKeys)
		for _, k := range liveKeys {
			if _, ok := tr.Schema[k]; !ok {
				missingInLive++ // deleted from mutated map, still live
				affected[name] = true
				ls := live[k]
				note("DELETED-LIVE   %s.%s apply{req=%v opt=%v comp=%v sens=%v type=%v} (deleted from diff schema)",
					name, k, ls.Required, ls.Optional, ls.Computed, ls.Sensitive, ls.Type)
			}
		}
	}
	fmt.Printf("   resources with any divergence : %d of %d\n", len(affected), both)
	fmt.Printf("   attrs only in .Schema         : %d\n", extraInLive)
	fmt.Printf("   attrs deleted but still live  : %d\n", missingInLive)
	fmt.Printf("   attrs whose flags differ      : %d\n", flagsDiffer)
	fmt.Printf("   attrs with replaced pointer only (visible flags equal): %d\n", ptrOnly)
	fmt.Println("   full inventory:")
	for _, e := range rows {
		fmt.Printf("     %s\n", e)
	}
	fmt.Println()

	fmt.Println("=== 2b. Does SchemaFunc return stable attribute pointers across calls? ===")
	stable, regen, mixed := 0, 0, 0
	for _, name := range names {
		tr := pc.Resources[name].TerraformResource
		if tr == nil || tr.SchemaFunc == nil {
			continue
		}
		a, b := tr.SchemaFunc(), tr.SchemaFunc()
		same, diff := 0, 0
		for k, av := range a {
			if bv, ok := b[k]; ok && av == bv {
				same++
			} else {
				diff++
			}
		}
		switch {
		case diff == 0:
			stable++
		case same == 0:
			regen++
		default:
			mixed++
		}
	}
	fmt.Printf("   resources whose SchemaFunc returns identical pointers   : %d\n", stable)
	fmt.Printf("   resources whose SchemaFunc rebuilds every attr object   : %d\n", regen)
	fmt.Printf("   resources with a mix of shared and rebuilt attr objects : %d\n\n", mixed)

	fmt.Println("=== 2c. Shared tags-schema singleton state (diff view vs fresh SchemaFunc view) ===")
	for _, name := range []string{"aws_instance", "aws_vpc", "aws_s3_bucket", "aws_iam_role"} {
		tr := pc.Resources[name].TerraformResource
		if tr == nil {
			continue
		}
		live := tr.SchemaMap()
		mt, lt := tr.Schema["tags"], live["tags"]
		if mt == nil || lt == nil {
			continue
		}
		fmt.Printf("   %-30s tags: samePtr=%v diff{opt=%v comp=%v} fresh{opt=%v comp=%v}\n",
			name, mt == lt, mt.Optional, mt.Computed, lt.Optional, lt.Computed)
	}
	if tr := pc.Resources["aws_instance"].TerraformResource; tr != nil {
		if ebd, ok := tr.Schema["ebs_block_device"]; ok {
			if er, ok := ebd.Elem.(*schema.Resource); ok {
				t := er.Schema["tags"]
				fmt.Printf("   aws_instance.ebs_block_device.tags diff view: opt=%v comp=%v\n", t.Optional, t.Computed)
			}
		}
		live := tr.SchemaMap()
		if ebd, ok := live["ebs_block_device"]; ok {
			if er, ok := ebd.Elem.(*schema.Resource); ok {
				t := er.Schema["tags"]
				fmt.Printf("   aws_instance.ebs_block_device.tags fresh view: opt=%v comp=%v\n", t.Optional, t.Computed)
			}
		}
	}
	fmt.Println()

	fmt.Println("=== 3. Cost of the schema rebuild the reconcile path pays ===")
	fmt.Printf("   %-30s %10s %12s %10s %12s\n", "resource", "SchemaMap", "alloc", "CoreConfig", "alloc")
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil {
			continue
		}
		tr := r.TerraformResource
		dm, am := allocs(50, func() { _ = tr.SchemaMap() })
		dc, ac := allocs(50, func() { _ = tr.CoreConfigSchema() })
		fmt.Printf("   %-30s %10s %10s KB %10s %10s KB\n",
			name, dm.Round(time.Microsecond), kb(am), dc.Round(time.Microsecond), kb(ac))
	}

	fmt.Println("\n   same resources with SchemaFunc cleared (the proposed fix):")
	fmt.Printf("   %-30s %10s %12s %10s %12s\n", "resource", "SchemaMap", "alloc", "CoreConfig", "alloc")
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil {
			continue
		}
		tr := r.TerraformResource
		saved := tr.SchemaFunc
		tr.SchemaFunc = nil
		dm, am := allocs(50, func() { _ = tr.SchemaMap() })
		dc, ac := allocs(50, func() { _ = tr.CoreConfigSchema() })
		tr.SchemaFunc = saved
		fmt.Printf("   %-30s %10s %10s KB %10s %10s KB\n",
			name, dm.Round(time.Microsecond), kb(am), dc.Round(time.Microsecond), kb(ac))
	}

	fmt.Println("\n=== 4. SchemaFunc invocations per reconcile ===")
	countSchemaFuncCalls(pc)

	fmt.Println("\n=== 5. Per-Connect construction in internal/clients/aws.go ===")
	var tfClient *xpprovider.AWSClient
	if os.Getenv("SKIP_FW") == "" {
		tfClient = measureFrameworkProvider(ctx, sdk)
	}

	fmt.Println("\n=== 6. Per-Connect work on the Terraform Plugin Framework path ===")
	if os.Getenv("SKIP_FW") == "" && tfClient != nil {
		measureFrameworkConnectPath(ctx, pc, tfClient)
	}

	fmt.Println("\n=== 7. Does a spec tags change still produce a diff? (aws_iam_role) ===")
	if tfClient != nil {
		tagsDiffExperiment(ctx, pc, tfClient)
	}

	// SCHEMA_DUMP=<path>: dump the flags of every attribute as the configured
	// runtime sees them (SchemaMap() of the configured provider), in the same
	// format as hack/memprofile/schemadump, for cross-process comparison.
	if path := os.Getenv("SCHEMA_DUMP"); path != "" {
		f, err := os.Create(path) //nolint:gosec // developer-supplied path in a throwaway harness
		must(err)
		w := bufio.NewWriter(f)
		resources := map[string]*schema.Resource{}
		for name, r := range pc.Resources {
			if r.TerraformResource != nil {
				resources[name] = r.TerraformResource
			}
		}
		dumpResources(w, resources)
		must(w.Flush())
		must(f.Close())
		fmt.Printf("\nwrote configured-schema dump to %s\n", path)
	}
	_ = schema.TypeString
}

func dumpAttrFlags(w *bufio.Writer, res, path string, s *schema.Schema) {
	fmt.Fprintf(w, "%s|%s|req=%v,opt=%v,comp=%v,sens=%v,forcenew=%v,type=%v\n",
		res, path, s.Required, s.Optional, s.Computed, s.Sensitive, s.ForceNew, s.Type)
	switch e := s.Elem.(type) {
	case *schema.Schema:
		dumpAttrFlags(w, res, path+".elem", e)
	case *schema.Resource:
		keys := make([]string, 0, len(e.Schema))
		for k := range e.Schema {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dumpAttrFlags(w, res, path+"."+k, e.Schema[k])
		}
	}
}

func dumpResources(w *bufio.Writer, resources map[string]*schema.Resource) {
	names := make([]string, 0, len(resources))
	for k := range resources {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		m := resources[name].SchemaMap()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dumpAttrFlags(w, name, k, m[k])
		}
	}
}

// tagsDiffExperiment computes the same InstanceDiff the Observe path computes,
// for a state with tags {env:a} and a desired config with tags {env:b}, first
// with the schema exactly as the runtime has it, then with the tags attribute
// flags restored to their upstream values (Optional, not Computed).
func tagsDiffExperiment(ctx context.Context, pc *config.Provider, meta any) {
	tr := pc.Resources["aws_iam_role"].TerraformResource
	state := &tf.InstanceState{
		ID: "somerole",
		Attributes: map[string]string{
			"id": "somerole", "name": "somerole",
			"assume_role_policy": "{}",
			"tags.%":             "1", "tags.env": "a",
			"tags_all.%": "1", "tags_all.env": "a",
		},
	}
	cfgMap := map[string]any{
		"name":               "somerole",
		"assume_role_policy": "{}",
		"tags":               map[string]any{"env": "b"},
		"tags_all":           map[string]any{"env": "b"},
	}
	// mimic the RawPlan/RawConfig reconstruction Connect performs
	block := tr.CoreConfigSchema()
	stateMap := map[string]any{
		"id": "somerole", "name": "somerole", "assume_role_policy": "{}",
		"tags": map[string]any{"env": "a"}, "tags_all": map[string]any{"env": "a"},
	}
	stateVal, err := schema.JSONMapToStateValue(stateMap, block)
	must(err)
	rawConfig, err := schema.JSONMapToStateValue(cfgMap, block)
	must(err)
	state.RawPlan = stateVal
	state.RawConfig = rawConfig
	run := func(label string) {
		rc := tf.NewResourceConfigRaw(cfgMap)
		d, err := schema.InternalMap(tr.Schema).Diff(ctx, state, rc, tr.CustomizeDiff, meta, false)
		if err != nil {
			fmt.Printf("   %-45s diff error: %v\n", label, err)
			return
		}
		if d == nil || d.Empty() {
			fmt.Printf("   %-45s NO DIFF\n", label)
			return
		}
		keys := make([]string, 0, len(d.Attributes))
		for k := range d.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			ad := d.Attributes[k]
			parts = append(parts, fmt.Sprintf("%s:%q->%q", k, ad.Old, ad.New))
		}
		fmt.Printf("   %-45s DIFF %s\n", label, strings.Join(parts, " "))
	}
	ts := tr.Schema["tags"]
	fmt.Printf("   runtime tags schema: opt=%v comp=%v\n", ts.Optional, ts.Computed)
	run("update tags, runtime schema")
	saveOpt, saveComp := ts.Optional, ts.Computed
	ts.Optional, ts.Computed = true, false
	run("update tags, upstream flags restored")
	ts.Optional, ts.Computed = saveOpt, saveComp

	// removal case: the user deletes all tags from the spec
	delete(cfgMap, "tags")
	delete(cfgMap, "tags_all")
	rawConfig, err = schema.JSONMapToStateValue(cfgMap, block)
	must(err)
	state.RawConfig = rawConfig
	run("remove all tags, runtime schema")
	ts.Optional, ts.Computed = true, false
	run("remove all tags, upstream flags restored")
	ts.Optional, ts.Computed = saveOpt, saveComp

	// create case: no prior state at all, tags in the config
	cfgMap["tags"] = map[string]any{"env": "b"}
	cfgMap["tags_all"] = map[string]any{"env": "b"}
	rawConfig, err = schema.JSONMapToStateValue(cfgMap, block)
	must(err)
	emptyVal, err := schema.JSONMapToStateValue(map[string]any{}, block)
	must(err)
	createState := &tf.InstanceState{RawPlan: rawConfig, RawConfig: rawConfig}
	_ = emptyVal
	runWith := func(label string, st *tf.InstanceState) {
		rc := tf.NewResourceConfigRaw(cfgMap)
		d, err := schema.InternalMap(tr.Schema).Diff(ctx, st, rc, tr.CustomizeDiff, meta, false)
		if err != nil {
			fmt.Printf("   %-45s diff error: %v\n", label, err)
			return
		}
		if d == nil || d.Empty() {
			fmt.Printf("   %-45s NO DIFF\n", label)
			return
		}
		var parts []string
		keys := make([]string, 0, len(d.Attributes))
		for k := range d.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasPrefix(k, "tags") {
				ad := d.Attributes[k]
				parts = append(parts, fmt.Sprintf("%s:%q->%q(computed=%v)", k, ad.Old, ad.New, ad.NewComputed))
			}
		}
		fmt.Printf("   %-45s tags portion of create diff: %s\n", label, strings.Join(parts, " "))
	}
	runWith("create with tags, runtime schema", createState)
	ts.Optional, ts.Computed = true, false
	runWith("create with tags, upstream flags restored", createState)
	ts.Optional, ts.Computed = saveOpt, saveComp

	fmt.Println("\n=== 7b. Does a policy change still produce a diff? (aws_sns_topic) ===")
	policyDiffExperiment(ctx, pc, meta)
}

// policyDiffExperiment repeats the experiment for the IAM policy document
// schema singleton, on aws_sns_topic whose policy attribute is
// sdkv2.IAMPolicyDocumentSchemaOptionalComputed().
func policyDiffExperiment(ctx context.Context, pc *config.Provider, meta any) {
	tr := pc.Resources["aws_sns_topic"].TerraformResource
	polA := `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"sns:Publish","Resource":"*","Principal":{"AWS":"111111111111"}}]}`
	polB := `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"sns:Publish","Resource":"*","Principal":{"AWS":"222222222222"}}]}`
	state := &tf.InstanceState{
		ID: "arn:aws:sns:us-east-1:111111111111:t",
		Attributes: map[string]string{
			"id": "arn:aws:sns:us-east-1:111111111111:t", "name": "t", "policy": polA, "region": "us-east-1",
		},
	}
	cfgMap := map[string]any{"name": "t", "policy": polB, "region": "us-east-1"}
	block := tr.CoreConfigSchema()
	stateVal, err := schema.JSONMapToStateValue(map[string]any{
		"id": state.ID, "name": "t", "policy": polA, "region": "us-east-1",
	}, block)
	must(err)
	rawConfig, err := schema.JSONMapToStateValue(cfgMap, block)
	must(err)
	state.RawPlan = stateVal
	state.RawConfig = rawConfig
	run := func(label string) {
		rc := tf.NewResourceConfigRaw(cfgMap)
		d, err := schema.InternalMap(tr.Schema).Diff(ctx, state, rc, tr.CustomizeDiff, meta, false)
		if err != nil {
			fmt.Printf("   %-45s diff error: %v\n", label, err)
			return
		}
		if d == nil || d.Empty() {
			fmt.Printf("   %-45s NO DIFF\n", label)
			return
		}
		keys := make([]string, 0, len(d.Attributes))
		for k := range d.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			ad := d.Attributes[k]
			parts = append(parts, fmt.Sprintf("%s:%.20q->%.20q", k, ad.Old, ad.New))
		}
		fmt.Printf("   %-45s DIFF %s\n", label, strings.Join(parts, " "))
	}
	ps := tr.Schema["policy"]
	fmt.Printf("   runtime policy schema: opt=%v comp=%v\n", ps.Optional, ps.Computed)
	run("change policy, runtime schema")
	saveOpt, saveComp := ps.Optional, ps.Computed
	ps.Optional, ps.Computed = true, true
	run("change policy, upstream flags restored")
	ps.Optional, ps.Computed = saveOpt, saveComp
}

func kb(b uint64) string { return fmt.Sprintf("%.0f", float64(b)/1024) }

// diffAttrsDeep compares the externally visible fields of two attribute
// schemas, recursing into Elem, and describes the differences. The func-typed
// fields (validators, diff suppression) cannot be compared and are ignored.
func diffAttrsDeep(ms, ls *schema.Schema, prefix string) []string {
	var d []string
	if ms.Type != ls.Type {
		d = append(d, fmt.Sprintf("%stype diff=%v apply=%v", prefix, ms.Type, ls.Type))
	}
	if ms.Required != ls.Required {
		d = append(d, fmt.Sprintf("%sreq diff=%v apply=%v", prefix, ms.Required, ls.Required))
	}
	if ms.Optional != ls.Optional {
		d = append(d, fmt.Sprintf("%sopt diff=%v apply=%v", prefix, ms.Optional, ls.Optional))
	}
	if ms.Computed != ls.Computed {
		d = append(d, fmt.Sprintf("%scomp diff=%v apply=%v", prefix, ms.Computed, ls.Computed))
	}
	if ms.Sensitive != ls.Sensitive {
		d = append(d, fmt.Sprintf("%ssens diff=%v apply=%v", prefix, ms.Sensitive, ls.Sensitive))
	}
	if ms.ForceNew != ls.ForceNew {
		d = append(d, fmt.Sprintf("%sforcenew diff=%v apply=%v", prefix, ms.ForceNew, ls.ForceNew))
	}
	if fmt.Sprintf("%v", ms.Default) != fmt.Sprintf("%v", ls.Default) {
		d = append(d, fmt.Sprintf("%sdefault diff=%v apply=%v", prefix, ms.Default, ls.Default))
	}
	if ms.MaxItems != ls.MaxItems || ms.MinItems != ls.MinItems {
		d = append(d, fmt.Sprintf("%sitems diff={%d,%d} apply={%d,%d}", prefix, ms.MinItems, ms.MaxItems, ls.MinItems, ls.MaxItems))
	}
	mr, mrOK := ms.Elem.(*schema.Resource)
	lr, lrOK := ls.Elem.(*schema.Resource)
	me, meOK := ms.Elem.(*schema.Schema)
	le, leOK := ls.Elem.(*schema.Schema)
	switch {
	case mrOK != lrOK || meOK != leOK:
		d = append(d, fmt.Sprintf("%selem kind diff=%T apply=%T", prefix, ms.Elem, ls.Elem))
	case meOK && leOK && me != le:
		d = append(d, diffAttrsDeep(me, le, prefix+"elem.")...)
	case mrOK && lrOK && mr != lr:
		for k, mv := range mr.Schema {
			lv, ok := lr.Schema[k]
			if !ok {
				d = append(d, fmt.Sprintf("%s%s only in diff schema", prefix, k))
				continue
			}
			if mv == lv {
				continue
			}
			d = append(d, diffAttrsDeep(mv, lv, prefix+k+".")...)
		}
		for k := range lr.Schema {
			if _, ok := mr.Schema[k]; !ok {
				d = append(d, fmt.Sprintf("%s%s deleted from diff schema but live", prefix, k))
			}
		}
	}
	return d
}

// countSchemaFuncCalls wraps SchemaFunc with a counter and drives the schema
// operations a single Connect+Observe pair performs, excluding the AWS Read
// itself, which needs live credentials.
func countSchemaFuncCalls(pc *config.Provider) {
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil || r.TerraformResource.SchemaFunc == nil {
			continue
		}
		tr := r.TerraformResource
		orig := tr.SchemaFunc
		n := 0
		tr.SchemaFunc = func() map[string]*schema.Schema { n++; return orig() }

		start := time.Now()
		// Connect: getExtendedParameters tags_all probe, then schemaBlock.
		_ = tr.CoreConfigSchema()
		block := tr.CoreConfigSchema()
		// Connect, cold cache: rebuild the instance state from the observation.
		empty, err := schema.JSONMapToStateValue(map[string]any{}, block)
		if err == nil {
			_, _ = tr.ShimInstanceStateFromValue(empty)
		}
		// Observe: getResourceDataDiff applies the diff to an empty value.
		_ = tr.CoreConfigSchema()
		d := time.Since(start)

		tr.SchemaFunc = orig
		fmt.Printf("   %-30s %2d SchemaFunc calls, %8s (AWS Read excluded)\n", name, n, d.Round(time.Microsecond))
	}
}

func measureFrameworkProvider(ctx context.Context, sdk *schema.Provider) *xpprovider.AWSClient {
	// This is what internal/clients/aws.go does on every Connect: take the
	// service packages off the singleton provider's meta and build a client.
	meta, ok := sdk.Meta().(*xpprovider.AWSClient)
	if !ok {
		fmt.Println("   singleton provider meta is not an *AWSClient; skipping")
		return nil
	}
	pkgs := meta.GetServicePackages()
	fmt.Printf("   service packages on the singleton meta : %d\n", len(pkgs))

	cfg := xpprovider.AWSConfig{
		AccessKey:               "AKIAIOSFODNN7EXAMPLE",
		SecretKey:               "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:                  "us-east-1",
		Endpoints:               map[string]string{},
		SkipCredsValidation:     true,
		SkipRegionValidation:    true,
		SkipRequestingAccountId: true,
	}
	client := &xpprovider.AWSClient{}
	client.SetServicePackagesField(pkgs)
	built, diags := cfg.GetClient(ctx, client)
	if diags.HasError() {
		fmt.Printf("   could not construct a TF AWS client here (%v)\n", diags)
		return nil
	}

	d, a := allocs(20, func() {
		_ = xpprovider.GetFrameworkProviderWithMeta(&metaOnly{built})
	})
	fmt.Printf("   GetFrameworkProviderWithMeta : %10s, %8s KB per call\n", d.Round(time.Microsecond), kb(a))

	d2, a2 := allocs(10, func() {
		c := &xpprovider.AWSClient{}
		c.SetServicePackagesField(pkgs)
		_, _ = cfg.GetClient(ctx, c)
	})
	fmt.Printf("   AWSConfig.GetClient          : %10s, %8s KB per call\n", d2.Round(time.Microsecond), kb(a2))
	fmt.Printf("   combined, per reconcile      : %10s, %8s KB\n",
		(d + d2).Round(time.Microsecond), kb(a+a2))
	return built
}

// measureFrameworkConnectPath measures the work
// TerraformPluginFrameworkConnector.Connect repeats on every reconcile of a
// framework-based resource, on top of what SelectTerraformSetup already did:
// rebuilding the resource schema (getResourceSchema) and constructing plus
// configuring a protocol-v6 provider server (configureProvider).
func measureFrameworkConnectPath(ctx context.Context, pc *config.Provider, tfClient *xpprovider.AWSClient) {
	fwProbes := []string{
		"aws_eks_pod_identity_association",
		"aws_vpc_security_group_ingress_rule",
		"aws_appconfig_environment",
	}
	fw := xpprovider.GetFrameworkProviderWithMeta(&metaOnly{tfClient})

	var schemaResp fwprovider.SchemaResponse
	fw.Schema(ctx, fwprovider.SchemaRequest{}, &schemaResp)
	provType := schemaResp.Schema.Type().TerraformType(ctx)
	cfgJSON, err := json.Marshal(map[string]any{"region": "us-east-1"})
	must(err)
	cfgVal, err := tftypes.ValueFromJSONWithOpts(cfgJSON, provType, tftypes.ValueFromJSONOpts{IgnoreUndefinedAttributes: true})
	must(err)
	dv, err := tfprotov6.NewDynamicValue(provType, cfgVal)
	must(err)

	d, a := allocs(20, func() {
		// what connector.configureProvider does per Connect
		var sr fwprovider.SchemaResponse
		fw.Schema(ctx, fwprovider.SchemaRequest{}, &sr)
		srv := providerserver.NewProtocol6(fw)()
		_, _ = srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{
			TerraformVersion: "crossTF000",
			Config:           &dv,
		})
	})
	fmt.Printf("   configureProvider (schema + NewProtocol6 + ConfigureProvider) : %10s, %8s KB per Connect\n", d.Round(time.Microsecond), kb(a))

	for _, name := range fwProbes {
		r := pc.Resources[name]
		if r == nil || r.TerraformPluginFrameworkResource == nil {
			fmt.Printf("   %-40s not a framework resource here; skipping\n", name)
			continue
		}
		res := r.TerraformPluginFrameworkResource
		ds, as := allocs(50, func() {
			resp := &fwresource.SchemaResponse{}
			res.Schema(ctx, fwresource.SchemaRequest{}, resp)
			_ = resp.Schema.Type().TerraformType(ctx)
		})
		fmt.Printf("   %-40s getResourceSchema: %10s, %8s KB per Connect\n", name, ds.Round(time.Microsecond), kb(as))
	}
}

type metaOnly struct{ m any }

func (m *metaOnly) Meta() any { return m.m }

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var _ = config.Provider{}
