package main

import (
	"flag"
	clusterConfig "github.com/liqoTech/liqo/api/cluster-config/v1"
	discoveryv1 "github.com/liqoTech/liqo/api/discovery/v1"
	"github.com/liqoTech/liqo/internal/dispatcher"
	util "github.com/liqoTech/liqo/pkg/liqonet"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	scheme          = runtime.NewScheme()
	clusteIDConfMap = "cluster-id"
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	flag.Parse()
	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme,
		Port:           9443,
		LeaderElection: false,
	})
	if err != nil {
		klog.Error(err, "unable to start manager")
		os.Exit(-1)
	}
	//create a clientSet
	k8sClient := kubernetes.NewForConfigOrDie(cfg)
	//get namespace where the operator is running
	namespaceName, found := os.LookupEnv("NAMESPACE")
	if !found {
		klog.Errorf("namespace env variable not set, please set it in manifest file o the operator")
		os.Exit(-1)
	}
	clusterID, err := util.GetClusterID(k8sClient, clusteIDConfMap, namespaceName)
	if err != nil {
		klog.Errorf("an error occurred while retrieving the clusterID: %s", err)
	} else {
		klog.Infof("setting local clusterID to: %s", clusterID)
	}

	d := &dispatcher.DispatcherReconciler{
		Scheme:                mgr.GetScheme(),
		Client:                mgr.GetClient(),
		ClientSet:             k8sClient,
		ClusterID:             clusterID,
		RemoteDynClients:      make(map[string]dynamic.Interface),
		LocalDynClient:        dynamic.NewForConfigOrDie(cfg),
		RegisteredResources:   nil,
		UnregisteredResources: nil,
		LocalWatchers:         make(map[string]chan bool),
		RemoteWatchers:        make(map[string]map[string]chan bool),
	}
	if err = d.SetupWithManager(mgr); err != nil {
		klog.Error(err, "unable to setup the dispatcher-operator")
		os.Exit(1)
	}
	err = d.WatchConfiguration(cfg, &clusterConfig.GroupVersion)
	if err != nil {
		klog.Error(err)
		os.Exit(-1)
	}
	klog.Info("Starting dispatcher-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Error(err, "problem running manager")
		os.Exit(1)
	}
}