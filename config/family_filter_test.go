// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reImperative finds a resource configurator that assigns ShortGroup directly,
// e.g.
//
//	p.AddResourceConfigurator("aws_lb_trust_store", func(r *config.Resource) {
//		r.ShortGroup = "elbv2"
//
// Those assignments run after the include lists are built, so a static family
// filter cannot see them.
var reImperative = regexp.MustCompile(`AddResourceConfigurator\("([a-z0-9_]+)",[^)]*\)[^{]*\{(?:[^}]|\}[^)])*?r\.ShortGroup\s*=\s*"([a-z0-9]+)"`)

// TestImperativeShortGroupsAreMirrored keeps imperativeShortGroups honest. A
// resource whose group is set inside a configurator is invisible to
// shortGroupOf, so UPJET_FAMILY_FILTER would drop it while the family's
// generated controllers still index config.Provider.Resources for it - a nil
// map entry, and a panic at provider startup.
func TestImperativeShortGroupsAreMirrored(t *testing.T) {
	found := map[string]string{}
	err := filepath.Walk("..", func(path string, _ os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(path, ".go") {
			return nil //nolint:nilerr // unreadable files are not this test's business
		}
		if !strings.Contains(path, "/config/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		for _, m := range reImperative.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = m[2]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the config tree: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("found no imperative ShortGroup assignments at all; the scan is broken, " +
			"which would let this test pass vacuously")
	}
	for name, group := range found {
		mirrored, ok := imperativeShortGroups[name]
		if !ok {
			t.Errorf("%s has its ShortGroup set imperatively to %q, but is not in "+
				"imperativeShortGroups; a family filter would drop it and the %s "+
				"controllers would panic at startup", name, group, group)
			continue
		}
		if mirrored != group {
			t.Errorf("%s is assigned ShortGroup %q imperatively but mirrored as %q",
				name, group, mirrored)
		}
	}
	for name := range imperativeShortGroups {
		if _, ok := found[name]; !ok {
			t.Errorf("%s is mirrored in imperativeShortGroups but no configurator "+
				"assigns its ShortGroup any more; remove it", name)
		}
	}
}
