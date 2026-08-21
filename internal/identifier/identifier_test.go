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

package identifier

import "testing"

func TestHexRejectsReservedValues(t *testing.T) {
	for _, value := range []uint64{0, MaxVPC, MaxVPC + 1} {
		if _, err := Hex(value, MaxVPC); err == nil {
			t.Errorf("Hex(%d): got no error, want one", value)
		}
	}
}

func TestRandomIdentifiersFitTheirInterfaceNameSegment(t *testing.T) {
	for range 200 {
		vpc, err := RandomVPCBase62()
		if err != nil {
			t.Fatalf("RandomVPCBase62: %v", err)
		}
		if len(vpc) > 9 {
			t.Errorf("VPC identifier %q exceeds nine base62 characters", vpc)
		}

		attachment, err := RandomVPCAttachmentBase62()
		if err != nil {
			t.Fatalf("RandomVPCAttachmentBase62: %v", err)
		}
		if len(attachment) > 3 {
			t.Errorf("attachment identifier %q exceeds three base62 characters", attachment)
		}
	}
}

func TestBase62RoundTrip(t *testing.T) {
	hex, err := RandomVPC()
	if err != nil {
		t.Fatalf("RandomVPC: %v", err)
	}
	base62, err := HexToBase62(hex)
	if err != nil {
		t.Fatalf("HexToBase62(%q): %v", hex, err)
	}
	back, err := Base62ToHex(base62)
	if err != nil {
		t.Fatalf("Base62ToHex(%q): %v", base62, err)
	}
	if want := trimLeadingZeros(hex); back != want {
		t.Errorf("round trip: got %q, want %q", back, want)
	}
}

func trimLeadingZeros(value string) string {
	for i := range value {
		if value[i] != '0' {
			return value[i:]
		}
	}
	return "0"
}
