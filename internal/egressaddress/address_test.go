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

package egressaddress

import (
	"errors"
	"strings"
	"testing"
)

// A claim name collision is two shards sharing one address, so the split
// between namespace and name has to be unambiguous. A dash would not be: the
// two cases below would produce the same name.
func TestClaimNameCannotCollideAcrossTheNamespaceBoundary(t *testing.T) {
	first := ClaimName("a-b", "c")
	second := ClaimName("a", "b-c")
	if first == second {
		t.Fatalf("two different shards share the claim name %q", first)
	}
}

func TestClaimNameNamesTheShard(t *testing.T) {
	got := ClaimName("galactic-system", "worker-8b4e1647-dfw")
	if !strings.Contains(got, "worker-8b4e1647-dfw") {
		t.Errorf("claim name %q does not name the shard it is held for", got)
	}
	if !strings.HasPrefix(got, claimNamePrefix) {
		t.Errorf("claim name %q is not recognisable as an egress shard address claim", got)
	}
}

// A shard is named after its node, and the name it produces still has to be a
// name the API server will accept.
func TestALongShardNameStillYieldsAValidClaimName(t *testing.T) {
	long := strings.Repeat("n", 300)
	got := ClaimName("galactic-system", long)
	if len(got) > maxClaimNameLength {
		t.Fatalf("claim name is %d characters, over the %d the API server allows", len(got), maxClaimNameLength)
	}
	// Two long names that share a prefix must not truncate to one claim.
	other := ClaimName("galactic-system", long+"x")
	if got == other {
		t.Fatal("two shards with long names share one claim name")
	}
}

func TestFromAllocatedCIDRReadsTheAddress(t *testing.T) {
	got, err := FromAllocatedCIDR("2607:ed40:70::1/128")
	if err != nil {
		t.Fatalf("read the address: %v", err)
	}
	if got.String() != "2607:ed40:70::1" {
		t.Errorf("address = %q, want the host address without its prefix length", got.String())
	}
}

// Every one of these is an answer the service gave that cannot be used as a
// shard address. Reading one anyway would write it into a field that cannot be
// corrected.
func TestFromAllocatedCIDRRefusesWhatIsNotAShardAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		cidr string
	}{
		{"not a prefix", "2607:ed40:70::1"},
		{"empty", ""},
		{"a block rather than an address", "2607:ed40:70::/64"},
		{"the wrong family", "198.51.100.7/32"},
		{"an IPv4 address in IPv6 clothing", "::ffff:198.51.100.7/128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromAllocatedCIDR(tc.cidr)
			if err == nil {
				t.Fatalf("%q was accepted as a shard address (%s)", tc.cidr, got)
			}
			var unusable *UnusableError
			if !errors.As(err, &unusable) {
				t.Errorf("error = %v; an answer that cannot be used is a wait on an operator, not a retry", err)
			}
		})
	}
}
