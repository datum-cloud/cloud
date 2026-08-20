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

// Command vpc-controller reconciles VPC and VPCAttachment in a POP cell,
// alongside network-services-operator, compute and the workload providers.
package main

import (
	"flag"
	"os"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/controller"
	"go.datum.net/cloud/internal/galactic"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cloudv1alpha1.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))
	utilruntime.Must(bgpv1alpha1.AddToScheme(scheme))
	utilruntime.Must(nadv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. A single writer is what makes identifier allocation safe.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vpc-controller.cloud.datumapis.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &cloudv1alpha1.VPCAttachment{},
		controller.IndexVPCAttachmentIdentity, func(obj client.Object) []string {
			attachment, ok := obj.(*cloudv1alpha1.VPCAttachment)
			if !ok || attachment.Status.VPC == "" || attachment.Status.VPCAttachment == "" {
				return nil
			}
			return []string{galactic.AdvertisementName(attachment.Status.VPC, attachment.Status.VPCAttachment)}
		}); err != nil {
		setupLog.Error(err, "unable to index VPC attachments by identity")
		os.Exit(1)
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &cloudv1alpha1.VPCAttachment{},
		controller.IndexVPCAttachmentInterface, func(obj client.Object) []string {
			attachment, ok := obj.(*cloudv1alpha1.VPCAttachment)
			if !ok || attachment.Spec.InterfaceRef == nil {
				return nil
			}
			return []string{attachment.Spec.InterfaceRef.Name}
		}); err != nil {
		setupLog.Error(err, "unable to index VPC attachments by interface")
		os.Exit(1)
	}

	if err := (&controller.NetworkContextReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkContext")
		os.Exit(1)
	}
	if err := (&controller.VPCAttachmentReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VPCAttachment")
		os.Exit(1)
	}
	if err := (&controller.BGPAdvertisementReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BGPAdvertisement")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
