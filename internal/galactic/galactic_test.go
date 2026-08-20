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

package galactic

import (
	"encoding/json"
	"testing"
)

func TestConflistChainIsComplete(t *testing.T) {
	conflist := Conflist("web-eth0", PluginTap, "0000000jU", "01a", 1400,
		[]string{"fd00:10:ff01:0:1::1/96", "172.20.1.7/32"})

	if conflist.CNIVersion != "1.0.0" {
		t.Errorf("cniVersion: got %q, want %q", conflist.CNIVersion, "1.0.0")
	}
	if len(conflist.Plugins) != 2 {
		t.Fatalf("plugin count: got %d, want 2", len(conflist.Plugins))
	}

	master, ok := conflist.Plugins[0].(MasterPlugin)
	if !ok {
		t.Fatalf("first plugin: got %T, want MasterPlugin", conflist.Plugins[0])
	}
	if master.Type != PluginTap {
		t.Errorf("master plugin: got %q, want %q", master.Type, PluginTap)
	}
	if master.IPAM == nil || len(master.IPAM.Addresses) != 2 {
		t.Fatalf("ipam addresses: got %v, want two entries", master.IPAM)
	}

	// The master plugin fails ADD before creating kernel state without this.
	bgp, ok := conflist.Plugins[1].(BGPPlugin)
	if !ok {
		t.Fatalf("second plugin: got %T, want BGPPlugin", conflist.Plugins[1])
	}
	if bgp.Type != PluginBGP {
		t.Errorf("bgp plugin: got %q, want %q", bgp.Type, PluginBGP)
	}
}

func TestConflistOmitsIPAMForSelfAddressingGuest(t *testing.T) {
	raw, err := ConflistJSON("web-eth0", PluginTap, "0000000jU", "01a", 0, nil)
	if err != nil {
		t.Fatalf("ConflistJSON: %v", err)
	}

	var decoded struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal conflist: %v", err)
	}
	if _, present := decoded.Plugins[0]["ipam"]; present {
		t.Errorf("ipam block present for a guest managing its own addressing: %s", raw)
	}
	if _, present := decoded.Plugins[0]["mtu"]; present {
		t.Errorf("mtu emitted when unset: %s", raw)
	}
}

func TestInterfaceNames(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"host", HostInterfaceName("jU", "1a"), "G0000000jU01aH"},
		{"guest", GuestInterfaceName("jU", "1a"), "G0000000jU01aG"},
		{"vrf", VRFInterfaceName("jU"), "G0000000jUV"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Errorf("got %q, want %q", test.got, test.want)
			}
			if len(test.got) > 15 {
				t.Errorf("interface name %q exceeds the 15 character kernel limit", test.got)
			}
		})
	}
}

func TestSplitAdvertisementName(t *testing.T) {
	tests := []struct {
		input         string
		vpc           string
		vpcAttachment string
		ok            bool
	}{
		{"0000000jU-01a", "0000000jU", "01a", true},
		{"0000000jU", "", "", false},
		{"-01a", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			vpc, vpcAttachment, ok := SplitAdvertisementName(test.input)
			if vpc != test.vpc || vpcAttachment != test.vpcAttachment || ok != test.ok {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)",
					vpc, vpcAttachment, ok, test.vpc, test.vpcAttachment, test.ok)
			}
		})
	}
}
