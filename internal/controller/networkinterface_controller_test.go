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
		{cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared, galactic.PluginTap},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			if got := masterPlugin(test.mode); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
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

// The cell-wide mode is the only signal every attachment written until now
// carries, so it has to keep deciding for all of them. An interface that
// states its own mode is the exception.
func TestAttachmentModeFallsBackToTheCell(t *testing.T) {
	r := &NetworkInterfaceReconciler{
		AttachmentMode: cloudv1alpha1.VPCAttachmentInterfaceModeNetns,
	}

	tests := []struct {
		name  string
		iface networkingv1alpha.NetworkInterfaceAttachmentMode
		want  cloudv1alpha1.VPCAttachmentInterfaceMode
	}{
		{"unset", "", cloudv1alpha1.VPCAttachmentInterfaceModeNetns},
		{"netns", networkingv1alpha.NetworkInterfaceAttachmentModeNetns,
			cloudv1alpha1.VPCAttachmentInterfaceModeNetns},
		{"hypervisor", networkingv1alpha.NetworkInterfaceAttachmentModeHypervisor,
			cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor},
		{"declared", networkingv1alpha.NetworkInterfaceAttachmentModeHypervisorDeclared,
			cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			networkInterface := &networkingv1alpha.NetworkInterface{
				Spec: networkingv1alpha.NetworkInterfaceSpec{AttachmentMode: test.iface},
			}
			if got := r.attachmentMode(networkInterface); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// Only the declared mode asks the tap plugin to describe the device.
func TestDeclaresDevice(t *testing.T) {
	if declaresDevice(cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor) {
		t.Error("a discovered hypervisor attachment must not ask for a description")
	}
	if !declaresDevice(cloudv1alpha1.VPCAttachmentInterfaceModeHypervisorDeclared) {
		t.Error("a declared hypervisor attachment must ask for a description")
	}
}
