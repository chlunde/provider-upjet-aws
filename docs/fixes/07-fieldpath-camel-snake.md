<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 07. camel→snake conversion mangles nested and digit-bearing field paths

| | |
| --- | --- |
| **Category** | corruption |
| **Severity** | high, but **latent** — no user-reachable path demonstrated |
| **Size** | medium |
| **Lives in** | upjet — `pkg/controller/annotation_conversions.go:81,207` |
| **Evidence** | measured |

## What happens

The conversion applies a camel→snake transform to a **whole field path** rather
than to each segment, so every separator is corrupted:

| input | produced | expected |
| --- | --- | --- |
| `fooBar.bazQux` | `foo_bar_._baz_qux` | `foo_bar.baz_qux` |
| `fooBar[0].bazQux` | `foo_bar_[_0_]._baz_qux` | `foo_bar[0].baz_qux` |
| `ipv6_addresses` | `ipv_6_addresses` | `ipv6_addresses` |

The machinery works only for single-segment names with no digits. Anything
nested, indexed, or containing a number is silently rewritten to a path that
matches nothing.

## Why "latent"

The defect in the transform is measured and certain. What is *not* established
is that a configured path reaching this code today is nested or digit-bearing —
that depends on which resources use the annotation-conversion feature and with
what paths. A path that matches nothing fails quietly, which is why nobody has
noticed either way.

**Decide severity by reproducing first.** Enumerate the field paths actually
passed through this code across `config/`; if any are nested or contain digits,
this is a release-worthy corruption bug. If none are, it is a latent trap worth
fixing routinely.

## The fix

Split the path into segments, convert each segment, and rejoin — preserving
`.` separators and `[n]` index suffixes. `upjet/pkg/types/name` already has
the segment-level conversion; the bug is applying it to the joined string.

Use `fieldpath.Parse` rather than string splitting, so indices and quoted
segments are handled by the same parser the rest of the stack uses.

## How to test

* **Unit (upjet), table-driven, from the table above** — plus quoted segments
  and a path that is already snake_case (must be idempotent). Fails today.
* **Repo sweep:** enumerate every field path passed to the annotation
  conversions across `config/` and assert each round-trips unchanged. This
  doubles as the reproduction that settles the severity question.

## Suggested issue

Repo: `crossplane/upjet`

**Title:** `camel→snake conversion is applied to whole field paths, corrupting nested and digit-bearing paths`

**Body:**

> `pkg/controller/annotation_conversions.go:81,207` converts an entire field
> path with a segment-level camel→snake transform. The separators are converted
> along with the names:
>
> ```
> fooBar.bazQux     -> foo_bar_._baz_qux      (want foo_bar.baz_qux)
> fooBar[0].bazQux  -> foo_bar_[_0_]._baz_qux (want foo_bar[0].baz_qux)
> ipv6_addresses    -> ipv_6_addresses        (want ipv6_addresses)
> ```
>
> Measured directly against the current implementation. The transform is only
> correct for single-segment names containing no digits; anything else produces
> a path that matches nothing, and does so silently.
>
> Suggested fix: parse the path (`fieldpath.Parse`), convert each segment, and
> rejoin, preserving `.` separators and `[n]` indices.

## Branch

`fix/fieldpath-segmentwise-camel-snake` (upjet fork)
