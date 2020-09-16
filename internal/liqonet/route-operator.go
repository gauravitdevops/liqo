/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package liqonetOperators

import (
	"context"
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
	netv1alpha1 "github.com/liqotech/liqo/api/net/v1alpha1"
	liqonetOperator "github.com/liqotech/liqo/pkg/liqonet"
	"github.com/vishvananda/netlink"
	k8sApiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"net"
	"os"
	"os/signal"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	LiqonetPostroutingChain = "LIQONET-POSTROUTING"
	LiqonetPreroutingChain  = "LIQONET-PREROUTING"
	LiqonetForwardingChain  = "LIQONET-FORWARD"
	LiqonetInputChain       = "LIQONET-INPUT"
	NatTable                = "nat"
	FilterTable             = "filter"
	shutdownSignals         = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGKILL}
)

// RouteController reconciles a TunnelEndpoint object
type RouteController struct {
	client.Client
	Log            logr.Logger
	Scheme         *runtime.Scheme
	clientset      kubernetes.Clientset
	Recorder       record.EventRecorder
	NodeName       string
	ClientSet      *kubernetes.Clientset
	RemoteVTEPs    []string
	IsGateway      bool
	VxlanNetwork   string
	GatewayVxlanIP string
	VxlanIfaceName string
	VxlanPort      int
	IPtables       liqonetOperator.IPTables
	NetLink        liqonetOperator.NetLink
	ClusterPodCIDR string
	Configured     chan bool //channel to comunicate when the podCIDR has been set
	IsConfigured   bool      //true when the operator is configured and ready to be started
	//here we save only the rules that reference the custom chains added by us
	//we need them at deletion time
	IPTablesRuleSpecsReferencingChains map[string]liqonetOperator.IPtableRule //using a map to avoid duplicate entries. the key is the rulespec
	//here we save the custom iptables chains, this chains are added at startup time so there should not be duplicates
	//but we use a map to avoid them in case the operator crashes and then is restarted by kubernetes
	IPTablesChains map[string]liqonetOperator.IPTableChain
	//for each cluster identified by clusterID we save all the rulespecs needed to ensure communication with its pods
	IPtablesRuleSpecsPerRemoteCluster map[string][]liqonetOperator.IPtableRule
	//here we save routes associated to each remote cluster
	RoutesPerRemoteCluster map[string][]netlink.Route
	RetryTimeout           time.Duration
}

// +kubebuilder:rbac:groups=net.liqo.io,resources=tunnelendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.liqo.io,resources=tunnelendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list

func (r *RouteController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("endpoint", req.NamespacedName)
	var endpoint netv1alpha1.TunnelEndpoint
	//name of our finalizer
	routeOperatorFinalizer := "routeOperator-" + r.NodeName + "-Finalizer.net.liqo.io"

	if err := r.Get(ctx, req.NamespacedName, &endpoint); err != nil {
		r.Log.Error(err, "unable to fetch endpoint")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	//we can process a tunnelendpoint resource only if the tunnel interface has been created and configured
	//this network interface is managed by the tunnel operator who sets this field when process the same tunnelendpoint
	//resource and creates the network interface.
	//if it has not been created yet than we return and wait for the resource to be processed by the tunnel operator.
	if endpoint.Status.TunnelIFaceName == "" {
		log.Info("the tunnel network interface is not ready")
		return ctrl.Result{RequeueAfter: r.RetryTimeout}, nil
	}
	// examine DeletionTimestamp to determine if object is under deletion
	if endpoint.ObjectMeta.DeletionTimestamp.IsZero() {
		if !liqonetOperator.ContainsString(endpoint.ObjectMeta.Finalizers, routeOperatorFinalizer) {
			// The object is not being deleted, so if it does not have our finalizer,
			// then lets add the finalizer and update the object. This is equivalent
			// registering our finalizer.
			endpoint.ObjectMeta.Finalizers = append(endpoint.Finalizers, routeOperatorFinalizer)
			if err := r.Update(ctx, &endpoint); err != nil {
				//while updating we check if the a resource version conflict happened
				//which means the version of the object we have is outdated.
				//a solution could be to return an error and requeue the object for later process
				//but if the object has been changed by another instance of the controller running in
				//another host it already has been put in the working queue so decide to forget the
				//current version and process the next item in the queue assured that we handle the object later
				if k8sApiErrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				r.Log.Error(err, "unable to update endpoint")
				return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
			}
		}
	} else {
		//the object is being deleted
		//if we encounter an error while removing iptables rules or the routes than we record an
		//event on the resource to notify the user
		//the finalizer is not removed
		if liqonetOperator.ContainsString(endpoint.Finalizers, routeOperatorFinalizer) {
			if err := r.deleteIPTablesRulespecForRemoteCluster(&endpoint); err != nil {
				r.Log.Error(err, "error while deleting rulespec from iptables")
				r.Recorder.Event(&endpoint, "Warning", "Processing", err.Error())
				return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
			}
			if err := r.deleteRoutesPerCluster(&endpoint); err != nil {
				r.Log.Error(err, "error while deleting routes")
				r.Recorder.Event(&endpoint, "Warning", "Processing", err.Error())
				return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
			}
			//remove the finalizer from the list and update it.
			endpoint.Finalizers = liqonetOperator.RemoveString(endpoint.Finalizers, routeOperatorFinalizer)
			if err := r.Update(ctx, &endpoint); err != nil {
				if k8sApiErrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				r.Log.Error(err, "unable to update")
				return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
			}
		}
		return ctrl.Result{RequeueAfter: r.RetryTimeout}, nil
	}

	if err := r.createAndInsertIPTablesChains(); err != nil {
		r.Log.Error(err, "unable to create iptables chains")
		r.Recorder.Event(&endpoint, "Warning", "Processing", err.Error())
		return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
	} else {
		r.Recorder.Event(&endpoint, "Normal", "Processing", "iptables chains inserted")
	}
	if err := r.addIPTablesRulespecForRemoteCluster(&endpoint); err != nil {
		log.Error(err, "unable to insert ruleSpec")
		r.Recorder.Event(&endpoint, "Warning", "Processing", err.Error())
		return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
	} else {
		r.Recorder.Event(&endpoint, "Normal", "Processing", "iptables rules inserted")
	}
	if err := r.InsertRoutesPerCluster(&endpoint); err != nil {
		log.Error(err, "unable to insert routes")
		r.Recorder.Event(&endpoint, "Warning", "Processing", err.Error())
		return ctrl.Result{RequeueAfter: r.RetryTimeout}, err
	} else {
		r.Recorder.Event(&endpoint, "Normal", "Processing", "routes inserted")
	}

	return ctrl.Result{RequeueAfter: r.RetryTimeout}, nil
}

//this function is called at startup of the operator
//here we:
//create LIQONET-FORWARD in the filter table and insert it in the "FORWARD" chain
//create LIQONET-POSTROUTING in the nat table and insert it in the "POSTROUTING" chain
//create LIQONET-INPUT in the filter table and insert it in the input chain
//insert the rulespec which allows in input all the udp traffic incoming for the vxlan in the LIQONET-INPUT chain
func (r *RouteController) createAndInsertIPTablesChains() error {
	var err error
	ipt := r.IPtables
	log := r.Log.WithName("iptables")
	//creating LIQONET-POSTROUTING chain
	if err = liqonetOperator.CreateIptablesChainsIfNotExist(ipt, NatTable, LiqonetPostroutingChain); err != nil {
		return err
	} else {
		log.Info("created", "chain", LiqonetPostroutingChain, "in table", NatTable)
	}
	r.IPTablesChains[LiqonetPostroutingChain] = liqonetOperator.IPTableChain{
		Table: NatTable,
		Name:  LiqonetPostroutingChain,
	}
	//installing rulespec which forwards all traffic to LIQONET-POSTROUTING chain
	forwardToLiqonetPostroutingRuleSpec := []string{"-j", LiqonetPostroutingChain}
	if err = liqonetOperator.InsertIptablesRulespecIfNotExists(ipt, NatTable, "POSTROUTING", forwardToLiqonetPostroutingRuleSpec); err != nil {
		return err
	} else {
		log.Info("installed", "rulespec", strings.Join(forwardToLiqonetPostroutingRuleSpec, " "), "belonging to chain POSTROUTING in table", NatTable)
	}
	//add it to iptables rulespec if it does not exist in the map
	r.IPTablesRuleSpecsReferencingChains[strings.Join(forwardToLiqonetPostroutingRuleSpec, " ")] = liqonetOperator.IPtableRule{
		Table:    NatTable,
		Chain:    "POSTROUTING",
		RuleSpec: forwardToLiqonetPostroutingRuleSpec,
	}
	//creating LIQONET-PREROUTING chain
	if err = liqonetOperator.CreateIptablesChainsIfNotExist(ipt, NatTable, LiqonetPreroutingChain); err != nil {
		return err
	} else {
		log.Info("created", "chain", LiqonetPreroutingChain, "in table", NatTable)
	}
	r.IPTablesChains[LiqonetPreroutingChain] = liqonetOperator.IPTableChain{
		Table: NatTable,
		Name:  LiqonetPreroutingChain,
	}
	//installing rulespec which forwards all traffic to LIQONET-PREROUTING chain
	forwardToLiqonetPreroutingRuleSpec := []string{"-j", LiqonetPreroutingChain}
	if err = liqonetOperator.InsertIptablesRulespecIfNotExists(ipt, NatTable, "PREROUTING", forwardToLiqonetPreroutingRuleSpec); err != nil {
		return err
	} else {
		log.Info("installed", "rulespec", strings.Join(forwardToLiqonetPreroutingRuleSpec, " "), "belonging to chain POSTROUTING in table", NatTable)
	}
	//add it to iptables rulespec if it does not exist in the map
	r.IPTablesRuleSpecsReferencingChains[strings.Join(forwardToLiqonetPreroutingRuleSpec, " ")] = liqonetOperator.IPtableRule{
		Table:    NatTable,
		Chain:    "PREROUTING",
		RuleSpec: forwardToLiqonetPreroutingRuleSpec,
	}
	//creating LIQONET-FORWARD chain
	if err = liqonetOperator.CreateIptablesChainsIfNotExist(ipt, FilterTable, LiqonetForwardingChain); err != nil {
		return err
	} else {
		log.Info("created", "chain", LiqonetForwardingChain, "in table", FilterTable)
	}
	r.IPTablesChains[LiqonetForwardingChain] = liqonetOperator.IPTableChain{
		Table: FilterTable,
		Name:  LiqonetForwardingChain,
	}
	//installing rulespec which forwards all traffic to LIQONET-FORWARD chain
	forwardToLiqonetForwardRuleSpec := []string{"-j", LiqonetForwardingChain}
	if err = liqonetOperator.InsertIptablesRulespecIfNotExists(ipt, FilterTable, "FORWARD", forwardToLiqonetForwardRuleSpec); err != nil {
		return err
	} else {
		log.Info("installed", "rulespec", strings.Join(forwardToLiqonetForwardRuleSpec, " "), "belonging to chain FORWARD in table", FilterTable)
	}
	r.IPTablesRuleSpecsReferencingChains[strings.Join(forwardToLiqonetForwardRuleSpec, " ")] = liqonetOperator.IPtableRule{
		Table:    FilterTable,
		Chain:    "FORWARD",
		RuleSpec: forwardToLiqonetForwardRuleSpec,
	}
	//creating LIQONET-INPUT chain
	if err = liqonetOperator.CreateIptablesChainsIfNotExist(ipt, FilterTable, LiqonetInputChain); err != nil {
		return err
	} else {
		log.Info("created", "chain", LiqonetInputChain, "in table", FilterTable)
	}
	r.IPTablesChains[LiqonetInputChain] = liqonetOperator.IPTableChain{
		Table: FilterTable,
		Name:  LiqonetInputChain,
	}
	//installing rulespec which forwards all udp incoming traffic to LIQONET-INPUT chain
	forwardToLiqonetInputSpec := []string{"-p", "udp", "-m", "udp", "-j", LiqonetInputChain}
	if err = liqonetOperator.InsertIptablesRulespecIfNotExists(ipt, FilterTable, "INPUT", forwardToLiqonetInputSpec); err != nil {
		return err
	} else {
		log.Info("installed", "rulespec", strings.Join(forwardToLiqonetInputSpec, " "), "belonging to chain INPUT in table", FilterTable)
	}
	r.IPTablesRuleSpecsReferencingChains[strings.Join(forwardToLiqonetInputSpec, " ")] = liqonetOperator.IPtableRule{
		Table:    FilterTable,
		Chain:    "INPUT",
		RuleSpec: forwardToLiqonetInputSpec,
	}
	//installing rulespec which allows udp traffic with destination port the VXLAN port
	//we put it here because this rulespec is independent from the remote cluster.
	//we don't save this rulespec it will be removed when the chains are flushed at exit time
	//TODO: do we need to move this one elsewhere? maybe in a dedicate function called at startup by the route operator?
	vxlanUdpRuleSpec := []string{"-p", "udp", "-m", "udp", "--dport", strconv.Itoa(r.VxlanPort), "-j", "ACCEPT"}
	if err = ipt.AppendUnique(FilterTable, LiqonetInputChain, vxlanUdpRuleSpec...); err != nil {
		return fmt.Errorf("unable to insert rulespec \"%s\" in %s table and %s chain: %v", vxlanUdpRuleSpec, FilterTable, LiqonetInputChain, err)
	} else {
		log.Info("installed", "rulespec", strings.Join(vxlanUdpRuleSpec, " "), "belonging to chain", LiqonetInputChain, "in table", FilterTable)
	}
	return nil
}

func (r *RouteController) addIPTablesRulespecForRemoteCluster(endpoint *netv1alpha1.TunnelEndpoint) error {
	var remotePodCIDR, localPodCIDR string
	var err error
	clusterID := endpoint.Spec.ClusterID
	log := r.Log.WithName("iptables")
	if endpoint.Status.RemoteRemappedPodCIDR != "None" && endpoint.Status.RemoteRemappedPodCIDR != "" {
		remotePodCIDR = endpoint.Status.RemoteRemappedPodCIDR
		log.Info("nat enabled", "pod cidr of cluster", clusterID, "remapped from", endpoint.Spec.PodCIDR, "to", endpoint.Status.RemoteRemappedPodCIDR)
	} else {
		remotePodCIDR = endpoint.Spec.PodCIDR
		log.Info("nat disabled", "using original pod cidr", endpoint.Spec.PodCIDR, "for cluster", clusterID)
	}
	if endpoint.Status.LocalRemappedPodCIDR != "None" && endpoint.Status.LocalRemappedPodCIDR != "" {
		localPodCIDR = endpoint.Status.LocalRemappedPodCIDR
	} else {
		localPodCIDR = r.ClusterPodCIDR
	}
	var ruleSpecs []liqonetOperator.IPtableRule
	ipt := r.IPtables
	//do not nat the traffic directed to the remote pods
	ruleSpec := []string{"-s", r.ClusterPodCIDR, "-d", remotePodCIDR, "-j", "ACCEPT"}
	if err = ipt.AppendUnique(NatTable, LiqonetPostroutingChain, ruleSpec...); err != nil {
		return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, NatTable, LiqonetPostroutingChain, err)
	} else {
		log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetPostroutingChain, "in table", NatTable)
	}
	ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
		Table:    NatTable,
		Chain:    LiqonetPostroutingChain,
		RuleSpec: ruleSpec,
	})
	r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
	//enable forwarding for all the traffic directed to the remote pods
	ruleSpec = []string{"-d", remotePodCIDR, "-j", "ACCEPT"}
	if err = ipt.AppendUnique(FilterTable, LiqonetForwardingChain, ruleSpec...); err != nil {
		return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, FilterTable, LiqonetForwardingChain, err)
	} else {
		log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetForwardingChain, "in table", FilterTable)
	}
	ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
		Table:    FilterTable,
		Chain:    LiqonetForwardingChain,
		RuleSpec: ruleSpec,
	})
	r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
	//this rules are needed in an environment where strictly policies are applied for the input chain
	ruleSpec = []string{"-s", r.ClusterPodCIDR, "-d", remotePodCIDR, "-j", "ACCEPT"}
	if err = ipt.AppendUnique(FilterTable, LiqonetInputChain, ruleSpec...); err != nil {
		return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, FilterTable, LiqonetInputChain, err)
	} else {
		log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetInputChain, "in table", FilterTable)
	}
	ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
		Table:    FilterTable,
		Chain:    LiqonetInputChain,
		RuleSpec: ruleSpec,
	})
	r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
	if r.IsGateway {
		//we get the first IP address from the podCIDR of the local cluster
		natIP, _, err := net.ParseCIDR(localPodCIDR)
		if err != nil {
			return fmt.Errorf("unable to get the IP from remotePodCidr %s used to NAT the traffic from localhosts to remote hosts", localPodCIDR)
		}
		//all the traffic coming from the hosts and directed to the remote pods is natted using the first IP address
		//taken from the podCIDR of the local cluster
		//all the traffic leaving the tunnel interface is source nated.
		ruleSpec = []string{"-o", endpoint.Status.TunnelIFaceName, "-j", "SNAT", "--to", natIP.String()}
		if err = ipt.AppendUnique(NatTable, LiqonetPostroutingChain, ruleSpec...); err != nil {
			return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, NatTable, LiqonetPostroutingChain, err)
		} else {
			log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetPostroutingChain, "in table", NatTable)
		}
		ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
			Table:    NatTable,
			Chain:    LiqonetPostroutingChain,
			RuleSpec: ruleSpec,
		})
		r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
		//if we have been remapped by the remote cluster then insert the iptables rule to masquerade the source ip
		if endpoint.Status.LocalRemappedPodCIDR != "None" {
			ruleSpec = []string{"-s", r.ClusterPodCIDR, "-d", remotePodCIDR, "-j", "NETMAP", "--to", endpoint.Status.LocalRemappedPodCIDR}
			if err = liqonetOperator.InsertIptablesRulespecIfNotExists(ipt, NatTable, LiqonetPostroutingChain, ruleSpec); err != nil {
				return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, NatTable, LiqonetPostroutingChain, err)
			} else {
				log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetPostroutingChain, "in table", NatTable)
			}
			ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
				Table:    NatTable,
				Chain:    LiqonetPostroutingChain,
				RuleSpec: ruleSpec,
			})
			r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
			//translate all the traffic coming to the local cluster in to the right podcidr because it has been remapped by the remote cluster
			ruleSpec = []string{"-d", endpoint.Status.LocalRemappedPodCIDR, "-i", endpoint.Status.TunnelIFaceName, "-j", "NETMAP", "--to", r.ClusterPodCIDR}
			if err = ipt.AppendUnique(NatTable, LiqonetPreroutingChain, ruleSpec...); err != nil {
				return fmt.Errorf("unable to insert iptable rule \"%s\" in %s table, %s chain: %v", ruleSpec, NatTable, LiqonetPreroutingChain, err)
			} else {
				log.Info("installed", "rulespec", strings.Join(ruleSpec, " "), "belonging to chain", LiqonetPreroutingChain, "in table", NatTable)
			}
			ruleSpecs = append(ruleSpecs, liqonetOperator.IPtableRule{
				Table:    NatTable,
				Chain:    LiqonetPreroutingChain,
				RuleSpec: ruleSpec,
			})
			r.IPtablesRuleSpecsPerRemoteCluster[clusterID] = ruleSpecs
		}
	}
	return nil
}

//remove all the rules added by addIPTablesRulespecForRemoteCluster function
func (r *RouteController) deleteIPTablesRulespecForRemoteCluster(endpoint *netv1alpha1.TunnelEndpoint) error {
	var err error
	clusterID := endpoint.Spec.ClusterID
	log := r.Log.WithName("iptables")
	ipt := r.IPtables
	//retrive the iptables rules for the remote cluster
	rules, ok := r.IPtablesRuleSpecsPerRemoteCluster[endpoint.Spec.ClusterID]
	if ok {
		for _, rule := range rules {
			if err = ipt.Delete(rule.Table, rule.Chain, rule.RuleSpec...); err != nil {
				// if the rule that we are trying to delete does not exist then we are fine and go on
				e, ok := err.(*iptables.Error)
				if ok && e.IsNotExist() {
					continue
				} else if !ok {
					return fmt.Errorf("unable to delete iptable rule \"%s\" in %s table, %s chain: %v", strings.Join(rule.RuleSpec, " "), rule.Table, rule.Chain, err)
				}
			} else {
				log.Info("removing", "rulespec", strings.Join(rule.RuleSpec, " "), "belonging to chain", rule.Chain, "in table", rule.Table)
			}
		}
	}
	//after all the iptables rules have been removed then we delete them from the map
	//this is safe to do even if the key does not exist
	delete(r.IPtablesRuleSpecsPerRemoteCluster, clusterID)
	return nil
}

//this function is called when the route-operator program is closed
//the errors are not checked because the function is called at exit time
//it cleans up all the possible resources
//a log message is emitted if in case of error
//only if the iptables binaries are missing an error is returned
func (r *RouteController) DeleteAllIPTablesChains() {
	var err error
	logger := r.Log.WithName("DeleteAllIPTablesChains")
	ipt := r.IPtables
	//first all the iptables chains are flushed
	for k, chain := range r.IPTablesChains {
		if err = ipt.ClearChain(chain.Table, chain.Name); err != nil {
			e, ok := err.(*iptables.Error)
			if ok && e.IsNotExist() {
				delete(r.IPTablesChains, k)
			} else if !ok {
				logger.Error(err, "unable to clear: ", "chain", chain.Name, "in table", chain.Table)
			}

		}
	}
	for k := range r.IPtablesRuleSpecsPerRemoteCluster {
		delete(r.IPtablesRuleSpecsPerRemoteCluster, k)
	}
	//second we delete the references to the chains
	for k, rulespec := range r.IPTablesRuleSpecsReferencingChains {
		if err = ipt.Delete(rulespec.Table, rulespec.Chain, rulespec.RuleSpec...); err != nil {
			e, ok := err.(*iptables.Error)
			if ok && e.IsNotExist() {
				delete(r.IPTablesRuleSpecsReferencingChains, k)
			} else if !ok {
				logger.Error(err, "unable to delete: ", "rule", strings.Join(rulespec.RuleSpec, ""), "in chain", rulespec.Chain, "in table", rulespec.Table)
			}
		}
	}
	//then we delete the chains which now should be empty
	for k, chain := range r.IPTablesChains {
		if err = ipt.DeleteChain(chain.Table, chain.Name); err != nil {
			e, ok := err.(*iptables.Error)
			if ok && e.IsNotExist() {
				delete(r.IPTablesChains, k)
			} else if !ok {
				logger.Error(err, "unable to delete ", "chain", chain.Name, "in table", chain.Table)
			}
		}
	}
}

func (r *RouteController) InsertRoutesPerCluster(endpoint *netv1alpha1.TunnelEndpoint) error {
	clusterID := endpoint.Spec.ClusterID
	log := r.Log.WithName("route")
	var remotePodCIDR string
	if endpoint.Status.RemoteRemappedPodCIDR != "None" && endpoint.Status.RemoteRemappedPodCIDR != "" {
		remotePodCIDR = endpoint.Status.RemoteRemappedPodCIDR
		log.Info("installing routes for", "cluster", clusterID, "with remapped pod cidr", remotePodCIDR)
	} else {
		remotePodCIDR = endpoint.Spec.PodCIDR
		log.Info("installing routes for", "cluster", clusterID, "with original pod cidr", remotePodCIDR)
	}
	var routes []netlink.Route
	if r.IsGateway {
		route, err := r.NetLink.AddRoute(remotePodCIDR, "", endpoint.Status.TunnelIFaceName, false)
		if err != nil {
			return err
		} else {
			log.Info("installing", "route", route.String())
		}
		routes = append(routes, route)
		r.RoutesPerRemoteCluster[endpoint.Spec.ClusterID] = routes
	} else {
		route, err := r.NetLink.AddRoute(remotePodCIDR, r.GatewayVxlanIP, r.VxlanIfaceName, false)
		if err != nil {
			return err
		} else {
			log.Info("installing", "route", route.String())
		}
		routes = append(routes, route)
		r.RoutesPerRemoteCluster[endpoint.Spec.ClusterID] = routes
	}
	return nil
}

//used to remove the routes when a tunnelEndpoint CR is removed
func (r *RouteController) deleteRoutesPerCluster(endpoint *netv1alpha1.TunnelEndpoint) error {
	clusterID := endpoint.Spec.ClusterID
	log := r.Log.WithName("route")
	log.Info("removing all routes for", "cluster", clusterID)
	for _, route := range r.RoutesPerRemoteCluster[endpoint.Spec.ClusterID] {
		err := r.NetLink.DelRoute(route)
		if err != nil {
			return err
		} else {
			log.Info("deleting", "route", route.String())
		}
	}
	//after all the routes have been removed then we delete them from the map
	//this is safe to do even if the key does not exist
	delete(r.RoutesPerRemoteCluster, clusterID)
	return nil
}

func (r *RouteController) deleteAllRoutes() {
	logger := r.Log.WithName("DeleteAllRoutes")
	//the errors are not checked because the function is called at exit time
	//it cleans up all the possible resources
	//a log message is emitted if in case of error
	for k, cluster := range r.RoutesPerRemoteCluster {
		for _, route := range cluster {
			if err := r.NetLink.DelRoute(route); err != nil {
				logger.Error(err, "an error occurred while deleting", "route", route.String())
			}
		}
		delete(r.RoutesPerRemoteCluster, k)
	}
}

//this function deletes the vxlan interface in host where the route operator is running
func (r *RouteController) deleteVxlanIFace() {
	logger := r.Log.WithName("DeleteVxlanIFace")
	//first get the iface index
	iface, err := netlink.LinkByName(r.VxlanIfaceName)
	if err != nil {
		logger.Error(err, "an error occurred while removing vxlan interface", "ifaceName", r.VxlanIfaceName)
	}
	err = liqonetOperator.DeleteIFaceByIndex(iface.Attrs().Index)
	if err != nil {
		logger.Error(err, "an error occurred while removing vxlan interface", "ifaceName", r.VxlanIfaceName)
	}
}

// SetupSignalHandlerForRouteOperator registers for SIGTERM, SIGINT. A stop channel is returned
// which is closed on one of these signals.
func (r *RouteController) SetupSignalHandlerForRouteOperator() (stopCh <-chan struct{}) {
	logger := r.Log.WithValues("Route Operator Signal Handler", r.NodeName)
	fmt.Printf("Entering signal handler")
	stop := make(chan struct{})
	c := make(chan os.Signal, 1)
	signal.Notify(c, shutdownSignals...)
	go func(r *RouteController) {
		sig := <-c
		logger.Info("received ", "signal", sig.String())
		r.DeleteAllIPTablesChains()
		r.deleteAllRoutes()
		r.deleteVxlanIFace()
		<-c
		close(stop)
	}(r)
	return stop
}

func (r *RouteController) SetupWithManager(mgr ctrl.Manager) error {
	resourceToBeProccesedPredicate := predicate.Funcs{
		DeleteFunc: func(e event.DeleteEvent) bool {
			//finalizers are used to check if a resource is being deleted, and perform there the needed actions
			//we don't want to reconcile on the delete of a resource.
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).WithEventFilter(resourceToBeProccesedPredicate).
		For(&netv1alpha1.TunnelEndpoint{}).
		Complete(r)
}
