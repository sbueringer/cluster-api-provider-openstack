/*
Copyright 2018 The Kubernetes Authors.

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

package clients

import (
	"errors"
	"fmt"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/monitors"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/pools"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/pkg/util"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/attributestags"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/loadbalancers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"k8s.io/klog"
	openstackconfigv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
)

const (
	networkPrefix string = "k8s"
)

// NetworkService interfaces with the OpenStack Networking API.
// It will create a network related infrastructure for the cluster, like network, subnet, router.
type NetworkService struct {
	client *gophercloud.ServiceClient
}

// NewNetworkService returns an instance for the OpenStack Networking API
func NewNetworkService(client *gophercloud.ServiceClient) (*NetworkService, error) {
	return &NetworkService{
		client: client,
	}, nil
}

func (s *NetworkService) ReconcileNetwork(clusterName string, desired openstackconfigv1.OpenstackClusterProviderSpec, status *openstackconfigv1.OpenstackClusterProviderStatus) error {
	klog.Infof("Reconciling network components for cluster %s", clusterName)
	if desired.NodeCIDR == "" {
		klog.V(4).Infof("No need to reconcile network for cluster %s", clusterName)
		return nil
	}
	networkName := fmt.Sprintf("%s-cluster-%s", networkPrefix, clusterName)
	network, err := s.reconcileNetwork(clusterName, networkName, desired)
	if err != nil {
		return err
	}
	if network.ID == "" {
		klog.V(4).Infof("No need to reconcile network components since no network exists.")
		status.Network = nil
		return nil
	}
	status.Network = &network

	observedSubnet, err := s.reconcileSubnets(clusterName, networkName, desired, network)
	if err != nil {
		return err
	}
	if observedSubnet.ID == "" {
		klog.V(4).Infof("No need to reconcile further network components since no subnet exists.")
		status.Network.Subnet = nil
		return nil
	}
	network.Subnet = &observedSubnet

	observedRouter, err := s.reconcileRouter(clusterName, networkName, desired, network)
	if err != nil {
		return err
	}
	if observedRouter.ID != "" {
		// Only appending the router if it has an actual id
		network.Router = &observedRouter
	} else {
		status.Network.Router = nil
	}

	return nil
}

// Reconcile the Network for a given cluster
func (s *NetworkService) ReconcileLoadBalancers(clusterName string, desired openstackconfigv1.OpenstackClusterProviderSpec, status *openstackconfigv1.OpenstackClusterProviderStatus, clusterMachines *clusterv1.MachineList) error {

	apiServerLoadBalancer, err := s.reconcileLoadBalancer(clusterName, desired, status, clusterMachines)
	if err != nil {
		return err
	}
	status.Network.APIServerLoadBalancer = apiServerLoadBalancer

	return nil
}

type createOpts struct {
	AdminStateUp        *bool  `json:"admin_state_up,omitempty"`
	Name                string `json:"name,omitempty"`
	PortSecurityEnabled *bool  `json:"port_security_enabled,omitempty"`
}

func (c createOpts) ToNetworkCreateMap() (map[string]interface{}, error) {
	return gophercloud.BuildRequestBody(c, "network")
}

func (s *NetworkService) reconcileNetwork(clusterName, networkName string, desired openstackconfigv1.OpenstackClusterProviderSpec) (openstackconfigv1.Network, error) {
	klog.Infof("Reconciling network %s", networkName)
	emptyNetwork := openstackconfigv1.Network{}
	res, err := s.getNetworkByName(networkName)
	if err != nil {
		return emptyNetwork, err
	}

	if res.ID != "" {
		// Network exists
		return openstackconfigv1.Network{
			ID:   res.ID,
			Name: res.Name,
		}, nil
	}

	createOpts := createOpts{
		AdminStateUp:        gophercloud.Enabled,
		Name:                networkName,
		PortSecurityEnabled: gophercloud.Disabled,
	}

	network, err := networks.Create(s.client, createOpts).Extract()
	if err != nil {
		return emptyNetwork, err
	}

	_, err = attributestags.ReplaceAll(s.client, "networks", network.ID, attributestags.ReplaceAllOpts{
		Tags: []string{
			"cluster-api-provider-openstack",
			clusterName,
		}}).Extract()
	if err != nil {
		return emptyNetwork, err
	}

	return openstackconfigv1.Network{
		ID:   network.ID,
		Name: network.Name,
	}, nil
}

func (s *NetworkService) reconcileSubnets(clusterName, name string, desired openstackconfigv1.OpenstackClusterProviderSpec, network openstackconfigv1.Network) (openstackconfigv1.Subnet, error) {
	klog.Infof("Reconciling subnet %s", name)
	emptySubnet := openstackconfigv1.Subnet{}
	allPages, err := subnets.List(s.client, subnets.ListOpts{
		NetworkID: network.ID,
		CIDR:      desired.NodeCIDR,
	}).AllPages()
	if err != nil {
		return emptySubnet, err
	}

	subnetList, err := subnets.ExtractSubnets(allPages)
	if err != nil {
		return emptySubnet, err
	}

	var observedSubnet openstackconfigv1.Subnet
	if len(subnetList) > 1 {
		// Not panicing here, because every other cluster might work.
		return emptySubnet, fmt.Errorf("found more than 1 network with the expected name (%d) and CIDR (%s), which should not be able to exist in OpenStack", len(subnetList), desired.NodeCIDR)
	} else if len(subnetList) == 0 {
		opts := subnets.CreateOpts{
			NetworkID: network.ID,
			Name:      name,
			IPVersion: 4,

			CIDR:           desired.NodeCIDR,
			DNSNameservers: desired.DNSNameservers,
		}

		newSubnet, err := subnets.Create(s.client, opts).Extract()
		if err != nil {
			return emptySubnet, err
		}
		observedSubnet = openstackconfigv1.Subnet{
			ID:   newSubnet.ID,
			Name: newSubnet.Name,

			CIDR: newSubnet.CIDR,
		}
	} else if len(subnetList) == 1 {
		observedSubnet = openstackconfigv1.Subnet{
			ID:   subnetList[0].ID,
			Name: subnetList[0].Name,

			CIDR: subnetList[0].CIDR,
		}
	}

	_, err = attributestags.ReplaceAll(s.client, "subnets", observedSubnet.ID, attributestags.ReplaceAllOpts{
		Tags: []string{
			"cluster-api-provider-openstack",
			clusterName,
		}}).Extract()
	if err != nil {
		return emptySubnet, err
	}

	return observedSubnet, nil
}

func (s *NetworkService) reconcileRouter(clusterName, name string, desired openstackconfigv1.OpenstackClusterProviderSpec, network openstackconfigv1.Network) (openstackconfigv1.Router, error) {
	klog.Infof("Reconciling router %s", name)
	emptyRouter := openstackconfigv1.Router{}
	if network.ID == "" {
		klog.V(3).Info("No need to reconcile router. There is no network.")
		return emptyRouter, nil
	}
	if network.Subnet == nil {
		klog.V(3).Info("No need to reconcile router. There is no subnet.")
		return emptyRouter, nil
	}
	if desired.ExternalNetworkID == "" {
		klog.V(3).Info("No need to create router, due to missing ExternalNetworkID")
		return emptyRouter, nil
	}

	allPages, err := routers.List(s.client, routers.ListOpts{
		Name: name,
	}).AllPages()
	if err != nil {
		return emptyRouter, err
	}

	routerList, err := routers.ExtractRouters(allPages)
	if err != nil {
		return emptyRouter, err
	}
	var router routers.Router
	if len(routerList) == 0 {
		opts := routers.CreateOpts{
			Name: name,
		}
		// only set the GatewayInfo right now when no externalIPs
		// should be configured
		if len(desired.ExternalRouterIPs) == 0 {
			opts.GatewayInfo = &routers.GatewayInfo{
				NetworkID: desired.ExternalNetworkID,
			}
		}
		newRouter, err := routers.Create(s.client, opts).Extract()
		if err != nil {
			return emptyRouter, err
		}
		router = *newRouter
	} else {
		router = routerList[0]
	}

	if len(desired.ExternalRouterIPs) > 0 {
		var updateOpts routers.UpdateOpts
		updateOpts.GatewayInfo = &routers.GatewayInfo{
			NetworkID: desired.ExternalNetworkID,
		}
		for _, externalRouterIP := range desired.ExternalRouterIPs {
			subnetID := externalRouterIP.Subnet.UUID
			if subnetID == "" {
				sopts := subnets.ListOpts(externalRouterIP.Subnet.Filter)
				snets, err := getSubnetsByFilter(s.client, &sopts)
				if err != nil {
					return emptyRouter, err
				}
				if len(snets) != 1 {
					return emptyRouter, fmt.Errorf("subnetParam didn't exactly match one subnet")
				}
				subnetID = snets[0].ID
			}
			updateOpts.GatewayInfo.ExternalFixedIPs = append(updateOpts.GatewayInfo.ExternalFixedIPs, routers.ExternalFixedIP{
				IPAddress: externalRouterIP.FixedIP,
				SubnetID:  subnetID,
			})
		}

		_, err = routers.Update(s.client, router.ID, updateOpts).Extract()
		if err != nil {
			return emptyRouter, fmt.Errorf("error updating OpenStack Neutron Router: %s", err)
		}
	}

	observedRouter := openstackconfigv1.Router{
		Name: router.Name,
		ID:   router.ID,
	}

	routerInterfaces, err := s.getRouterInterfaces(router.ID)
	if err != nil {
		return emptyRouter, err
	}

	createInterface := true
	// check all router interfaces for an existing port in our subnet.
INTERFACE_LOOP:
	for _, iface := range routerInterfaces {
		for _, ip := range iface.FixedIPs {
			if ip.SubnetID == network.Subnet.ID {
				createInterface = false
				break INTERFACE_LOOP
			}
		}
	}

	// ... and create a router interface for our subnet.
	if createInterface {
		klog.V(4).Infof("Creating RouterInterface on %s in subnet %s", router.ID, network.Subnet.ID)
		iface, err := routers.AddInterface(s.client, router.ID, routers.AddInterfaceOpts{
			SubnetID: network.Subnet.ID,
		}).Extract()
		if err != nil {
			return observedRouter, fmt.Errorf("unable to create router interface: %v", err)
		}
		klog.V(4).Infof("Created RouterInterface: %v", iface)
	}

	_, err = attributestags.ReplaceAll(s.client, "routers", observedRouter.ID, attributestags.ReplaceAllOpts{
		Tags: []string{
			"cluster-api-provider-openstack",
			clusterName,
		}}).Extract()
	if err != nil {
		return emptyRouter, err
	}

	return observedRouter, nil
}

const kubeapiLBObjectsName = "kubeapi"

func (s *NetworkService) reconcileLoadBalancer(clusterName string, desired openstackconfigv1.OpenstackClusterProviderSpec, status *openstackconfigv1.OpenstackClusterProviderStatus, clusterMachines *clusterv1.MachineList) (*openstackconfigv1.LoadBalancer, error) {
	klog.Info("Reconciling master load balancer")

	// TODO add more check
	if desired.ExternalNetworkID == "" {
		klog.V(3).Info("No need to create floating ip, due to missing ExternalNetworkID")
		return nil, nil
	}
	if desired.ExternalSubnetID == "" {
		klog.V(3).Info("No need to create floating ip, due to missing ExternalSubnetID")
		return nil, nil
	}

	split := strings.Split(desired.ClusterConfiguration.ControlPlaneEndpoint, ":")
	if len(split) != 2 {
		return nil, fmt.Errorf("format of ControlPlaneEndpoint is invalid")
	}
	port, err := strconv.Atoi(split[1])
	if err != nil {
		return nil, fmt.Errorf("error extracting port from controlPlaneEndpoint %s: %v", desired.ClusterConfiguration.ControlPlaneEndpoint, err)
	}
	observedLoadBalancer := &openstackconfigv1.LoadBalancer{
		IP:   split[0],
		Port: port,
	}

	// lb
	lb, err := checkIfLbExists(s.client, kubeapiLBObjectsName)
	if err != nil {
		return nil, err
	}
	if lb == nil {
		lbCreateOpts := loadbalancers.CreateOpts{
			Name:        kubeapiLBObjectsName,
			VipSubnetID: status.Network.Subnet.ID,
		}

		lb, err = loadbalancers.Create(s.client, lbCreateOpts).Extract()
		if err != nil {
			return nil, fmt.Errorf("error create loadbalancer: %s", err)
		}
		err = waitForLoadBalancer(s.client, lb.ID, "ACTIVE")
		if err != nil {
			return nil, err
		}
	}
	observedLoadBalancer.Name = lb.Name
	observedLoadBalancer.ID = lb.ID
	observedLoadBalancer.InternalIP = lb.VipAddress

	// floatingip
	fp, err := checkIfFloatingIPExists(s.client, observedLoadBalancer.IP)
	if err != nil {
		return nil, err
	}
	if fp == nil {
		fpCreateOpts := &floatingips.CreateOpts{
			FloatingIP:        observedLoadBalancer.IP,
			FloatingNetworkID: desired.ExternalNetworkID,
			PortID:            lb.VipPortID,
		}
		fp, err := floatingips.Create(s.client, fpCreateOpts).Extract()
		if err != nil {
			return nil, fmt.Errorf("error allocating floating IP: %s", err)
		}
		err = waitForFloatingIP(s.client, fp.ID, "ACTIVE")
		if err != nil {
			return nil, err
		}
	}

	// lb_listener
	for _, port := range []int{22, 6443} {
		lbPortObjectsName := fmt.Sprintf("%s-%d", kubeapiLBObjectsName, port)

		listener, err := checkIfListenerExists(s.client, lbPortObjectsName)
		if err != nil {
			return nil, err
		}
		if listener == nil {
			listenerCreateOpts := listeners.CreateOpts{
				Name:           lbPortObjectsName,
				Protocol:       "TCP",
				ProtocolPort:   port,
				LoadbalancerID: lb.ID,
				//DefaultPoolID:          d.Get("default_pool_id").(string), TODO check vio4/5
			}
			listener, err = listeners.Create(s.client, listenerCreateOpts).Extract()
			if err != nil {
				return nil, fmt.Errorf("error creating listener: %s", err)
			}
			err = waitForLoadBalancer(s.client, lb.ID, "ACTIVE")
			if err != nil {
				return nil, err
			}
			err = waitForListener(s.client, listener.ID, "ACTIVE")
			if err != nil {
				return nil, err
			}
		}

		// lb_pool
		pool, err := checkIfPoolExists(s.client, lbPortObjectsName)
		if err != nil {
			return nil, err
		}
		if pool == nil {
			poolCreateOpts := pools.CreateOpts{
				Name:       lbPortObjectsName,
				Protocol:   "TCP",
				LBMethod:   pools.LBMethodRoundRobin,
				ListenerID: listener.ID,
			}
			pool, err = pools.Create(s.client, poolCreateOpts).Extract()
			if err != nil {
				return nil, fmt.Errorf("error creating pool: %s", err)
			}
			err = waitForLoadBalancer(s.client, lb.ID, "ACTIVE")
			if err != nil {
				return nil, err
			}
		}
		observedLoadBalancer.PoolID = pool.ID

		// lb_monitor
		monitor, err := checkIfMonitorExists(s.client, lbPortObjectsName)
		if err != nil {
			return nil, err
		}
		if monitor == nil {
			monitorCreateOpts := monitors.CreateOpts{
				Name:       lbPortObjectsName,
				PoolID:     pool.ID,
				Type:       "TCP",
				Delay:      30,
				Timeout:    5,
				MaxRetries: 3,
			}
			_, err = monitors.Create(s.client, monitorCreateOpts).Extract()
			if err != nil {
				return nil, fmt.Errorf("error creating monitor: %s", err)
			}
			err = waitForLoadBalancer(s.client, lb.ID, "ACTIVE")
			if err != nil {
				return nil, err
			}
		}
	}
	return observedLoadBalancer, nil
}

func (s *NetworkService) CreateLBMember(networkClient *gophercloud.ServiceClient, machine *clusterv1.Machine, providerStatus *openstackconfigv1.OpenstackClusterProviderStatus) error {

	if !util.IsControlPlaneMachine(machine) {
		return nil
	}

	lbID := providerStatus.Network.APIServerLoadBalancer.ID
	subnetID := providerStatus.Network.Subnet.ID

	for _, port := range []int{22, 6443} {
		lbPortObjectsName := fmt.Sprintf("%s-%d", kubeapiLBObjectsName, port)
		name := lbPortObjectsName + "-" + machine.Name

		pool, err := checkIfPoolExists(s.client, lbPortObjectsName)
		if err != nil {
			return err
		}

		ip, ok := machine.ObjectMeta.Annotations[openstack.OpenstackIPAnnotationKey]
		if !ok {
			klog.Infof("no ip found yet on annotation %s on machine %s", openstack.OpenstackIPAnnotationKey, machine.Name)
			return nil
		}

		lbMember, err := checkIfLbMemberExists(networkClient, pool.ID, name)
		if err != nil {
			return err
		}

		if lbMember != nil {
			// check if we have to recreate the LB
			if lbMember.Address == ip {
				// nothing to do return
				return nil
			}

			// lb member changed so let's delete it so we can create it again with the correct IP
			err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
			err = pools.DeleteMember(networkClient, pool.ID, lbMember.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbmember: %s", err)
			}
			err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
		}

		// if we got to this point we should either create or re-create the lb member
		lbMemberOpts := pools.CreateMemberOpts{
			Name:         name,
			ProtocolPort: port,
			Address:      ip,
			SubnetID:     subnetID,
		}

		err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
		if err != nil {
			return err
		}
		lbMember, err = pools.CreateMember(networkClient, pool.ID, lbMemberOpts).Extract()
		if err != nil {
			return fmt.Errorf("error create lbmember: %s", err)
		}
		err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *NetworkService) DeleteLBMember(networkClient *gophercloud.ServiceClient, machine *clusterv1.Machine, providerStatus *openstackconfigv1.OpenstackClusterProviderStatus) error {

	if !util.IsControlPlaneMachine(machine) {
		return nil
	}

	lbID := providerStatus.Network.APIServerLoadBalancer.ID

	for _, port := range []int{22, 6443} {
		lbPortObjectsName := fmt.Sprintf("%s-%d", kubeapiLBObjectsName, port)
		name := lbPortObjectsName + "-" + machine.Name

		pool, err := checkIfPoolExists(s.client, lbPortObjectsName)
		if err != nil {
			return err
		}


		lbMember, err := checkIfLbMemberExists(networkClient, pool.ID, name)
		if err != nil {
			return err
		}

		if lbMember != nil {

			// lb member changed so let's delete it so we can create it again with the correct IP
			err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
			err = pools.DeleteMember(networkClient, pool.ID, lbMember.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbmember: %s", err)
			}
			err = waitForLoadBalancer(networkClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func checkIfLbExists(networkingClient *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	allPages, err := loadbalancers.List(networkingClient, loadbalancers.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	lbList, err := loadbalancers.ExtractLoadBalancers(allPages)
	if err != nil {
		return nil, err
	}
	if len(lbList) == 0 {
		return nil, nil
	}
	return &lbList[0], nil
}

func checkIfFloatingIPExists(networkingClient *gophercloud.ServiceClient, ip string) (*floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(networkingClient, floatingips.ListOpts{FloatingIP: ip}).AllPages()
	if err != nil {
		return nil, err
	}
	fpList, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return nil, err
	}
	if len(fpList) == 0 {
		return nil, nil
	}
	return &fpList[0], nil
}

func checkIfListenerExists(networkingClient *gophercloud.ServiceClient, name string) (*listeners.Listener, error) {
	allPages, err := listeners.List(networkingClient, listeners.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	listenerList, err := listeners.ExtractListeners(allPages)
	if err != nil {
		return nil, err
	}
	if len(listenerList) == 0 {
		return nil, nil
	}
	return &listenerList[0], nil
}

func checkIfPoolExists(networkingClient *gophercloud.ServiceClient, name string) (*pools.Pool, error) {
	allPages, err := pools.List(networkingClient, pools.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	poolList, err := pools.ExtractPools(allPages)
	if err != nil {
		return nil, err
	}
	if len(poolList) == 0 {
		return nil, nil
	}
	return &poolList[0], nil
}

func checkIfMonitorExists(networkingClient *gophercloud.ServiceClient, name string) (*monitors.Monitor, error) {
	allPages, err := monitors.List(networkingClient, monitors.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	monitorList, err := monitors.ExtractMonitors(allPages)
	if err != nil {
		return nil, err
	}
	if len(monitorList) == 0 {
		return nil, nil
	}
	return &monitorList[0], nil
}

func checkIfLbMemberExists(networkingClient *gophercloud.ServiceClient, poolID, name string) (*pools.Member, error) {
	allPages, err := pools.ListMembers(networkingClient, poolID, pools.ListMembersOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	lbMemberList, err := pools.ExtractMembers(allPages)
	if err != nil {
		return nil, err
	}
	if len(lbMemberList) == 0 {
		return nil, nil
	}
	return &lbMemberList[0], nil
}

var backoff = wait.Backoff{
	Steps:    10,
	Duration: 30 * time.Second,
	Factor:   1.0,
	Jitter:   0.1,
}

func waitForLoadBalancer(networkingClient *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for loadbalancer %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		lb, err := loadbalancers.Get(networkingClient, id).Extract()
		if err != nil {
			return false, err
		}
		return lb.ProvisioningStatus == target, nil
	})
}

func waitForFloatingIP(networkingClient *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for floatingip %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		fp, err := floatingips.Get(networkingClient, id).Extract()
		if err != nil {
			return false, err
		}
		return fp.Status == target, nil
	})
}

func waitForListener(networkingClient *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for listener %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := listeners.Get(networkingClient, id).Extract()
		if err != nil {
			return false, err
		}
		// The listener resource has no Status attribute, so a successful Get is the best we can do
		return true, nil
	})
}

func (s *NetworkService) getRouterInterfaces(routerID string) ([]ports.Port, error) {
	allPages, err := ports.List(s.client, ports.ListOpts{
		DeviceID: routerID,
	}).AllPages()
	if err != nil {
		return []ports.Port{}, err
	}

	portList, err := ports.ExtractPorts(allPages)
	if err != nil {
		return []ports.Port{}, err
	}

	return portList, nil
}

func (s *NetworkService) getNetworkByName(networkName string) (networks.Network, error) {
	opts := networks.ListOpts{
		Name: networkName,
	}

	allPages, err := networks.List(s.client, opts).AllPages()
	if err != nil {
		return networks.Network{}, err
	}

	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return networks.Network{}, err
	}

	switch len(allNetworks) {
	case 0:
		return networks.Network{}, nil
	case 1:
		return allNetworks[0], nil
	}
	return networks.Network{}, errors.New("too many resources")
}
