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

package main

import (
	"testing"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
)

func TestParseAttachmentMode(t *testing.T) {
	tests := []struct {
		input   string
		want    cloudv1alpha1.VPCAttachmentInterfaceMode
		wantErr bool
	}{
		{"Hypervisor", cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor, false},
		{"Netns", cloudv1alpha1.VPCAttachmentInterfaceModeNetns, false},
		{"", "", true},
		{"netns", "", true},
		{"tap", "", true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseAttachmentMode(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("error: got %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}
