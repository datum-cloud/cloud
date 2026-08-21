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
	"errors"
	"flag"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/controller"
	"go.datum.net/cloud/internal/galactic"
	datumwebhook "go.datum.net/cloud/internal/webhook"
	computev1alpha "go.datum.net/compute/api/v1alpha"
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
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, rawAttachmentMode, webhookCertDir string
	var webhookPort int
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. A single writer is what makes identifier allocation safe.")
	flag.StringVar(&rawAttachmentMode, "attachment-mode", "",
		"Required. How guests in this cell consume an interface: Netns or Hypervisor.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the admission webhook server binds to.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory holding the webhook server's tls.crt and tls.key.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	attachmentMode, err := parseAttachmentMode(rawAttachmentMode)
	if err != nil {
		setupLog.Error(err, "invalid configuration")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vpc-controller.cloud.datumapis.com",
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
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

	if err := (&controller.NetworkContextReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkContext")
		os.Exit(1)
	}
	if err := (&controller.NetworkInterfaceReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
		AttachmentMode: attachmentMode,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkInterface")
		os.Exit(1)
	}
	if err := (&controller.BGPAdvertisementReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BGPAdvertisement")
		os.Exit(1)
	}

	(&datumwebhook.PodInterfaceInjector{Client: mgr.GetClient()}).SetupWithManager(mgr)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Gate readiness on the webhook server: with failurePolicy Fail, a Pod that
	// reports Ready before the webhook serves would take endpoints and reject
	// every labelled Pod in the cell.
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// parseAttachmentMode resolves the required attachment mode. There is no
// default: defaulting to Netns would hand a microVM a veth it cannot use.
func parseAttachmentMode(value string) (cloudv1alpha1.VPCAttachmentInterfaceMode, error) {
	switch cloudv1alpha1.VPCAttachmentInterfaceMode(value) {
	case cloudv1alpha1.VPCAttachmentInterfaceModeNetns:
		return cloudv1alpha1.VPCAttachmentInterfaceModeNetns, nil
	case cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor:
		return cloudv1alpha1.VPCAttachmentInterfaceModeHypervisor, nil
	case "":
		return "", errors.New("--attachment-mode is required: set Netns for container cells or Hypervisor for microVM cells")
	default:
		return "", fmt.Errorf("--attachment-mode %q is not one of Netns, Hypervisor", value)
	}
}
