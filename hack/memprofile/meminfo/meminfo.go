// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Package meminfo contains helpers shared by the memory profiling programs
// under hack/memprofile. See hack/memprofile/README.md.
package meminfo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStatusMiB returns the value of the given /proc/self/status field, in MiB.
func procStatusMiB(field string) float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(l, field+":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0
		}
		kb, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// RSS returns the current resident set size in MiB.
func RSS() float64 { return procStatusMiB("VmRSS") }

// PeakRSS returns the high-water resident set size in MiB.
func PeakRSS() float64 { return procStatusMiB("VmHWM") }

// Smaps returns a one-line summary of /proc/self/smaps_rollup. Anonymous is the
// heap and stacks; Private_Clean is executable text and rodata paged in from
// the binary itself.
func Smaps() string {
	b, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		return ""
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		for _, k := range []string{"Rss:", "Pss:", "Private_Clean:", "Private_Dirty:", "Anonymous:"} {
			if strings.HasPrefix(l, k) {
				out = append(out, strings.Join(strings.Fields(l), " "))
			}
		}
	}
	return strings.Join(out, " | ")
}

// ReportLinkCost prints the RSS of a program that has linked in a given slice
// of the provider but has not done any work yet.
func ReportLinkCost(stage string) {
	fmt.Printf("%-30s RSS=%7.1f MiB\n", stage, RSS())
}
