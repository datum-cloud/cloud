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

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/galactic"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

func TestMasterPlugin(t *testing.T) {
	tests := []struct {
		mode cloudv1alpha1.VPCAttachmentInterfaceMode
		want string
	}{
		{cloudv1alpha1.VPCAttachmentInterfaceModeNetns, galactic.PluginVeth},
		{cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor, galactic.PluginTap},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			if got := masterPlugin(test.mode); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestConsumerAnnotations(t *testing.T) {
	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-eth0", Namespace: "my-project"},
	}
	got := consumerAnnotations(nad)
	want := "my-project/web-0-eth0"
	if got[MultusNetworksAnnotation] != want {
		t.Errorf("%s: got %q, want %q", MultusNetworksAnnotation, got[MultusNetworksAnnotation], want)
	}
	if len(got) != 1 {
		t.Errorf("annotation count: got %d, want 1", len(got))
	}
}

func TestClaimFulfilled(t *testing.T) {
	newInterface := func(phase networkingv1alpha.NetworkInterfacePhase, allocated metav1.ConditionStatus,
		context *networkingv1alpha.LocalNetworkContextRef) *networkingv1alpha.NetworkInterface {
		return &networkingv1alpha.NetworkInterface{
			Status: networkingv1alpha.NetworkInterfaceStatus{
				Phase:             phase,
				NetworkContextRef: context,
				Conditions: []metav1.Condition{{
					Type:   networkingv1alpha.NetworkInterfaceAllocated,
					Status: allocated,
					Reason: "Test",
				}},
			},
		}
	}
	context := &networkingv1alpha.LocalNetworkContextRef{Name: "default-us-central-1"}

	tests := []struct {
		name             string
		networkInterface *networkingv1alpha.NetworkInterface
		want             bool
	}{
		{"bound and allocated", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionTrue, context), true},
		{"available", newInterface(
			networkingv1alpha.NetworkInterfacePhaseAvailable, metav1.ConditionTrue, context), false},
		{"not allocated", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionFalse, context), false},
		{"no network context", newInterface(
			networkingv1alpha.NetworkInterfacePhaseBound, metav1.ConditionTrue, nil), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claimFulfilled(test.networkInterface); got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}
