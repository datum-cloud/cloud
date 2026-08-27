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

// Command fabric-identity-controller allocates the identity the fabric knows a
// network by, and carries it to the cells where the network is required.
//
// It runs centrally rather than in a cell. A network spans locations, so the
// one thing that has to be the same in all of them cannot be decided in any one
// of them.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudv1alpha1 "go.datum.net/cloud/api/v1alpha1"
	"go.datum.net/cloud/internal/controller"
	"go.datum.net/cloud/internal/ipam"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cloudv1alpha1.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var identityClass, identityNamespace, platformProject, ipamKubeconfig, hubKubeconfig string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. A single writer is what keeps one network to one identity.")
	flag.StringVar(&identityClass, "identity-class", "",
		"Required. The IPClass that hands out fabric identities. It roots an identifier space that is never routed, and must not hand out its own first block.")
	flag.StringVar(&identityNamespace, "identity-namespace", "default",
		"Namespace in the platform's own tenancy that identity claims are written to.")
	flag.StringVar(&platformProject, "platform-project", "",
		"Required. The project control plane the platform allocates its own values in. A network's identity must be unique across every consumer, so it cannot be drawn from any one of them.")
	flag.StringVar(&ipamKubeconfig, "ipam-kubeconfig", "",
		"Required. Path to a kubeconfig for the cluster serving the IPAM API.")
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "",
		"Required. Path to a kubeconfig for the federation hub. Networks and their NetworkContexts are read there as copies published by the operator that owns them, and the identity and its placement are written there. Leader election is not: that stays on the cluster this runs on.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	switch {
	case identityClass == "":
		setupLog.Error(nil, "-identity-class is required")
		os.Exit(1)
	case platformProject == "":
		// A deployment naming an identifier space and nowhere platform-owned to
		// draw from would hand out identities unique only within one consumer,
		// which is not unique at all. Say so at startup rather than per network.
		setupLog.Error(nil, "-platform-project is required")
		os.Exit(1)
	case ipamKubeconfig == "":
		setupLog.Error(nil, "-ipam-kubeconfig is required")
		os.Exit(1)
	case hubKubeconfig == "":
		// Nothing this component does happens anywhere else, so there is no
		// degraded mode worth starting into.
		setupLog.Error(nil, "-hub-kubeconfig is required")
		os.Exit(1)
	}

	hubRestConfig, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		setupLog.Error(err, "unable to load the hub kubeconfig")
		os.Exit(1)
	}

	// The manager runs against the cluster this is scheduled on, and reaches the
	// hub as a second cluster. Only the leader election lease lives here.
	//
	// Putting the lease on the hub instead would write constantly to a control
	// plane everything else federates through, and would make this component's
	// leadership only as available as that plane: a hub hiccup would churn
	// leadership and restart the controller. An unreachable hub has to cost
	// this component its work. It does not have to cost it its identity.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "fabric-identity-controller.cloud.datumapis.com",
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

	hub, err := cluster.New(hubRestConfig, func(options *cluster.Options) {
		options.Scheme = scheme
	})
	if err != nil {
		setupLog.Error(err, "unable to reach the federation hub")
		os.Exit(1)
	}
	// A cluster carries a cache, so the manager puts it in the cache group
	// rather than the leader election group: it starts on every replica, ahead
	// of any election, and startup blocks until its cache has synced. That is
	// why nothing below probes the hub for readiness. A pod cannot report ready
	// while the hub is unreadable, because it has not finished starting.
	if err := mgr.Add(hub); err != nil {
		setupLog.Error(err, "unable to run the federation hub's cache")
		os.Exit(1)
	}

	// Networks and Hub are both the hub. A Network lives in its consumer's
	// project control plane, which this binary cannot reach, so it is read
	// there as a copy the network operator publishes. They stay separate fields
	// because a read failing and a write failing have to be distinguishable.
	if err := (&controller.NetworkFabricIdentityReconciler{
		Networks:          hub.GetClient(),
		Hub:               hub.GetClient(),
		HubCluster:        hub,
		IPAM:              ipamClients,
		IdentityClass:     identityClass,
		IdentityNamespace: identityNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkFabricIdentity")
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

	setupLog.Info("starting fabric identity controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
