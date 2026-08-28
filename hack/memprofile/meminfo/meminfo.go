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
	"time"
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

// SinceExec returns how long ago the process was exec'd, which includes the
// runtime and package init work that happens before main. Returns 0 if it
// cannot be determined.
func SinceExec() time.Duration {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	// Field 22 is starttime, in clock ticks since boot. The comm field at
	// index 1 can contain spaces, so index from the closing parenthesis.
	i := strings.LastIndex(string(stat), ")")
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(stat)[i+1:])
	// f[0] is state, which is field 3, so starttime is at f[22-3].
	if len(f) < 20 {
		return 0
	}
	ticks, err := strconv.ParseFloat(f[19], 64)
	if err != nil {
		return 0
	}
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	uf := strings.Fields(string(up))
	if len(uf) == 0 {
		return 0
	}
	uptime, err := strconv.ParseFloat(uf[0], 64)
	if err != nil {
		return 0
	}
	// USER_HZ is 100 on every Linux platform Go builds for.
	return time.Duration((uptime - ticks/100) * float64(time.Second))
}

// SmapsFields returns the /proc/self/smaps_rollup counters, in MiB, keyed by
// the field name without its colon ("Rss", "Pss", "Private_Clean",
// "Private_Dirty", "Anonymous", ...). Smaps() formats the same data for a
// human; this returns it for a time series.
func SmapsFields() map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !strings.HasSuffix(f[0], ":") || f[2] != "kB" {
			continue
		}
		kb, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(f[0], ":")] = kb / 1024
	}
	return out
}

// Anonymous returns the smaps_rollup Anonymous counter in MiB: the process's
// anonymous (heap and stack) resident pages, which is the part of a pod's
// working set that scavenging can actually return.
func Anonymous() float64 { return SmapsFields()["Anonymous"] }
