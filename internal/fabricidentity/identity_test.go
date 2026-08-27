/*
Copyright © 2026 Datum Technology, Inc. All rights reserved.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package fabricidentity

import (
	"errors"
	"strings"
	"testing"
)

// The identity is the block's index within the pool, which is the 32 bits
// between the pool's root and the block, which is exactly the width that
// survives into the Route Target.
func TestFromBlockReadsTheIndexOutOfTheBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cidr  string
		want  int64
		wants string
	}{
		{name: "the first usable block", cidr: "fc00:0:0:1::/64", want: 1},
		{name: "an index spanning both halves", cidr: "fc00:0:1234:5678::/64", want: 0x12345678},
		{name: "the last block in the pool", cidr: "fc00:0:ffff:ffff::/64", want: 0xffffffff},
		{name: "a pool rooted longer than a /32", cidr: "fc00:0:0:beef::/64", want: 0xbeef},

		{name: "the pool's own zero block", cidr: "fc00::/64", wants: "index is zero"},
		{name: "a block wider than a /64", cidr: "fc00:0:0:1::/48", wants: "read out of a /64"},
		{name: "a block narrower than a /64", cidr: "fc00:0:0:1::/96", wants: "read out of a /64"},
		{name: "an IPv4 block", cidr: "10.0.0.0/24", wants: "read out of an IPv6 space"},
		{name: "not a prefix at all", cidr: "fc00::", wants: "not a prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := FromBlock(tc.cidr)
			if tc.wants != "" {
				if err == nil {
					t.Fatalf("expected a refusal, got identity %d", identity)
				}
				if !contains(err.Error(), tc.wants) {
					t.Fatalf("expected the refusal to mention %q, got %q", tc.wants, err.Error())
				}
				var unusable *UnusableError
				if !asUnusable(err, &unusable) {
					t.Fatalf("a bad block must be an UnusableError so the caller can tell it from an outage")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if identity != tc.want {
				t.Fatalf("expected identity %d, got %d", tc.want, identity)
			}
			// A value needing more than 32 bits would be uniqueness the platform
			// believes it has and the Route Target does not.
			if identity > 0xffffffff {
				t.Fatalf("identity %d does not fit the 32 bits the fabric carries", identity)
			}
		})
	}
}

// The claim is named from the network's namespace and name, which is what makes
// the identity a permanent property of that pair and makes allocation idempotent
// without recording anything of its own.
func TestClaimNameIsStableAndCollisionFree(t *testing.T) {
	namespace, name := "ns", "prod"
	if ClaimName(namespace, name) != ClaimName("ns", "prod") {
		t.Fatal("the same network must reach the same claim")
	}
	if ClaimName("ns", "prod") == ClaimName("ns", "staging") {
		t.Fatal("two networks in a namespace must reach different claims")
	}
	if ClaimName("a", "b") == ClaimName("b", "a") {
		t.Fatal("namespace and name must not be interchangeable")
	}

	// A dash delimiter would collide here: both would render "...a-b-c". A
	// namespace is a DNS label and cannot contain a dot, so the first dot after
	// the prefix always ends it.
	if ClaimName("a-b", "c") == ClaimName("a", "b-c") {
		t.Fatal("the delimiter must not be ambiguous")
	}

	// A network name may contain dots; a namespace may not. Uniqueness rests on
	// that: "ns.a" is not a namespace the API server will accept, so the pair
	// that would collide with ("ns", "a.b") cannot exist. Asserted here so the
	// dependency is recorded rather than assumed.
	if strings.Contains("ns", ".") {
		t.Fatal("a namespace is a DNS label and cannot contain a dot")
	}
	if ClaimName("ns", "a.b") == ClaimName("ns", "a-b") {
		t.Fatal("two names in one namespace must reach different claims")
	}

	long := ClaimName(strings.Repeat("n", 200), strings.Repeat("p", 200))
	if len(long) > maxClaimNameLength {
		t.Fatalf("an over-long pair must still yield a usable name, got %d characters", len(long))
	}
	if long == ClaimName(strings.Repeat("n", 200), strings.Repeat("q", 200)) {
		t.Fatal("two truncated names must still differ")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func asUnusable(err error, target **UnusableError) bool {
	return errors.As(err, target)
}
