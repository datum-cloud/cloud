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

// Command egress-address-controller gives each egress shard in a cell the
// public IPv6 address it translates to.
//
// It runs in the cell, unlike fabric-identity-controller, which allocates
// centrally because a network spans locations and its identity must be the same
// in all of them. A shard is the opposite case: it names the Node it executes
// on, so it exists only where that Node does, and nothing about its address has
// to agree with any other location.
//
// It is a binary of its own rather than a reconciler inside vpc-controller
// because it needs a credential vpc-controller does not have. vpc-controller
// writes the attachment state of every workload in the cell and serves an
// admission webhook; giving that pod a credential into the platform's own
// tenancy widens the blast radius of the one component the cell cannot run
// without, and a missing or expired address credential would stop workloads
// attaching. Split out, an address that cannot be claimed costs new shards
// their addresses and costs nothing else.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.datum.net/cloud/internal/controller"
	"go.datum.net/cloud/internal/ipam"
	bgpv1alpha1 "go.datum.net/network/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bgpv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var addressClass, claimNamespace, location, platformProject, ipamKubeconfig string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. A single writer is what keeps one shard to one address.")
	flag.StringVar(&addressClass, "address-class-ipv6", "",
		"Required. The IPClass that hands out shard addresses. It draws from announceable public space shared by every shard in a location.")
	flag.StringVar(&claimNamespace, "claim-namespace", "default",
		"Namespace in the platform's own tenancy that address claims are written to.")
	flag.StringVar(&location, "location", "",
		"Required. The location this cell serves. It selects the shared public range addresses come from; two cells serving one location draw from the same range.")
	flag.StringVar(&platformProject, "platform-project", "",
		"Required. The project control plane the platform allocates its own values in. A shard's address is not a consumer's address and must not be drawn from any one consumer's space or counted against their quota.")
	flag.StringVar(&ipamKubeconfig, "ipam-kubeconfig", "",
		"Required. Path to a kubeconfig for the cluster serving the IPAM API.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// Every one of these is fatal at startup rather than per shard. A shard's
	// address cannot be corrected once written, so a deployment that would draw
	// from the wrong space, or from no space, must not reach a single shard.
	switch {
	case addressClass == "":
		setupLog.Error(nil, "-address-class-ipv6 is required")
		os.Exit(1)
	case location == "":
		// A claim carrying the wrong location is the dangerous case, not the
		// missing one: it succeeds, and hands this cell an address that another
		// location's fabric attracts.
		setupLog.Error(nil, "-location is required")
		os.Exit(1)
	case platformProject == "":
		setupLog.Error(nil, "-platform-project is required")
		os.Exit(1)
	case ipamKubeconfig == "":
		setupLog.Error(nil, "-ipam-kubeconfig is required")
		os.Exit(1)
	}

	// The manager runs against the cell this is scheduled on, which is also
	// where the shards are. There is no second cluster: an EgressShard names a
	// Node, so it is never anywhere but the cell holding that Node.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "egress-address-controller.cloud.datumapis.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ipamRestConfig, err := clientcmd.BuildConfigFromFlags("", ipamKubeconfig)
	if err != nil {
		setupLog.Error(err, "unable to load the IPAM kubeconfig")
		os.Exit(1)
	}

	ipamScheme, err := ipam.Scheme()
	if err != nil {
		setupLog.Error(err, "unable to build the IPAM scheme")
		os.Exit(1)
	}

	ipamClients, err := ipam.NewClientFactory(ipamRestConfig, ipamScheme, platformProject)
	if err != nil {
		setupLog.Error(err, "unable to build the IPAM client factory")
		os.Exit(1)
	}

	if err := (&controller.EgressShardAddressReconciler{
		Client:           mgr.GetClient(),
		IPAM:             ipamClients,
		AddressClassIPv6: addressClass,
		ClaimNamespace:   claimNamespace,
		Location:         location,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EgressShardAddress")
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

	setupLog.Info("starting egress address controller", "location", location)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
