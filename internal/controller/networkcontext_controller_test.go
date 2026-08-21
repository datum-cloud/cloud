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

package controller

import (
	"testing"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// A location's Subnet arrives by propagation, which carries spec and never
// status, so the allocated range has to be readable from either.
func TestSubnetRangePrefersStatusAndFallsBackToSpec(t *testing.T) {
	start := "fd00::"
	var length int32 = 48

	withStatus := &networkingv1alpha.Subnet{
		Spec:   networkingv1alpha.SubnetSpec{StartAddress: "fd20::", PrefixLength: 64},
		Status: networkingv1alpha.SubnetStatus{StartAddress: &start, PrefixLength: &length},
	}
	gotStart, gotLength, ok := subnetRange(withStatus)
	if !ok || gotStart != "fd00::" || gotLength != 48 {
		t.Fatalf("status should win when present, got %q/%d ok=%v", gotStart, gotLength, ok)
	}

	specOnly := &networkingv1alpha.Subnet{
		Spec: networkingv1alpha.SubnetSpec{StartAddress: "fd20::", PrefixLength: 64},
	}
	gotStart, gotLength, ok = subnetRange(specOnly)
	if !ok || gotStart != "fd20::" || gotLength != 64 {
		t.Fatalf("a propagated copy carries spec only, got %q/%d ok=%v", gotStart, gotLength, ok)
	}

	if _, _, ok := subnetRange(&networkingv1alpha.Subnet{}); ok {
		t.Fatal("an unallocated subnet should yield nothing")
	}
}
