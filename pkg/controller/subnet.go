package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ipam"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) enqueueAddSubnet(obj any) {
	key := cache.MetaObjectToName(obj.(*kubeovnv1.Subnet)).String()
	klog.V(3).Infof("enqueue add subnet %s", key)
	c.addOrUpdateSubnetQueue.Add(key)
}

func (c *Controller) enqueueDeleteSubnet(obj any) {
	subnet := obj.(*kubeovnv1.Subnet)
	klog.V(3).Infof("enqueue delete subnet %s", subnet.Name)
	c.deleteSubnetQueue.Add(subnet)
}

func (c *Controller) enqueueUpdateSubnet(oldObj, newObj any) {
	var usingIPs float64
	var u2oInterconnIP string
	oldSubnet := oldObj.(*kubeovnv1.Subnet)
	newSubnet := newObj.(*kubeovnv1.Subnet)
	key := cache.MetaObjectToName(newSubnet).String()

	// Trigger network policy refresh only if they are enabled, otherwise the lister will be nil
	if c.npsLister != nil {
		if newSubnet.Spec.Gateway != oldSubnet.Spec.Gateway || newSubnet.Status.U2OInterconnectionIP != oldSubnet.Status.U2OInterconnectionIP {
			policies, err := c.npsLister.List(labels.Everything())
			if err != nil {
				klog.Errorf("failed to list network policies: %v", err)
			} else {
				for _, np := range policies {
					c.enqueueAddNp(np)
				}
			}
		}
	}

	if newSubnet.Spec.Protocol == kubeovnv1.ProtocolIPv6 {
		usingIPs = newSubnet.Status.V6UsingIPs
	} else {
		usingIPs = newSubnet.Status.V4UsingIPs
	}

	u2oInterconnIP = newSubnet.Status.U2OInterconnectionIP
	if !newSubnet.DeletionTimestamp.IsZero() && (usingIPs == 0 || (usingIPs == 1 && u2oInterconnIP != "")) {
		c.addOrUpdateSubnetQueue.Add(key)
		return
	}

	if oldSubnet.Spec.Vpc != newSubnet.Spec.Vpc &&
		((oldSubnet.Spec.Vpc != "" || newSubnet.Spec.Vpc != c.config.ClusterRouter) && (oldSubnet.Spec.Vpc != c.config.ClusterRouter || newSubnet.Spec.Vpc != "")) {
		// recode last vpc name for subnet
		if oldSubnet.Spec.Vpc == "" {
			c.subnetLastVpcNameMap.Store(newSubnet.Name, c.config.ClusterRouter)
		} else {
			c.subnetLastVpcNameMap.Store(newSubnet.Name, oldSubnet.Spec.Vpc)
		}

		c.updateVpcStatusQueue.Add(oldSubnet.Spec.Vpc)
	}

	if oldSubnet.Spec.U2OInterconnection != newSubnet.Spec.U2OInterconnection {
		klog.Infof("enqueue update vpc %s triggered by u2o interconnection change of subnet %s", newSubnet.Spec.Vpc, key)
		c.addOrUpdateVpcQueue.Add(newSubnet.Spec.Vpc)
	}

	if oldSubnet.Spec.Private != newSubnet.Spec.Private ||
		oldSubnet.Spec.CIDRBlock != newSubnet.Spec.CIDRBlock ||
		!slices.Equal(oldSubnet.Spec.AllowSubnets, newSubnet.Spec.AllowSubnets) ||
		!slices.Equal(oldSubnet.Spec.Namespaces, newSubnet.Spec.Namespaces) ||
		oldSubnet.Spec.GatewayType != newSubnet.Spec.GatewayType ||
		oldSubnet.Spec.GatewayNode != newSubnet.Spec.GatewayNode ||
		oldSubnet.Spec.LogicalGateway != newSubnet.Spec.LogicalGateway ||
		oldSubnet.Spec.Gateway != newSubnet.Spec.Gateway ||
		!slices.Equal(oldSubnet.Spec.ExcludeIps, newSubnet.Spec.ExcludeIps) ||
		!slices.Equal(oldSubnet.Spec.Vips, newSubnet.Spec.Vips) ||
		oldSubnet.Spec.Vlan != newSubnet.Spec.Vlan ||
		oldSubnet.Spec.EnableDHCP != newSubnet.Spec.EnableDHCP ||
		oldSubnet.Spec.DHCPv4Options != newSubnet.Spec.DHCPv4Options ||
		oldSubnet.Spec.DHCPv6Options != newSubnet.Spec.DHCPv6Options ||
		oldSubnet.Spec.EnableIPv6RA != newSubnet.Spec.EnableIPv6RA ||
		oldSubnet.Spec.IPv6RAConfigs != newSubnet.Spec.IPv6RAConfigs ||
		oldSubnet.Spec.Protocol != newSubnet.Spec.Protocol ||
		(oldSubnet.Spec.EnableLb == nil && newSubnet.Spec.EnableLb != nil) ||
		(oldSubnet.Spec.EnableLb != nil && newSubnet.Spec.EnableLb == nil) ||
		(oldSubnet.Spec.EnableLb != nil && newSubnet.Spec.EnableLb != nil && *oldSubnet.Spec.EnableLb != *newSubnet.Spec.EnableLb) ||
		oldSubnet.Spec.EnableEcmp != newSubnet.Spec.EnableEcmp ||
		!reflect.DeepEqual(oldSubnet.Spec.Acls, newSubnet.Spec.Acls) ||
		oldSubnet.Spec.U2OInterconnection != newSubnet.Spec.U2OInterconnection ||
		oldSubnet.Spec.RouteTable != newSubnet.Spec.RouteTable ||
		oldSubnet.Spec.Vpc != newSubnet.Spec.Vpc ||
		oldSubnet.Spec.NatOutgoing != newSubnet.Spec.NatOutgoing ||
		oldSubnet.Spec.EnableMulticastSnoop != newSubnet.Spec.EnableMulticastSnoop ||
		!reflect.DeepEqual(oldSubnet.Spec.NatOutgoingPolicyRules, newSubnet.Spec.NatOutgoingPolicyRules) ||
		!reflect.DeepEqual(oldSubnet.Spec.NamespaceSelectors, newSubnet.Spec.NamespaceSelectors) ||
		(newSubnet.Spec.U2OInterconnection && newSubnet.Spec.U2OInterconnectionIP != "" && oldSubnet.Spec.U2OInterconnectionIP != newSubnet.Spec.U2OInterconnectionIP) {
		klog.V(3).Infof("enqueue update subnet %s", key)

		if oldSubnet.Spec.GatewayType != newSubnet.Spec.GatewayType {
			c.recorder.Eventf(newSubnet, v1.EventTypeNormal, "SubnetGatewayTypeChanged",
				"subnet gateway type changes from %q to %q", oldSubnet.Spec.GatewayType, newSubnet.Spec.GatewayType)
		}

		if oldSubnet.Spec.GatewayNode != newSubnet.Spec.GatewayNode {
			c.recorder.Eventf(newSubnet, v1.EventTypeNormal, "SubnetGatewayNodeChanged",
				"gateway node changes from %q to %q", oldSubnet.Spec.GatewayNode, newSubnet.Spec.GatewayNode)
		}

		c.addOrUpdateSubnetQueue.Add(key)
	}
}

func (c *Controller) formatSubnet(subnet *kubeovnv1.Subnet) (*kubeovnv1.Subnet, error) {
	var (
		changed bool
		err     error
	)

	if changed, err = checkSubnetChanged(subnet); err != nil {
		klog.Error(err)
		return nil, err
	}

	if subnet.Spec.Provider == "" {
		subnet.Spec.Provider = util.OvnProvider
		changed = true
	}

	if subnet.Spec.Vpc == "" {
		if isOvnSubnet(subnet) {
			subnet.Spec.Vpc = c.config.ClusterRouter
			changed = true
		}
	}

	if subnet.Spec.Vpc == c.config.ClusterRouter && subnet.Name != c.config.NodeSwitch {
		// Some format only needed in the default VPC
		if subnet.Spec.GatewayType == "" {
			subnet.Spec.GatewayType = kubeovnv1.GWDistributedType
			changed = true
		}
		if subnet.Spec.Default && subnet.Name != c.config.DefaultLogicalSwitch {
			subnet.Spec.Default = false
			changed = true
		}
	}

	if subnet.Spec.EnableLb == nil && subnet.Name != c.config.NodeSwitch {
		changed = true
		subnet.Spec.EnableLb = &c.config.EnableLb
	}
	// set join subnet Spec.EnableLb to nil
	if subnet.Spec.EnableLb != nil && subnet.Name == c.config.NodeSwitch {
		changed = true
		subnet.Spec.EnableLb = nil
	}

	if subnet.Spec.U2OInterconnectionIP != "" && !subnet.Spec.U2OInterconnection {
		subnet.Spec.U2OInterconnectionIP = ""
		changed = true
	}

	if subnet.Spec.Vlan == "" && subnet.Spec.U2OInterconnection {
		subnet.Spec.U2OInterconnection = false
		changed = true
	}

	klog.Infof("format subnet %v, changed %v", subnet.Name, changed)
	if changed {
		newSubnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Update(context.Background(), subnet, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to update subnet %s, %v", subnet.Name, err)
			return nil, err
		}
		return newSubnet, nil
	}
	return subnet, nil
}

func (c *Controller) validateSubnetVlan(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vlan == "" {
		return nil
	}

	vlan, err := c.vlansLister.Get(subnet.Spec.Vlan)
	if err != nil {
		err = fmt.Errorf("failed to get vlan %s: %w", subnet.Spec.Vlan, err)
		klog.Error(err)
		return err
	}

	if vlan.Status.Conflict {
		err = fmt.Errorf("subnet %s has invalid conflict vlan %s", subnet.Name, vlan.Name)
		klog.Error(err)
		return err
	}
	return nil
}

func (c *Controller) updateNatOutgoingPolicyRulesStatus(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.NatOutgoing {
		subnet.Status.NatOutgoingPolicyRules = make([]kubeovnv1.NatOutgoingPolicyRuleStatus, len(subnet.Spec.NatOutgoingPolicyRules))
		for index, rule := range subnet.Spec.NatOutgoingPolicyRules {
			jsonRule, err := json.Marshal(rule)
			if err != nil {
				klog.Error(err)
				return err
			}
			priority := strconv.Itoa(index)
			// hash code generate by subnetName, rule and priority
			var retBytes []byte
			retBytes = append(retBytes, []byte(subnet.Name)...)
			retBytes = append(retBytes, []byte(priority)...)
			retBytes = append(retBytes, jsonRule...)
			result := util.Sha256Hash(retBytes)

			subnet.Status.NatOutgoingPolicyRules[index].RuleID = result[:util.NatPolicyRuleIDLength]
			subnet.Status.NatOutgoingPolicyRules[index].Match = rule.Match
			subnet.Status.NatOutgoingPolicyRules[index].Action = rule.Action
		}
	} else {
		subnet.Status.NatOutgoingPolicyRules = []kubeovnv1.NatOutgoingPolicyRuleStatus{}
	}

	return nil
}

func checkSubnetChanged(subnet *kubeovnv1.Subnet) (bool, error) {
	var (
		changed, ret bool
		err          error
	)

	// changed value may be overlapped, so use ret to record value
	if changed, err = checkAndUpdateCIDR(subnet); err != nil {
		klog.Error(err)
		return changed, err
	}
	if changed {
		ret = true
	}

	if changed, err = checkAndUpdateGateway(subnet); err != nil {
		klog.Error(err)
		return changed, err
	}
	if changed {
		ret = true
	}

	if changed = checkAndUpdateExcludeIPs(subnet); changed {
		ret = true
	}

	if subnet.Spec.Protocol != util.CheckProtocol(subnet.Spec.CIDRBlock) {
		subnet.Spec.Protocol = util.CheckProtocol(subnet.Spec.CIDRBlock)
		ret = true
	}

	return ret, nil
}

func checkAndUpdateCIDR(subnet *kubeovnv1.Subnet) (bool, error) {
	var (
		changed    bool
		cidrBlocks []string
	)

	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			klog.Error(err)
			return false, fmt.Errorf("subnet %s cidr %s is invalid", subnet.Name, cidr)
		}
		if ipNet.String() != cidr {
			changed = true
		}
		cidrBlocks = append(cidrBlocks, ipNet.String())
	}
	subnet.Spec.CIDRBlock = strings.Join(cidrBlocks, ",")
	return changed, nil
}

func checkAndUpdateGateway(subnet *kubeovnv1.Subnet) (bool, error) {
	var (
		changed bool
		gw      string
		err     error
	)

	switch {
	case subnet.Spec.Gateway == "":
		gw, err = util.GetGwByCidr(subnet.Spec.CIDRBlock)
	case subnet.Spec.Protocol == kubeovnv1.ProtocolDual && util.CheckProtocol(subnet.Spec.Gateway) != util.CheckProtocol(subnet.Spec.CIDRBlock):
		gw, err = util.AppendGwByCidr(subnet.Spec.Gateway, subnet.Spec.CIDRBlock)
	default:
		gw = subnet.Spec.Gateway
	}
	if err != nil {
		klog.Error(err)
		return false, err
	}
	if subnet.Spec.Gateway != gw {
		subnet.Spec.Gateway = gw
		changed = true
	}

	return changed, nil
}

// this func must be called after subnet.Spec.Gateway is valued
func checkAndUpdateExcludeIPs(subnet *kubeovnv1.Subnet) bool {
	var (
		changed    bool
		excludeIPs []string
	)
	excludeIPs = append(excludeIPs, strings.Split(subnet.Spec.Gateway, ",")...)
	sort.Strings(excludeIPs)
	if len(subnet.Spec.ExcludeIps) == 0 {
		subnet.Spec.ExcludeIps = excludeIPs
		changed = true
	} else {
		changed = checkAndFormatsExcludeIPs(subnet)
		for _, gw := range excludeIPs {
			gwExists := false
			for _, excludeIP := range subnet.Spec.ExcludeIps {
				if util.ContainsIPs(excludeIP, gw) {
					gwExists = true
					break
				}
			}
			if !gwExists {
				subnet.Spec.ExcludeIps = append(subnet.Spec.ExcludeIps, gw)
				sort.Strings(subnet.Spec.ExcludeIps)
				changed = true
			}
		}
	}
	return changed
}

func (c *Controller) syncSubnetFinalizer(cl client.Client) error {
	// migrate depreciated finalizer to new finalizer
	subnets := &kubeovnv1.SubnetList{}
	return migrateFinalizers(cl, subnets, func(i int) (client.Object, client.Object) {
		if i < 0 || i >= len(subnets.Items) {
			return nil, nil
		}
		return subnets.Items[i].DeepCopy(), subnets.Items[i].DeepCopy()
	})
}

func (c *Controller) handleSubnetFinalizer(subnet *kubeovnv1.Subnet) (*kubeovnv1.Subnet, bool, error) {
	if subnet.DeletionTimestamp.IsZero() && !slices.Contains(subnet.GetFinalizers(), util.KubeOVNControllerFinalizer) {
		newSubnet := subnet.DeepCopy()
		controllerutil.AddFinalizer(newSubnet, util.KubeOVNControllerFinalizer)
		patch, err := util.GenerateMergePatchPayload(subnet, newSubnet)
		if err != nil {
			klog.Errorf("failed to generate patch payload for subnet '%s', %v", subnet.Name, err)
			return newSubnet, false, err
		}
		patchSubnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "")
		if err != nil {
			klog.Errorf("failed to add finalizer to subnet %s, %v", subnet.Name, err)
			return patchSubnet, false, err
		}
		// wait local cache ready
		time.Sleep(1 * time.Second)
		return patchSubnet, false, nil
	}

	usingIPs := subnet.Status.V4UsingIPs
	if util.CheckProtocol(subnet.Spec.CIDRBlock) == kubeovnv1.ProtocolIPv6 {
		usingIPs = subnet.Status.V6UsingIPs
	}

	u2oInterconnIP := subnet.Status.U2OInterconnectionIP
	if !subnet.DeletionTimestamp.IsZero() && (usingIPs == 0 || (usingIPs == 1 && u2oInterconnIP != "")) {
		newSubnet := subnet.DeepCopy()
		controllerutil.RemoveFinalizer(newSubnet, util.KubeOVNControllerFinalizer)
		patch, err := util.GenerateMergePatchPayload(subnet, newSubnet)
		if err != nil {
			klog.Errorf("failed to generate patch payload for subnet '%s', %v", subnet.Name, err)
			return newSubnet, false, err
		}
		if _, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name,
			types.MergePatchType, patch, metav1.PatchOptions{}, ""); err != nil {
			klog.Errorf("failed to remove finalizer from subnet %s, %v", subnet.Name, err)
			return newSubnet, false, err
		}
		return newSubnet, true, nil
	}
	return subnet, false, nil
}

func (c Controller) patchSubnetStatus(subnet *kubeovnv1.Subnet, reason, errStr string) error {
	if errStr != "" {
		subnet.Status.SetError(reason, errStr)
		if reason == "ValidateLogicalSwitchFailed" {
			subnet.Status.NotValidated(reason, errStr)
		} else {
			subnet.Status.Validated(reason, "")
		}
		subnet.Status.NotReady(reason, errStr)
		c.recorder.Eventf(subnet, v1.EventTypeWarning, reason, errStr)
	} else {
		subnet.Status.Validated(reason, "")
		c.recorder.Eventf(subnet, v1.EventTypeNormal, reason, errStr)
		if reason == "SetPrivateLogicalSwitchSuccess" ||
			reason == "ResetLogicalSwitchAclSuccess" ||
			reason == "ReconcileCentralizedGatewaySuccess" ||
			reason == "SetNonOvnSubnetSuccess" {
			subnet.Status.Ready(reason, "")
		}
	}

	bytes, err := subnet.Status.Bytes()
	if err != nil {
		klog.Error(err)
		return err
	}
	if _, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status"); err != nil {
		klog.Errorf("failed to patch status for subnet %s, %v", subnet.Name, err)
		return err
	}
	return nil
}

func (c *Controller) validateVpcBySubnet(subnet *kubeovnv1.Subnet) (*kubeovnv1.Vpc, error) {
	vpc, err := c.vpcsLister.Get(subnet.Spec.Vpc)
	if err != nil {
		klog.Errorf("failed to get subnet's vpc '%s', %v", subnet.Spec.Vpc, err)
		return vpc, err
	}

	if !vpc.Status.Standby {
		err = fmt.Errorf("the vpc '%s' not standby yet", vpc.Name)
		klog.Error(err)
		return vpc, err
	}

	if !vpc.Status.Default {
		for _, ns := range subnet.Spec.Namespaces {
			if !slices.Contains(vpc.Spec.Namespaces, ns) {
				err = fmt.Errorf("namespace '%s' is out of range to custom vpc '%s'", ns, vpc.Name)
				klog.Error(err)
				return vpc, err
			}
		}
	} else {
		vpcs, err := c.vpcsLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list vpc, %v", err)
			return vpc, err
		}
		lastVpcName, _ := c.subnetLastVpcNameMap.Load(subnet.Name)
		for _, vpc := range vpcs {
			if (lastVpcName == "" && subnet.Spec.Vpc != vpc.Name || lastVpcName != "" && lastVpcName != vpc.Name) &&
				!vpc.Status.Default && util.IsStringsOverlap(vpc.Spec.Namespaces, subnet.Spec.Namespaces) {
				err = fmt.Errorf("namespaces %v are overlap with vpc '%s'", subnet.Spec.Namespaces, vpc.Name)
				klog.Error(err)
				return vpc, err
			}
		}
	}
	return vpc, nil
}

func (c *Controller) checkSubnetConflict(subnet *kubeovnv1.Subnet) error {
	subnetList, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnets %v", err)
		return err
	}

	for _, sub := range subnetList {
		if sub.Spec.Vpc != subnet.Spec.Vpc || sub.Spec.Vlan != subnet.Spec.Vlan || sub.Name == subnet.Name {
			continue
		}

		if util.CIDROverlap(sub.Spec.CIDRBlock, subnet.Spec.CIDRBlock) {
			err = fmt.Errorf("subnet %s cidr %s is conflict with subnet %s cidr %s", subnet.Name, subnet.Spec.CIDRBlock, sub.Name, sub.Spec.CIDRBlock)
			klog.Error(err)
			if err = c.patchSubnetStatus(subnet, "ValidateLogicalSwitchFailed", err.Error()); err != nil {
				klog.Error(err)
				return err
			}
			return err
		}

		if subnet.Spec.ExternalEgressGateway != "" && sub.Spec.ExternalEgressGateway != "" &&
			subnet.Spec.PolicyRoutingTableID == sub.Spec.PolicyRoutingTableID {
			err = fmt.Errorf("subnet %s policy routing table ID %d is conflict with subnet %s policy routing table ID %d", subnet.Name, subnet.Spec.PolicyRoutingTableID, sub.Name, sub.Spec.PolicyRoutingTableID)
			klog.Error(err)
			if err = c.patchSubnetStatus(subnet, "ValidateLogicalSwitchFailed", err.Error()); err != nil {
				klog.Error(err)
				return err
			}
			return err
		}
	}

	if subnet.Spec.Vlan == "" && subnet.Spec.Vpc == c.config.ClusterRouter {
		nodes, err := c.nodesLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list nodes: %v", err)
			return err
		}
		for _, node := range nodes {
			for _, addr := range node.Status.Addresses {
				if addr.Type == v1.NodeInternalIP && util.CIDRContainIP(subnet.Spec.CIDRBlock, addr.Address) {
					err = fmt.Errorf("subnet %s cidr %s conflict with node %s address %s", subnet.Name, subnet.Spec.CIDRBlock, node.Name, addr.Address)
					klog.Error(err)
					if err = c.patchSubnetStatus(subnet, "ValidateLogicalSwitchFailed", err.Error()); err != nil {
						klog.Error(err)
						return err
					}
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) updateSubnetDHCPOption(subnet *kubeovnv1.Subnet, needRouter bool) error {
	var mtu int
	if subnet.Spec.Mtu > 0 {
		mtu = int(subnet.Spec.Mtu)
	} else {
		mtu = util.DefaultMTU
		if subnet.Spec.Vlan == "" {
			switch c.config.NetworkType {
			case util.NetworkTypeVlan:
				// default to geneve
				fallthrough
			case util.NetworkTypeGeneve:
				mtu -= util.GeneveHeaderLength
			case util.NetworkTypeVxlan:
				mtu -= util.VxlanHeaderLength
			case util.NetworkTypeStt:
				mtu -= util.SttHeaderLength
			default:
				return fmt.Errorf("invalid network type: %s", c.config.NetworkType)
			}
		}
	}

	dhcpOptionsUUIDs, err := c.OVNNbClient.UpdateDHCPOptions(subnet, mtu)
	if err != nil {
		klog.Errorf("failed to update dhcp options for switch %s, %v", subnet.Name, err)
		return err
	}

	vpc, err := c.vpcsLister.Get(subnet.Spec.Vpc)
	if err != nil {
		klog.Errorf("failed to get subnet's vpc '%s', %v", subnet.Spec.Vpc, err)
		return err
	}

	if needRouter {
		lrpName := fmt.Sprintf("%s-%s", vpc.Status.Router, subnet.Name)
		if err := c.OVNNbClient.UpdateLogicalRouterPortRA(lrpName, subnet.Spec.IPv6RAConfigs, subnet.Spec.EnableIPv6RA); err != nil {
			klog.Errorf("update ipv6 ra configs for logical router port %s, %v", lrpName, err)
			return err
		}
	}

	if subnet.Status.DHCPv4OptionsUUID != dhcpOptionsUUIDs.DHCPv4OptionsUUID || subnet.Status.DHCPv6OptionsUUID != dhcpOptionsUUIDs.DHCPv6OptionsUUID {
		subnet.Status.DHCPv4OptionsUUID = dhcpOptionsUUIDs.DHCPv4OptionsUUID
		subnet.Status.DHCPv6OptionsUUID = dhcpOptionsUUIDs.DHCPv6OptionsUUID
		bytes, err := subnet.Status.Bytes()
		if err != nil {
			klog.Error(err)
			return err
		}
		if _, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status"); err != nil {
			klog.Errorf("patch subnet %s dhcp options failed: %v", subnet.Name, err)
			return err
		}
	}

	return nil
}

func (c *Controller) handleAddOrUpdateSubnet(key string) error {
	c.subnetKeyMutex.LockKey(key)
	defer func() { _ = c.subnetKeyMutex.UnlockKey(key) }()

	cachedSubnet, err := c.subnetsLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Error(err)
		return err
	}
	klog.V(3).Infof("handle add or update subnet %s", cachedSubnet.Name)
	subnet := cachedSubnet.DeepCopy()
	subnet, err = c.formatSubnet(subnet)
	if err != nil {
		err := fmt.Errorf("failed to format subnet %s, %w", key, err)
		klog.Error(err)
		return err
	}

	err = c.validateSubnetVlan(subnet)
	if err != nil {
		err := fmt.Errorf("failed to validate vlan for subnet %s, %w", key, err)
		klog.Error(err)
		if err = c.patchSubnetStatus(subnet, "ValidateSubnetVlanFailed", err.Error()); err != nil {
			klog.Error(err)
			return err
		}
		return err
	}

	if err = util.ValidateSubnet(*subnet); err != nil {
		klog.Errorf("failed to validate subnet %s, %v", subnet.Name, err)
		if err = c.patchSubnetStatus(subnet, "ValidateLogicalSwitchFailed", err.Error()); err != nil {
			klog.Error(err)
			return err
		}
		return err
	}
	if err = c.patchSubnetStatus(subnet, "ValidateLogicalSwitchSuccess", ""); err != nil {
		klog.Error(err)
		return err
	}

	if err := c.ipam.AddOrUpdateSubnet(subnet.Name, subnet.Spec.CIDRBlock, subnet.Spec.Gateway, subnet.Spec.ExcludeIps); err != nil {
		klog.Error(err)
		return err
	}

	// availableIPStr valued from ipam, so leave update subnet.status after ipam process
	if subnet.Spec.Protocol == kubeovnv1.ProtocolDual {
		subnet, err = c.calcDualSubnetStatusIP(subnet)
	} else {
		subnet, err = c.calcSubnetStatusIP(subnet)
	}
	if err != nil {
		klog.Errorf("calculate subnet %s used ip failed, %v", cachedSubnet.Name, err)
		return err
	}

	subnet, deleted, err := c.handleSubnetFinalizer(subnet)
	if err != nil {
		klog.Errorf("handle subnet finalizer failed %v", err)
		return err
	}
	if deleted {
		return nil
	}

	if !isOvnSubnet(subnet) {
		// subnet provider is not ovn, and vpc is empty, should not reconcile
		if err = c.patchSubnetStatus(subnet, "SetNonOvnSubnetSuccess", ""); err != nil {
			klog.Error(err)
			return err
		}

		subnet.Status.EnsureStandardConditions()
		klog.Infof("non ovn subnet %s is ready", subnet.Name)
		return nil
	}

	// This validate should be processed after isOvnSubnet, since maybe there's no vpc for subnet not managed by kube-ovn
	vpc, err := c.validateVpcBySubnet(subnet)
	if err != nil {
		klog.Errorf("failed to get subnet's vpc '%s', %v", subnet.Spec.Vpc, err)
		return err
	}
	_, isMcastQuerierChanged, err := c.reconcileSubnetSpecialIPs(subnet)
	if err != nil {
		klog.Errorf("failed to reconcile subnet %s Custom IPs %v", subnet.Name, err)
		return err
	}
	if err := c.checkSubnetConflict(subnet); err != nil {
		klog.Errorf("failed to check subnet %s, %v", subnet.Name, err)
		return err
	}

	needRouter := subnet.Spec.Vlan == "" || subnet.Spec.LogicalGateway ||
		(subnet.Status.U2OInterconnectionIP != "" && subnet.Spec.U2OInterconnection)
	// 1. overlay subnet, should add lrp, lrp ip is subnet gw
	// 2. underlay subnet use logical gw, should add lrp, lrp ip is subnet gw
	randomAllocateGW := !subnet.Spec.LogicalGateway && vpc.Spec.EnableExternal && subnet.Name == c.config.ExternalGatewaySwitch
	// 3. underlay subnet use physical gw, vpc has eip, lrp managed in vpc process, lrp ip is random allocation, not subnet gw

	gateway := subnet.Spec.Gateway
	var gatewayMAC string
	if subnet.Status.U2OInterconnectionIP != "" && subnet.Spec.U2OInterconnection {
		gateway = subnet.Status.U2OInterconnectionIP
		gatewayMAC = subnet.Status.U2OInterconnectionMAC
	}

	if err := c.clearOldU2OResource(subnet); err != nil {
		klog.Errorf("clear subnet %s old u2o resource failed: %v", subnet.Name, err)
		return err
	}

	// create or update logical switch
	if err := c.OVNNbClient.CreateLogicalSwitch(subnet.Name, vpc.Status.Router, subnet.Spec.CIDRBlock, gateway, gatewayMAC, needRouter, randomAllocateGW); err != nil {
		klog.Errorf("create logical switch %s: %v", subnet.Name, err)
		return err
	}

	if isMcastQuerierChanged {
		if err := c.handleMcastQuerierChange(subnet); err != nil {
			klog.Errorf("failed to handle mcast querier IP change for subnet %s: %v", subnet.Name, err)
			return err
		}
	}

	subnet.Status.EnsureStandardConditions()

	if err := c.updateSubnetDHCPOption(subnet, needRouter); err != nil {
		klog.Errorf("failed to update subnet %s dhcpOptions: %v", subnet.Name, err)
		return err
	}

	if c.config.EnableLb && subnet.Name != c.config.NodeSwitch {
		lbs := []string{
			vpc.Status.TCPLoadBalancer,
			vpc.Status.TCPSessionLoadBalancer,
			vpc.Status.UDPLoadBalancer,
			vpc.Status.UDPSessionLoadBalancer,
			vpc.Status.SctpLoadBalancer,
			vpc.Status.SctpSessionLoadBalancer,
		}
		if subnet.Spec.EnableLb != nil && *subnet.Spec.EnableLb {
			if err := c.OVNNbClient.LogicalSwitchUpdateLoadBalancers(subnet.Name, ovsdb.MutateOperationInsert, lbs...); err != nil {
				if err = c.patchSubnetStatus(subnet, "AddLbToLogicalSwitchFailed", err.Error()); err != nil {
					klog.Error(err)
					return err
				}
				klog.Error(err)
				return err
			}
		} else {
			if err := c.OVNNbClient.LogicalSwitchUpdateLoadBalancers(subnet.Name, ovsdb.MutateOperationDelete, lbs...); err != nil {
				klog.Errorf("remove load-balancer from subnet %s failed: %v", subnet.Name, err)
				return err
			}
		}
	}

	if err := c.reconcileSubnet(subnet); err != nil {
		klog.Errorf("reconcile subnet for %s failed, %v", subnet.Name, err)
		return err
	}

	subnet.Status.U2OInterconnectionVPC = ""
	if subnet.Spec.U2OInterconnection {
		subnet.Status.U2OInterconnectionVPC = vpc.Status.Router
	}

	if err = c.updateNatOutgoingPolicyRulesStatus(subnet); err != nil {
		klog.Errorf("failed to update NAT outgoing policy status for subnet %s: %v", subnet.Name, err)
		return err
	}

	if subnet.Spec.Private {
		if err := c.OVNNbClient.SetLogicalSwitchPrivate(subnet.Name, subnet.Spec.CIDRBlock, c.config.NodeSwitchCIDR, subnet.Spec.AllowSubnets); err != nil {
			if err = c.patchSubnetStatus(subnet, "SetPrivateLogicalSwitchFailed", err.Error()); err != nil {
				klog.Error(err)
				return err
			}
			klog.Error(err)
			return err
		}

		if err = c.patchSubnetStatus(subnet, "SetPrivateLogicalSwitchSuccess", ""); err != nil {
			klog.Error(err)
			return err
		}
	} else {
		// clear acl when direction is ""
		if err = c.OVNNbClient.DeleteAcls(subnet.Name, logicalSwitchKey, "", nil); err != nil {
			if err = c.patchSubnetStatus(subnet, "ResetLogicalSwitchAclFailed", err.Error()); err != nil {
				klog.Error(err)
				return err
			}
			klog.Error(err)
			return err
		}

		if err = c.patchSubnetStatus(subnet, "ResetLogicalSwitchAclSuccess", ""); err != nil {
			klog.Error(err)
			return err
		}
	}

	if err := c.OVNNbClient.UpdateLogicalSwitchACL(subnet.Name, subnet.Spec.CIDRBlock, subnet.Spec.Acls, subnet.Spec.AllowEWTraffic); err != nil {
		if err = c.patchSubnetStatus(subnet, "SetLogicalSwitchAclsFailed", err.Error()); err != nil {
			klog.Error(err)
			return err
		}
		klog.Error(err)
		return err
	}

	c.updateVpcStatusQueue.Add(subnet.Spec.Vpc)

	ippools, err := c.ippoolLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list ippools: %v", err)
		return err
	}

	for _, p := range ippools {
		if p.Spec.Subnet == subnet.Name {
			c.addOrUpdateIPPoolQueue.Add(p.Name)
		}
	}

	return nil
}

func (c *Controller) handleUpdateSubnetStatus(key string) error {
	c.subnetKeyMutex.LockKey(key)
	defer func() { _ = c.subnetKeyMutex.UnlockKey(key) }()

	cachedSubnet, err := c.subnetsLister.Get(key)
	subnet := cachedSubnet.DeepCopy()
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Error(err)
		return err
	}

	ippools, err := c.ippoolLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list ippool: %v", err)
		return err
	}
	for _, p := range ippools {
		if p.Spec.Subnet == subnet.Name {
			c.updateIPPoolStatusQueue.Add(p.Name)
		}
	}

	if util.CheckProtocol(subnet.Spec.CIDRBlock) == kubeovnv1.ProtocolDual {
		if _, err := c.calcDualSubnetStatusIP(subnet); err != nil {
			klog.Error(err)
			return err
		}
	} else {
		if _, err = c.calcSubnetStatusIP(subnet); err != nil {
			klog.Error(err)
			return err
		}
	}

	if err := c.checkSubnetUsingIPs(subnet); err != nil {
		klog.Errorf("inconsistency detected in status of subnet %s : %v", subnet.Name, err)
		return err
	}
	return nil
}

func (c *Controller) handleDeleteLogicalSwitch(key string) (err error) {
	c.ipam.DeleteSubnet(key)

	exist, err := c.OVNNbClient.LogicalSwitchExists(key)
	if err != nil {
		klog.Errorf("check logical switch %s exist: %v", key, err)
		return err
	}

	// not found, skip
	if !exist {
		return nil
	}

	// clear acl when direction is ""
	if err = c.OVNNbClient.DeleteAcls(key, logicalSwitchKey, "", nil); err != nil {
		klog.Errorf("clear logical switch %s acls: %v", key, err)
		return err
	}

	if err = c.OVNNbClient.DeleteDHCPOptions(key, kubeovnv1.ProtocolDual); err != nil {
		klog.Errorf("failed to delete dhcp options of logical switch %s %v", key, err)
		return err
	}

	if err = c.OVNNbClient.DeleteLogicalSwitch(key); err != nil {
		klog.Errorf("delete logical switch %s: %v", key, err)
		return err
	}

	nss, err := c.namespacesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list namespaces, %v", err)
		return err
	}

	// re-annotate namespace
	for _, ns := range nss {
		annotations := ns.GetAnnotations()
		if annotations == nil {
			continue
		}

		if slices.Contains(strings.Split(annotations[util.LogicalSwitchAnnotation], ","), key) {
			c.enqueueAddNamespace(ns)
		}
	}

	return c.delLocalnet(key)
}

func (c *Controller) handleDeleteSubnet(subnet *kubeovnv1.Subnet) error {
	c.subnetKeyMutex.LockKey(subnet.Name)
	defer func() { _ = c.subnetKeyMutex.UnlockKey(subnet.Name) }()

	c.updateVpcStatusQueue.Add(subnet.Spec.Vpc)
	klog.Infof("delete u2o interconnection policy route for subnet %s", subnet.Name)
	if err := c.deletePolicyRouteForU2OInterconn(subnet); err != nil {
		klog.Errorf("failed to delete policy route for underlay to overlay subnet interconnection %s, %v", subnet.Name, err)
		return err
	}
	if subnet.Spec.Vpc != c.config.ClusterRouter {
		if err := c.deleteStaticRouteForU2OInterconn(subnet); err != nil {
			klog.Errorf("failed to delete static route for underlay to overlay subnet interconnection %s, %v", subnet.Name, err)
			return err
		}
	}

	u2oInterconnName := fmt.Sprintf(util.U2OInterconnName, subnet.Spec.Vpc, subnet.Name)
	if err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), u2oInterconnName, metav1.DeleteOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			klog.Errorf("failed to delete ip %s, %v", u2oInterconnName, err)
			return err
		}
	}

	if subnet.Spec.Vpc != c.config.ClusterRouter {
		if err := c.deleteCustomVPCPolicyRoutesForSubnet(subnet); err != nil {
			klog.Errorf("failed to delete custom vpc routes subnet %s, %v", subnet.Name, err)
			return err
		}
	}

	klog.Infof("delete policy route for %s subnet %s", subnet.Spec.GatewayType, subnet.Name)
	if err := c.deletePolicyRouteByGatewayType(subnet, subnet.Spec.GatewayType, true); err != nil {
		klog.Errorf("failed to delete policy route for overlay subnet %s, %v", subnet.Name, err)
		return err
	}

	err := c.handleDeleteLogicalSwitch(subnet.Name)
	if err != nil {
		klog.Errorf("failed to delete logical switch %s %v", subnet.Name, err)
		return err
	}

	var router string
	vpc, err := c.vpcsLister.Get(subnet.Spec.Vpc)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			klog.Errorf("get vpc %s: %v", vpc.Name, err)
			return err
		}
		router = c.config.ClusterRouter
	} else {
		router = vpc.Status.Router
	}

	lspName := fmt.Sprintf("%s-%s", subnet.Name, router)
	lrpName := fmt.Sprintf("%s-%s", router, subnet.Name)
	if err = c.OVNNbClient.RemoveLogicalPatchPort(lspName, lrpName); err != nil {
		klog.Errorf("delete router port %s and %s:%v", lspName, lrpName, err)
		return err
	}

	vlans, err := c.vlansLister.List(labels.Everything())
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("failed to list vlans: %v", err)
		return err
	}

	for _, vlan := range vlans {
		if err = c.updateVlanStatusForSubnetDeletion(vlan, subnet.Name); err != nil {
			klog.Error(err)
			return err
		}
	}

	// clean up subnet last vpc name cached
	c.subnetLastVpcNameMap.Delete(subnet.Name)

	return nil
}

func (c *Controller) updateVlanStatusForSubnetDeletion(vlan *kubeovnv1.Vlan, subnet string) error {
	if !slices.Contains(vlan.Status.Subnets, subnet) {
		return nil
	}

	newVlan := vlan.DeepCopy()
	newVlan.Status.Subnets = util.RemoveString(newVlan.Status.Subnets, subnet)
	_, err := c.config.KubeOvnClient.KubeovnV1().Vlans().UpdateStatus(context.Background(), newVlan, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("failed to update status of vlan %s: %v", vlan.Name, err)
		return err
	}

	return nil
}

func (c *Controller) reconcileSubnet(subnet *kubeovnv1.Subnet) error {
	if err := c.reconcileNamespaces(subnet); err != nil {
		klog.Errorf("reconcile namespaces for subnet %s failed, %v", subnet.Name, err)
		return err
	}

	if err := c.reconcileRouteTableForSubnet(subnet); err != nil {
		klog.Errorf("reconcile route table for subnet %s failed, %v", subnet.Name, err)
		return err
	}

	if subnet.Spec.Vpc == c.config.ClusterRouter {
		if err := c.reconcileOvnDefaultVpcRoute(subnet); err != nil {
			klog.Errorf("reconcile default vpc ovn route for subnet %s failed: %v", subnet.Name, err)
			return err
		}
	} else if err := c.reconcileCustomVpcStaticRoute(subnet); err != nil {
		klog.Errorf("reconcile custom vpc ovn route for subnet %s failed: %v", subnet.Name, err)
		return err
	}

	if err := c.reconcileVlan(subnet); err != nil {
		klog.Errorf("reconcile vlan for subnet %s failed, %v", subnet.Name, err)
		return err
	}

	if err := c.reconcileVips(subnet); err != nil {
		klog.Errorf("reconcile vips for subnet %s failed, %v", subnet.Name, err)
		return err
	}
	return nil
}

func (c *Controller) reconcileVips(subnet *kubeovnv1.Subnet) error {
	/* get all virtual port belongs to this logical switch */
	lsps, err := c.OVNNbClient.ListLogicalSwitchPorts(true, map[string]string{logicalSwitchKey: subnet.Name}, func(lsp *ovnnb.LogicalSwitchPort) bool {
		return lsp.Type == "virtual"
	})
	if err != nil {
		klog.Errorf("failed to find virtual port for subnet %s: %v", subnet.Name, err)
		return err
	}

	/* filter all invalid virtual port */
	existVips := make(map[string]string) // key is vip, value is port name
	for _, lsp := range lsps {
		vip, ok := lsp.Options["virtual-ip"]
		if !ok {
			continue // ignore vip which is empty
		}

		if net.ParseIP(vip) == nil {
			continue // ignore invalid vip
		}

		existVips[vip] = lsp.Name
	}

	/* filter virtual port to be added and old virtual port to be deleted */
	var newVips []string
	for _, vip := range subnet.Spec.Vips {
		if _, ok := existVips[vip]; !ok {
			// new virtual port to be added
			newVips = append(newVips, vip)
		} else {
			// delete old virtual port that do not need to be deleted
			delete(existVips, vip)
		}
	}

	// delete old virtual ports
	for _, lspName := range existVips {
		if err = c.OVNNbClient.DeleteLogicalSwitchPort(lspName); err != nil {
			klog.Errorf("delete virtual port %s lspName from logical switch %s: %v", lspName, subnet.Name, err)
			return err
		}
	}

	// add new virtual port
	if err = c.OVNNbClient.CreateVirtualLogicalSwitchPorts(subnet.Name, newVips...); err != nil {
		klog.Errorf("create virtual port with vips %v from logical switch %s: %v", newVips, subnet.Name, err)
		return err
	}

	c.syncVirtualPortsQueue.Add(subnet.Name)
	return nil
}

func (c *Controller) syncVirtualPort(key string) error {
	subnet, err := c.subnetsLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Errorf("failed to get subnet %s, %v", key, err)
		return err
	}
	if len(subnet.Spec.Vips) == 0 {
		return nil
	}

	externalIDs := map[string]string{
		logicalSwitchKey: subnet.Name,
		"attach-vips":    "true",
	}

	lsps, err := c.OVNNbClient.ListNormalLogicalSwitchPorts(true, externalIDs)
	if err != nil {
		klog.Errorf("list logical switch %s ports: %v", subnet.Name, err)
		return err
	}

	for _, vip := range subnet.Spec.Vips {
		if !util.CIDRContainIP(subnet.Spec.CIDRBlock, vip) {
			klog.Errorf("vip %s is out of range to subnet %s", vip, subnet.Name)
			continue
		}

		var virtualParents []string
		for _, lsp := range lsps {
			vips, ok := lsp.ExternalIDs["vips"]
			if !ok {
				continue // ignore vips which is empty
			}

			if strings.Contains(vips, vip) {
				virtualParents = append(virtualParents, lsp.Name)
			}
		}

		// logical switch port has no valid vip
		if len(virtualParents) == 0 {
			continue
		}

		if err = c.OVNNbClient.SetLogicalSwitchPortVirtualParents(subnet.Name, strings.Join(virtualParents, ","), vip); err != nil {
			klog.Errorf("set vip %s virtual parents %v: %v", vip, virtualParents, err)
			return err
		}
	}

	return nil
}

func (c *Controller) reconcileNamespaces(subnet *kubeovnv1.Subnet) error {
	var (
		namespaces []*v1.Namespace
		err        error
	)

	// 1. get all namespaces should be updated
	expectNss, err := c.getNamespacesBySelector(subnet.Spec.NamespaceSelectors)
	if err != nil {
		klog.Errorf("failed to list namespaces by selector, %v", err)
		return err
	}
	for _, ns := range subnet.Spec.Namespaces {
		if !slices.Contains(expectNss, ns) {
			expectNss = append(expectNss, ns)
		}
	}

	// 2. update namespaces
	for _, expectNs := range expectNss {
		checkNs, err := c.namespacesLister.Get(expectNs)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			klog.Error(err)
			return err
		}
		if checkNs.Annotations != nil && slices.Contains(strings.Split(checkNs.Annotations[util.LogicalSwitchAnnotation], ","), subnet.Name) {
			// when subnet cidr changed, the ns annotation with the subnet should be updated
			if !slices.Contains(strings.Split(checkNs.Annotations[util.CidrAnnotation], ";"), subnet.Spec.CIDRBlock) {
				c.addNamespaceQueue.Add(checkNs.Name)
			}
			continue
		}
		c.addNamespaceQueue.Add(expectNs)
	}

	// 3. update unbind namespace annotation
	if namespaces, err = c.namespacesLister.List(labels.Everything()); err != nil {
		klog.Errorf("failed to list namespaces, %v", err)
		return err
	}

	for _, ns := range namespaces {
		if ns.Annotations != nil && slices.Contains(strings.Split(ns.Annotations[util.LogicalSwitchAnnotation], ","), subnet.Name) {
			if slices.Contains(expectNss, ns.Name) {
				continue
			}
			// ns deleted from subnet.Spec.Namespaces or subnet delete namespaceSelectors which match the checked namespace
			c.addNamespaceQueue.Add(ns.Name)
		}
	}

	return nil
}

func (c *Controller) getNamespacesBySelector(nsSelectors []metav1.LabelSelector) ([]string, error) {
	var expectNss []string
	for _, nsSelector := range nsSelectors {
		matchSelector, err := metav1.LabelSelectorAsSelector(&nsSelector)
		if err != nil {
			klog.Errorf("failed to convert label selector, %v", err)
			return expectNss, err
		}

		nss, err := c.namespacesLister.List(matchSelector)
		if err != nil {
			klog.Errorf("failed to list namespaces by selector, %v", err)
			return expectNss, err
		}
		for _, ns := range nss {
			expectNss = append(expectNss, ns.Name)
		}
	}
	return expectNss, nil
}

func (c *Controller) reconcileCustomVpcBfdStaticRoute(vpcName, subnetName string) error {
	// vpc enable bfd and subnet enable ecmp
	// use static ecmp route with bfd
	ovnEips, err := c.ovnEipsLister.List(labels.SelectorFromSet(labels.Set{util.OvnEipTypeLabel: util.OvnEipTypeLSP}))
	if err != nil {
		klog.Errorf("failed to list node external ovn eip, %v", err)
		return err
	}
	if len(ovnEips) < 2 {
		err := fmt.Errorf("ecmp route with bfd for HA, which need two %s type eips at least, has %d", util.OvnEipTypeLSP, len(ovnEips))
		klog.Error(err)
		return err
	}

	subnet, err := c.subnetsLister.Get(subnetName)
	if err != nil {
		klog.Errorf("failed to get subnet %s, %v", subnetName, err)
		return err
	}
	cachedVpc, err := c.vpcsLister.Get(vpcName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Errorf("failed to get vpc %s, %v", vpcName, err)
		return err
	}

	var (
		needUpdate, v4Exist bool
		lrpEipName          string
	)

	lrpEipName = fmt.Sprintf("%s-%s", vpcName, c.config.ExternalGatewaySwitch)
	lrpEip, err := c.ovnEipsLister.Get(lrpEipName)
	if err != nil {
		err := fmt.Errorf("failed to get lrp eip %s, %w", lrpEipName, err)
		klog.Error(err)
		return err
	}
	if !lrpEip.Status.Ready || lrpEip.Status.V4Ip == "" {
		err := fmt.Errorf("lrp eip %q not ready", lrpEipName)
		klog.Error(err)
		return err
	}
	vpc := cachedVpc.DeepCopy()

	for _, eip := range ovnEips {
		if !eip.Status.Ready || eip.Status.V4Ip == "" {
			err := fmt.Errorf("ovn eip %q not ready", eip.Name)
			klog.Error(err)
			return err
		}
		bfd, err := c.OVNNbClient.CreateBFD(lrpEipName, eip.Status.V4Ip, c.config.BfdMinRx, c.config.BfdMinTx, c.config.BfdDetectMult, nil)
		if err != nil {
			klog.Error(err)
			return err
		}
		// TODO:// support v6
		v4Exist = false
		for _, route := range vpc.Spec.StaticRoutes {
			if route.Policy == kubeovnv1.PolicySrc &&
				route.NextHopIP == eip.Status.V4Ip &&
				route.ECMPMode == util.StaticRouteBfdEcmp &&
				route.CIDR == subnet.Spec.CIDRBlock &&
				route.RouteTable == subnet.Spec.RouteTable {
				v4Exist = true
				break
			}
		}
		if !v4Exist {
			// add ecmp type static route with bfd
			route := &kubeovnv1.StaticRoute{
				Policy:     kubeovnv1.PolicySrc,
				CIDR:       subnet.Spec.CIDRBlock,
				NextHopIP:  eip.Status.V4Ip,
				ECMPMode:   util.StaticRouteBfdEcmp,
				BfdID:      bfd.UUID,
				RouteTable: subnet.Spec.RouteTable,
			}
			klog.Infof("add ecmp bfd static route %v", route)
			vpc.Spec.StaticRoutes = append(vpc.Spec.StaticRoutes, route)
			needUpdate = true
		}
	}
	if needUpdate {
		if _, err = c.config.KubeOvnClient.KubeovnV1().Vpcs().Update(context.Background(), vpc, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("failed to update vpc spec static route %s, %v", vpc.Name, err)
			return err
		}
		if err = c.patchVpcBfdStatus(vpc.Name); err != nil {
			klog.Errorf("failed to patch vpc %s, %v", vpc.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) reconcileCustomVpcDelNormalStaticRoute(vpcName string) error {
	// normal static route is prior than ecmp bfd static route
	// if use ecmp bfd static route, normal static route should not exist
	defaultExternalSubnet, err := c.subnetsLister.Get(c.config.ExternalGatewaySwitch)
	if err != nil {
		klog.Errorf("failed to get default external switch subnet %s: %v", c.config.ExternalGatewaySwitch, err)
		return err
	}
	gatewayV4, gatewayV6 := util.SplitStringIP(defaultExternalSubnet.Spec.Gateway)
	needUpdate := false
	vpc, err := c.vpcsLister.Get(vpcName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Errorf("failed to get vpc %s, %v", vpcName, err)
		return err
	}
	routeTotal := len(vpc.Spec.StaticRoutes)
	routes := make([]*kubeovnv1.StaticRoute, 0, routeTotal)
	for _, route := range vpc.Spec.StaticRoutes {
		if route.Policy == kubeovnv1.PolicyDst &&
			(route.NextHopIP == gatewayV4 || route.NextHopIP == gatewayV6) &&
			(route.CIDR == "0.0.0.0/0" || route.CIDR == "::/0") {
			klog.Infof("in order to use ecmp bfd route, need remove normal static route %v", route)
			needUpdate = true
		} else {
			routes = append(routes, route)
		}
	}

	if needUpdate {
		vpc.Spec.StaticRoutes = routes
		if _, err = c.config.KubeOvnClient.KubeovnV1().Vpcs().Update(context.Background(), vpc, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("failed to update vpc spec static route %s, %v", vpc.Name, err)
			return err
		}
	}

	if err = c.patchVpcBfdStatus(vpc.Name); err != nil {
		klog.Errorf("failed to patch vpc %s, %v", vpc.Name, err)
		return err
	}

	return nil
}

func (c *Controller) reconcileDistributedSubnetRouteInDefaultVpc(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.GatewayNode != "" || subnet.Status.ActivateGateway != "" {
		klog.Infof("delete old centralized policy route for subnet %s", subnet.Name)
		if err := c.deletePolicyRouteForCentralizedSubnet(subnet); err != nil {
			klog.Errorf("failed to delete policy route for subnet %s, %v", subnet.Name, err)
			return err
		}

		subnet.Spec.GatewayNode = ""
		if _, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Update(context.Background(), subnet, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("failed to remove gatewayNode or activateGateway from subnet %s, %v", subnet.Name, err)
			return err
		}
		subnet.Status.ActivateGateway = ""
		if err := c.patchSubnetStatus(subnet, "ChangeToDistributedGw", ""); err != nil {
			klog.Error(err)
			return err
		}
	}

	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}
	for _, node := range nodes {
		if err = c.createPortGroupForDistributedSubnet(node, subnet); err != nil {
			klog.Errorf("failed to create port group %v", err)
			return err
		}
		if node.Annotations[util.AllocatedAnnotation] != "true" {
			klog.Warningf("node %s has not been successfully initialized, skip adding policy route for subnet %s", node.Name, subnet.Name)
			continue
		}
		nodeIP, err := getNodeTunlIP(node)
		if err != nil {
			klog.Errorf("failed to get node %s tunnel ip, %v", node.Name, err)
			return err
		}
		nextHop := getNextHopByTunnelIP(nodeIP)
		v4IP, v6IP := util.SplitStringIP(nextHop)
		if err = c.addPolicyRouteForDistributedSubnet(subnet, node.Name, v4IP, v6IP); err != nil {
			klog.Errorf("failed to add policy router for node %s and subnet %s: %v", node.Name, subnet.Name, err)
			return err
		}
	}

	portGroups, err := c.OVNNbClient.ListPortGroups(map[string]string{"subnet": subnet.Name, "node": "", networkPolicyKey: ""})
	if err != nil {
		klog.Errorf("failed to list port groups for subnet %s: %v", subnet.Name, err)
		return err
	}

	pods, err := c.podsLister.Pods(metav1.NamespaceAll).List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list pods %v", err)
		return err
	}
	for _, pod := range pods {
		if !isPodAlive(pod) || pod.Spec.NodeName == "" {
			continue
		}

		podNets, err := c.getPodKubeovnNets(pod)
		if err != nil {
			klog.Errorf("failed to get pod nets %v", err)
			continue
		}

		podPorts := make([]string, 0, 1)
		for _, podNet := range podNets {
			if !isOvnSubnet(podNet.Subnet) {
				continue
			}

			if pod.Annotations[fmt.Sprintf(util.IPAddressAnnotationTemplate, podNet.ProviderName)] == "" || pod.Annotations[fmt.Sprintf(util.LogicalSwitchAnnotationTemplate, podNet.ProviderName)] != subnet.Name {
				continue
			}

			podName := c.getNameByPod(pod)
			portName := ovs.PodNameToPortName(podName, pod.Namespace, podNet.ProviderName)
			podPorts = append(podPorts, portName)
		}

		pgName := getOverlaySubnetsPortGroupName(subnet.Name, pod.Spec.NodeName)
		portsToAdd := make([]string, 0, len(podPorts))
		for _, port := range podPorts {
			exist, err := c.OVNNbClient.LogicalSwitchPortExists(port)
			if err != nil {
				klog.Error(err)
				return err
			}

			if !exist {
				klog.Errorf("lsp does not exist for pod %v, please delete the pod and retry", port)
				continue
			}

			portsToAdd = append(portsToAdd, port)
		}

		// remove lsp from other port groups
		// we need to do this because the pod, e.g. a sts/vm, can be rescheduled to another node
		for _, pg := range portGroups {
			if pg.Name == pgName {
				continue
			}
			if err = c.OVNNbClient.PortGroupRemovePorts(pg.Name, podPorts...); err != nil {
				klog.Errorf("remove ports from port group %s: %v", pg.Name, err)
				return err
			}
		}
		// add ports to the port group
		if err = c.OVNNbClient.PortGroupAddPorts(pgName, portsToAdd...); err != nil {
			klog.Errorf("add ports to port group %s: %v", pgName, err)
			return err
		}
	}
	return nil
}

func (c *Controller) reconcileDefaultCentralizedSubnetRouteInDefaultVpc(subnet *kubeovnv1.Subnet) error {
	// check if activateGateway still ready
	if subnet.Status.ActivateGateway != "" && util.GatewayContains(subnet.Spec.GatewayNode, subnet.Status.ActivateGateway) {
		node, err := c.nodesLister.Get(subnet.Status.ActivateGateway)
		if err == nil && nodeReady(node) {
			klog.Infof("subnet %s uses the old activate gw %s", subnet.Name, node.Name)

			nodeTunlIPAddr, err := getNodeTunlIP(node)
			if err != nil {
				klog.Errorf("failed to get gatewayNode tunnel ip for subnet %s", subnet.Name)
				return err
			}
			nextHop := getNextHopByTunnelIP(nodeTunlIPAddr)
			if err = c.addPolicyRouteForCentralizedSubnet(subnet, subnet.Status.ActivateGateway, nil, strings.Split(nextHop, ",")); err != nil {
				klog.Errorf("failed to add active-backup policy route for centralized subnet %s: %v", subnet.Name, err)
				return err
			}
			return nil
		}
	}

	klog.Info("find a new activate node")
	// need a new activate gateway
	newActivateNode := ""
	var nodeTunlIPAddr []net.IP
	for gw := range strings.SplitSeq(subnet.Spec.GatewayNode, ",") {
		// the format of gatewayNodeStr can be like 'kube-ovn-worker:172.18.0.2, kube-ovn-control-plane:172.18.0.3', which consists of node name and designative egress ip
		if strings.Contains(gw, ":") {
			gw = strings.TrimSpace(strings.Split(gw, ":")[0])
		} else {
			gw = strings.TrimSpace(gw)
		}
		node, err := c.nodesLister.Get(gw)
		if err == nil && nodeReady(node) {
			newActivateNode = node.Name
			nodeTunlIPAddr, err = getNodeTunlIP(node)
			if err != nil {
				klog.Error(err)
				return err
			}
			klog.Infof("subnet %s uses a new activate gw %s", subnet.Name, node.Name)
			break
		}
	}
	if newActivateNode == "" {
		klog.Warningf("all gateways of subnet %s are not ready", subnet.Name)
		subnet.Status.ActivateGateway = newActivateNode
		if err := c.patchSubnetStatus(subnet, "NoActiveGatewayFound", fmt.Sprintf("subnet %s gws are not ready", subnet.Name)); err != nil {
			klog.Error(err)
			return err
		}

		return fmt.Errorf("subnet %s gws are not ready", subnet.Name)
	}

	nextHop := getNextHopByTunnelIP(nodeTunlIPAddr)
	klog.Infof("subnet %s configure new gateway node, nextHop %s", subnet.Name, nextHop)
	if err := c.addPolicyRouteForCentralizedSubnet(subnet, newActivateNode, nil, strings.Split(nextHop, ",")); err != nil {
		klog.Errorf("failed to add policy route for active-backup centralized subnet %s: %v", subnet.Name, err)
		return err
	}
	subnet.Status.ActivateGateway = newActivateNode
	if err := c.patchSubnetStatus(subnet, "ReconcileCentralizedGatewaySuccess", ""); err != nil {
		klog.Error(err)
		return err
	}

	klog.Infof("delete old distributed policy route for subnet %s", subnet.Name)
	if err := c.deletePolicyRouteByGatewayType(subnet, kubeovnv1.GWDistributedType, false); err != nil {
		klog.Errorf("failed to delete policy route for overlay subnet %s, %v", subnet.Name, err)
		return err
	}
	return nil
}

func (c *Controller) reconcileEcmpCentralizedSubnetRouteInDefaultVpc(subnet *kubeovnv1.Subnet) error {
	// centralized subnet, enable ecmp, add ecmp policy route
	var (
		gatewayNodes = strings.Split(subnet.Spec.GatewayNode, ",")
		nodeV4IPs    = make([]string, 0, len(gatewayNodes))
		nodeV6IPs    = make([]string, 0, len(gatewayNodes))
		nameV4IPMap  = make(map[string]string, len(gatewayNodes))
		nameV6IPMap  = make(map[string]string, len(gatewayNodes))
	)

	for _, gw := range gatewayNodes {
		// the format of gatewayNodeStr can be like 'kube-ovn-worker:172.18.0.2, kube-ovn-control-plane:172.18.0.3', which consists of node name and designative egress ip
		if strings.Contains(gw, ":") {
			gw = strings.TrimSpace(strings.Split(gw, ":")[0])
		} else {
			gw = strings.TrimSpace(gw)
		}

		node, err := c.nodesLister.Get(gw)
		if err != nil {
			klog.Errorf("failed to get gw node %s, %v", gw, err)
			continue
		}

		if nodeReady(node) {
			nexthopNodeIP := strings.TrimSpace(node.Annotations[util.IPAddressAnnotation])
			if nexthopNodeIP == "" {
				klog.Errorf("gateway node %v has no ip annotation", node.Name)
				continue
			}
			nexthopV4, nexthopV6 := util.SplitStringIP(nexthopNodeIP)
			if nexthopV4 != "" {
				nameV4IPMap[node.Name] = nexthopV4
				nodeV4IPs = append(nodeV4IPs, nexthopV4)
			}
			if nexthopV6 != "" {
				nameV6IPMap[node.Name] = nexthopV6
				nodeV6IPs = append(nodeV6IPs, nexthopV6)
			}
		} else {
			klog.Errorf("gateway node %v is not ready", gw)
		}
	}

	v4CIDR, v6CIDR := util.SplitStringIP(subnet.Spec.CIDRBlock)
	if nodeV4IPs != nil && v4CIDR != "" {
		klog.Infof("delete old distributed policy route for subnet %s", subnet.Name)
		if err := c.deletePolicyRouteByGatewayType(subnet, kubeovnv1.GWDistributedType, false); err != nil {
			klog.Errorf("failed to delete policy route for overlay subnet %s, %v", subnet.Name, err)
			return err
		}
		klog.Infof("subnet %s configure ecmp policy route, nexthops %v", subnet.Name, nodeV4IPs)
		if err := c.updatePolicyRouteForCentralizedSubnet(subnet.Name, v4CIDR, nodeV4IPs, nameV4IPMap); err != nil {
			klog.Errorf("failed to add v4 ecmp policy route for centralized subnet %s: %v", subnet.Name, err)
			return err
		}
	}
	if nodeV6IPs != nil && v6CIDR != "" {
		klog.Infof("delete old distributed policy route for subnet %s", subnet.Name)
		if err := c.deletePolicyRouteByGatewayType(subnet, kubeovnv1.GWDistributedType, false); err != nil {
			klog.Errorf("failed to delete policy route for overlay subnet %s, %v", subnet.Name, err)
			return err
		}
		klog.Infof("subnet %s configure ecmp policy route, nexthops %v", subnet.Name, nodeV6IPs)
		if err := c.updatePolicyRouteForCentralizedSubnet(subnet.Name, v6CIDR, nodeV6IPs, nameV6IPMap); err != nil {
			klog.Errorf("failed to add v6 ecmp policy route for centralized subnet %s: %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) reconcileOvnDefaultVpcRoute(subnet *kubeovnv1.Subnet) error {
	if subnet.Name == c.config.NodeSwitch {
		if err := c.addCommonRoutesForSubnet(subnet); err != nil {
			klog.Error(err)
			return err
		}
		return nil
	}

	if subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway {
		// physical switch provide gw for this underlay subnet
		pods, err := c.podsLister.Pods(metav1.NamespaceAll).List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list pods %v", err)
			return err
		}
		for _, pod := range pods {
			if pod.Annotations[util.LogicalSwitchAnnotation] == subnet.Name && pod.Annotations[util.IPAddressAnnotation] != "" {
				if err := c.deleteStaticRoute(
					pod.Annotations[util.IPAddressAnnotation], c.config.ClusterRouter, subnet.Spec.RouteTable); err != nil {
					klog.Errorf("failed to delete static route %v", err)
					return err
				}
			}
		}

		if !subnet.Spec.LogicalGateway && subnet.Name != c.config.ExternalGatewaySwitch && !subnet.Spec.U2OInterconnection {
			lspName := fmt.Sprintf("%s-%s", subnet.Name, c.config.ClusterRouter)
			klog.Infof("delete logical switch port %s", lspName)
			if err := c.OVNNbClient.DeleteLogicalSwitchPort(lspName); err != nil {
				klog.Errorf("failed to delete lsp %s-%s, %v", subnet.Name, c.config.ClusterRouter, err)
				return err
			}
			lrpName := fmt.Sprintf("%s-%s", c.config.ClusterRouter, subnet.Name)
			klog.Infof("delete logical router port %s", lrpName)
			if err := c.OVNNbClient.DeleteLogicalRouterPort(lrpName); err != nil {
				klog.Errorf("failed to delete lrp %s: %v", lrpName, err)
				return err
			}
		}

		if subnet.Spec.U2OInterconnection && subnet.Status.U2OInterconnectionIP != "" {
			if err := c.addPolicyRouteForU2OInterconn(subnet); err != nil {
				klog.Errorf("failed to add policy route for underlay to overlay subnet interconnection %s %v", subnet.Name, err)
				return err
			}
		} else {
			if err := c.deletePolicyRouteForU2OInterconn(subnet); err != nil {
				klog.Errorf("failed to delete policy route for underlay to overlay subnet interconnection %s, %v", subnet.Name, err)
				return err
			}
		}

		if (!c.config.EnableLb || (subnet.Spec.EnableLb == nil || !*subnet.Spec.EnableLb)) &&
			subnet.Spec.U2OInterconnection && subnet.Status.U2OInterconnectionIP != "" {
			if err := c.addPolicyRouteForU2ONoLoadBalancer(subnet); err != nil {
				klog.Errorf("failed to add policy route for underlay to overlay subnet interconnection without enabling loadbalancer %s %v", subnet.Name, err)
				return err
			}
		} else {
			if err := c.deletePolicyRouteForU2ONoLoadBalancer(subnet); err != nil {
				klog.Errorf("failed to delete policy route for underlay to overlay subnet interconnection without enabling loadbalancer %s, %v", subnet.Name, err)
				return err
			}
		}
	} else {
		// It's difficult to update policy route when subnet cidr is changed, add check for cidr changed situation
		if err := c.reconcilePolicyRouteForCidrChangedSubnet(subnet, true); err != nil {
			klog.Error(err)
			return err
		}

		if err := c.addCommonRoutesForSubnet(subnet); err != nil {
			klog.Error(err)
			return err
		}

		// distributed subnet, only add distributed policy route
		if subnet.Spec.GatewayType == kubeovnv1.GWDistributedType {
			if err := c.reconcileDistributedSubnetRouteInDefaultVpc(subnet); err != nil {
				klog.Error(err)
				return err
			}
		} else {
			// centralized subnet
			if subnet.Spec.GatewayNode == "" {
				subnet.Status.NotReady("NoReadyGateway", "")
				if err := c.patchSubnetStatus(subnet, "NoReadyGateway", ""); err != nil {
					klog.Error(err)
					return err
				}
				err := fmt.Errorf("subnet %s Spec.GatewayNode field must be specified for centralized gateway type", subnet.Name)
				klog.Error(err)
				return err
			}

			gwNodeExists := c.checkGwNodeExists(subnet.Spec.GatewayNode)
			if !gwNodeExists {
				klog.Errorf("failed to init centralized gateway for subnet %s, no gateway node exists", subnet.Name)
				return errors.New("failed to add ecmp policy route, no gateway node exists")
			}

			if err := c.reconcilePolicyRouteForCidrChangedSubnet(subnet, false); err != nil {
				klog.Error(err)
				return err
			}

			if subnet.Spec.EnableEcmp {
				if err := c.reconcileEcmpCentralizedSubnetRouteInDefaultVpc(subnet); err != nil {
					klog.Error(err)
					return err
				}
			} else {
				if err := c.reconcileDefaultCentralizedSubnetRouteInDefaultVpc(subnet); err != nil {
					klog.Error(err)
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) reconcileCustomVpcStaticRoute(subnet *kubeovnv1.Subnet) error {
	// in custom vpc, subnet gw type is unmeaning
	// 1. vpc out to public network through vpc nat gw pod, the static route is auto managed by admin user
	// 2. vpc out to public network through ovn nat lrp, whose nexthop rely on bfd ecmp, the vpc spec bfd static route is auto managed here
	// 3. vpc out to public network through ovn nat lrp, without bfd ecmp, the vpc spec static route is auto managed here

	vpc, err := c.vpcsLister.Get(subnet.Spec.Vpc)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Errorf("failed to get vpc %s, %v", subnet.Spec.Vpc, err)
		return err
	}

	if vpc.Spec.EnableExternal && vpc.Spec.EnableBfd && subnet.Spec.EnableEcmp {
		klog.Infof("add bfd and external static ecmp route for vpc %s, subnet %s", vpc.Name, subnet.Name)
		// handle vpc static route
		// use static ecmp route with bfd
		// bfd ecmp static route depend on subnet cidr
		if err := c.reconcileCustomVpcBfdStaticRoute(vpc.Name, subnet.Name); err != nil {
			klog.Errorf("failed to reconcile vpc %q bfd static route", vpc.Name)
			return err
		}
	}

	if subnet.Spec.Vlan == "" || subnet.Spec.LogicalGateway || subnet.Spec.U2OInterconnection {
		if err = c.addCustomVPCStaticRouteForSubnet(subnet); err != nil {
			klog.Errorf("failed to add static route for underlay to overlay subnet interconnection %s %v", subnet.Name, err)
			return err
		}
		if err = c.addCustomVPCPolicyRoutesForSubnet(subnet); err != nil {
			klog.Error(err)
			return err
		}
	}

	if subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway && subnet.Spec.U2OInterconnection && subnet.Status.U2OInterconnectionIP != "" {
		if err := c.addPolicyRouteForU2OInterconn(subnet); err != nil {
			klog.Errorf("failed to add policy route for underlay to overlay subnet interconnection %s %v", subnet.Name, err)
			return err
		}
	}

	return nil
}

func (c *Controller) deleteStaticRoute(ip, router, routeTable string) error {
	for ipStr := range strings.SplitSeq(ip, ",") {
		if err := c.deleteStaticRouteFromVpc(
			router,
			routeTable,
			ipStr,
			"",
			kubeovnv1.PolicyDst,
		); err != nil {
			klog.Errorf("failed to delete static route %s, %v", ipStr, err)
			return err
		}
	}

	return nil
}

func (c *Controller) reconcileVlan(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vlan == "" {
		return nil
	}
	klog.Infof("reconcile vlan %v", subnet.Spec.Vlan)

	vlan, err := c.vlansLister.Get(subnet.Spec.Vlan)
	if err != nil {
		klog.Errorf("failed to get vlan %s: %v", subnet.Spec.Vlan, err)
		return err
	}
	if vlan.Status.Conflict {
		err = fmt.Errorf("subnet %s has invalid conflict vlan %s", subnet.Name, vlan.Name)
		klog.Error(err)
		return err
	}

	localnetPort := ovs.GetLocalnetName(subnet.Name)
	if err := c.OVNNbClient.CreateLocalnetLogicalSwitchPort(subnet.Name, localnetPort, vlan.Spec.Provider, subnet.Spec.CIDRBlock, vlan.Spec.ID); err != nil {
		klog.Errorf("create localnet port for subnet %s: %v", subnet.Name, err)
		return err
	}

	if !slices.Contains(vlan.Status.Subnets, subnet.Name) {
		newVlan := vlan.DeepCopy()
		newVlan.Status.Subnets = append(newVlan.Status.Subnets, subnet.Name)
		_, err = c.config.KubeOvnClient.KubeovnV1().Vlans().UpdateStatus(context.Background(), newVlan, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to update status of vlan %s: %v", vlan.Name, err)
			return err
		}
	}

	return nil
}

func (c *Controller) reconcileSubnetSpecialIPs(subnet *kubeovnv1.Subnet) (bool, bool, error) {
	isU2OIPChanged := false
	isMcastQuerierIPChanged := false
	var err error

	// reconcile u2o IP
	if subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway {
		u2oInterconnName := fmt.Sprintf(util.U2OInterconnName, subnet.Spec.Vpc, subnet.Name)
		u2oInterconnLrpName := fmt.Sprintf("%s-%s", subnet.Spec.Vpc, subnet.Name)
		var v4ip, v6ip string
		if subnet.Spec.U2OInterconnection {
			v4ip, v6ip, _, err = c.acquireU2OIP(subnet, u2oInterconnName, u2oInterconnLrpName)
			if err != nil {
				return isU2OIPChanged, isMcastQuerierIPChanged, err
			}

			if v4ip != "" || v6ip != "" {
				isU2OIPChanged = true
			}
		} else if subnet.Status.U2OInterconnectionIP != "" {
			err = c.releaseU2OIP(subnet, u2oInterconnName)
			if err != nil {
				return isU2OIPChanged, isMcastQuerierIPChanged, err
			}
			isU2OIPChanged = true
		}

		if isU2OIPChanged {
			klog.Infof("reconcile underlay subnet %s to overlay interconnection with U2OInterconnection %v U2OInterconnectionIP %s",
				subnet.Name, subnet.Spec.U2OInterconnection, subnet.Status.U2OInterconnectionIP)
		}
	}

	// reconcile mcast querier IP
	if subnet.Spec.EnableMulticastSnoop {
		isMcastQuerierIPChanged, err = c.acquireMcastQuerierIP(subnet)
		if err != nil {
			return isU2OIPChanged, isMcastQuerierIPChanged, err
		}
	} else {
		isMcastQuerierIPChanged, err = c.releaseMcastQuerierIP(subnet)
		if err != nil {
			return isU2OIPChanged, isMcastQuerierIPChanged, err
		}
	}

	// caculate subnet status
	if isU2OIPChanged || isMcastQuerierIPChanged {
		if subnet.Spec.Protocol == kubeovnv1.ProtocolDual {
			if _, err := c.calcDualSubnetStatusIP(subnet); err != nil {
				klog.Error(err)
				return isU2OIPChanged, isMcastQuerierIPChanged, err
			}
		} else {
			if _, err := c.calcSubnetStatusIP(subnet); err != nil {
				klog.Error(err)
				return isU2OIPChanged, isMcastQuerierIPChanged, err
			}
		}
	}

	return isU2OIPChanged, isMcastQuerierIPChanged, nil
}

func (c *Controller) acquireU2OIP(subnet *kubeovnv1.Subnet, u2oInterconnName, u2oInterconnLrpName string) (string, string, string, error) {
	var v4ip, v6ip, mac string
	var err error
	if subnet.Spec.U2OInterconnectionIP == "" && (subnet.Status.U2OInterconnectionIP == "" || subnet.Status.U2OInterconnectionMAC == "") {
		v4ip, v6ip, mac, err = c.acquireIPAddress(subnet.Name, u2oInterconnName, u2oInterconnLrpName)
		if err != nil {
			klog.Errorf("failed to acquire underlay to overlay interconnection ip address for subnet %s, %v", subnet.Name, err)
			return "", "", "", err
		}
	} else if subnet.Spec.U2OInterconnectionIP != "" && subnet.Status.U2OInterconnectionIP != subnet.Spec.U2OInterconnectionIP {
		if subnet.Status.U2OInterconnectionIP != "" {
			klog.Infof("release underlay to overlay interconnection ip address %s for subnet %s", subnet.Status.U2OInterconnectionIP, subnet.Name)
			c.ipam.ReleaseAddressByPod(u2oInterconnName, subnet.Name)
		}
		v4ip, v6ip, mac, err = c.acquireStaticIPAddress(subnet.Name, u2oInterconnName, u2oInterconnLrpName, subnet.Spec.U2OInterconnectionIP, nil)
		if err != nil {
			klog.Errorf("failed to acquire static underlay to overlay interconnection ip address for subnet %s, %v", subnet.Name, err)
			return "", "", "", err
		}
	}
	if v4ip != "" || v6ip != "" {
		switch subnet.Spec.Protocol {
		case kubeovnv1.ProtocolIPv4:
			subnet.Status.U2OInterconnectionIP = v4ip
		case kubeovnv1.ProtocolIPv6:
			subnet.Status.U2OInterconnectionIP = v6ip
		case kubeovnv1.ProtocolDual:
			subnet.Status.U2OInterconnectionIP = fmt.Sprintf("%s,%s", v4ip, v6ip)
		}
		err = c.createOrUpdateIPCR("", u2oInterconnName, subnet.Status.U2OInterconnectionIP, mac, subnet.Name, "default", "", "")
		if err != nil {
			klog.Errorf("failed to create or update IPs of %s : %v", u2oInterconnLrpName, err)
			return "", "", "", err
		}
		subnet.Status.U2OInterconnectionMAC = mac
	}
	return v4ip, v6ip, mac, nil
}

func (c *Controller) releaseU2OIP(subnet *kubeovnv1.Subnet, u2oInterconnName string) error {
	klog.Infof("release underlay to overlay interconnection ip address %s for subnet %s", subnet.Status.U2OInterconnectionIP, subnet.Name)
	c.ipam.ReleaseAddressByPod(u2oInterconnName, subnet.Name)
	subnet.Status.U2OInterconnectionIP = ""
	subnet.Status.U2OInterconnectionMAC = ""
	subnet.Status.U2OInterconnectionVPC = ""

	err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), u2oInterconnName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("failed to delete ip %s, %v", u2oInterconnName, err)
		return err
	}

	return nil
}

func (c *Controller) acquireMcastQuerierIP(subnet *kubeovnv1.Subnet) (bool, error) {
	isMcastQuerierChanged := false
	mcastQuerierLspName := fmt.Sprintf(util.McastQuerierName, subnet.Name)
	var v4ip, v6ip, mac string
	var err error

	if subnet.Status.McastQuerierIP == "" || subnet.Status.McastQuerierMAC == "" {
		v4ip, v6ip, mac, err = c.acquireIPAddress(subnet.Name, mcastQuerierLspName, mcastQuerierLspName)
		if err != nil {
			klog.Errorf("failed to acquire mcast querier ip address for subnet %s, %v", subnet.Name, err)
			return isMcastQuerierChanged, err
		}
	}

	if v4ip != "" || v6ip != "" {
		switch subnet.Spec.Protocol {
		case kubeovnv1.ProtocolIPv4:
			subnet.Status.McastQuerierIP = v4ip
		case kubeovnv1.ProtocolIPv6:
			subnet.Status.McastQuerierIP = v6ip
		case kubeovnv1.ProtocolDual:
			subnet.Status.McastQuerierIP = fmt.Sprintf("%s,%s", v4ip, v6ip)
		}

		err := c.createOrUpdateIPCR("", mcastQuerierLspName, subnet.Status.McastQuerierIP, mac, subnet.Name, "default", "", "")
		if err != nil {
			klog.Errorf("failed to create or update IPs of %s : %v", mcastQuerierLspName, err)
			return isMcastQuerierChanged, err
		}

		subnet.Status.McastQuerierMAC = mac
		klog.Infof("reconcile subnet %s mcast querier IP %s mac %s",
			subnet.Name, subnet.Status.McastQuerierIP, subnet.Status.McastQuerierMAC)
		isMcastQuerierChanged = true
	}

	return isMcastQuerierChanged, nil
}

func (c *Controller) releaseMcastQuerierIP(subnet *kubeovnv1.Subnet) (bool, error) {
	isMcastQuerierChanged := false
	if subnet.Status.McastQuerierIP != "" {
		mcastQuerierLspName := fmt.Sprintf(util.McastQuerierName, subnet.Name)
		klog.Infof("release mcast querier ip address %s for subnet %s", subnet.Status.McastQuerierIP, subnet.Name)
		c.ipam.ReleaseAddressByPod(mcastQuerierLspName, subnet.Name)
		subnet.Status.McastQuerierIP = ""
		subnet.Status.McastQuerierMAC = ""

		if err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), mcastQuerierLspName, metav1.DeleteOptions{}); err != nil {
			if !k8serrors.IsNotFound(err) {
				klog.Errorf("failed to delete ip %s, %v", mcastQuerierLspName, err)
				return isMcastQuerierChanged, err
			}
		}
		isMcastQuerierChanged = true
		klog.Infof("reconcile subnet %s mcast querier IP %s mac %s",
			subnet.Name, subnet.Status.McastQuerierIP, subnet.Status.McastQuerierMAC)
	}
	return isMcastQuerierChanged, nil
}

func (c *Controller) calcDualSubnetStatusIP(subnet *kubeovnv1.Subnet) (*kubeovnv1.Subnet, error) {
	if err := util.CheckCidrs(subnet.Spec.CIDRBlock); err != nil {
		return nil, err
	}
	// Get the number of pods, not ips. For one pod with two ip(v4 & v6) in dual-stack, num of Items is 1
	podUsedIPs, err := c.ipsLister.List(labels.SelectorFromSet(labels.Set{subnet.Name: ""}))
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	var lenIP, lenVip, lenIptablesEip, lenOvnEip int
	lenIP = len(podUsedIPs)
	usingIPNums := lenIP

	// TODO:// replace ExcludeIps with ip pool and gw to avoid later loop
	noGWExcludeIPs := []string{}
	v4gw, v6gw := util.SplitStringIP(subnet.Spec.Gateway)
	for _, excludeIP := range subnet.Spec.ExcludeIps {
		if v4gw == excludeIP || v6gw == excludeIP {
			// no need to compare gateway ip with pod ip
			continue
		}
		noGWExcludeIPs = append(noGWExcludeIPs, excludeIP)
	}
	if noGWExcludeIPs != nil {
		for _, podUsedIP := range podUsedIPs {
			for _, excludeIP := range noGWExcludeIPs {
				if util.ContainsIPs(excludeIP, podUsedIP.Spec.V4IPAddress) || util.ContainsIPs(excludeIP, podUsedIP.Spec.V6IPAddress) {
					// This ip cr is allocated from subnet.spec.excludeIPs, do not count it as usingIPNums
					usingIPNums--
					break
				}
			}
		}
	}

	// subnet.Spec.ExcludeIps contains both v4 and v6 addresses
	v4ExcludeIPs, v6ExcludeIPs := util.SplitIpsByProtocol(subnet.Spec.ExcludeIps)
	// gateway always in excludeIPs
	cidrBlocks := strings.Split(subnet.Spec.CIDRBlock, ",")
	v4toSubIPs := util.ExpandExcludeIPs(v4ExcludeIPs, cidrBlocks[0])
	v6toSubIPs := util.ExpandExcludeIPs(v6ExcludeIPs, cidrBlocks[1])
	_, v4CIDR, _ := net.ParseCIDR(cidrBlocks[0])
	_, v6CIDR, _ := net.ParseCIDR(cidrBlocks[1])
	v4availableIPs := util.AddressCount(v4CIDR) - util.CountIPNums(v4toSubIPs)
	v6availableIPs := util.AddressCount(v6CIDR) - util.CountIPNums(v6toSubIPs)

	usingIPs := float64(usingIPNums)

	vips, err := c.virtualIpsLister.List(labels.SelectorFromSet(labels.Set{
		util.SubnetNameLabel: subnet.Name,
		util.IPReservedLabel: "",
	}))
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	lenVip = len(vips)
	usingIPs += float64(lenVip)

	if !isOvnSubnet(subnet) {
		eips, err := c.iptablesEipsLister.List(
			labels.SelectorFromSet(labels.Set{util.SubnetNameLabel: subnet.Name}))
		if err != nil {
			klog.Error(err)
			return nil, err
		}
		lenIptablesEip = len(eips)
		usingIPs += float64(lenIptablesEip)
	}
	if subnet.Spec.Vlan != "" {
		ovnEips, err := c.ovnEipsLister.List(labels.SelectorFromSet(labels.Set{
			util.SubnetNameLabel: subnet.Name,
		}))
		if err != nil {
			klog.Error(err)
			return nil, err
		}
		lenOvnEip = len(ovnEips)
		usingIPs += float64(lenOvnEip)
	}

	v4availableIPs -= usingIPs
	if v4availableIPs < 0 {
		v4availableIPs = 0
	}
	v6availableIPs -= usingIPs
	if v6availableIPs < 0 {
		v6availableIPs = 0
	}

	v4UsingIPStr, v6UsingIPStr, v4AvailableIPStr, v6AvailableIPStr := c.ipam.GetSubnetIPRangeString(subnet.Name, subnet.Spec.ExcludeIps)

	if subnet.Status.V4AvailableIPs == v4availableIPs &&
		subnet.Status.V6AvailableIPs == v6availableIPs &&
		subnet.Status.V4UsingIPs == usingIPs &&
		subnet.Status.V6UsingIPs == usingIPs &&
		subnet.Status.V4UsingIPRange == v4UsingIPStr &&
		subnet.Status.V6UsingIPRange == v6UsingIPStr &&
		subnet.Status.V4AvailableIPRange == v4AvailableIPStr &&
		subnet.Status.V6AvailableIPRange == v6AvailableIPStr {
		return subnet, nil
	}

	subnet.Status.V4AvailableIPs = v4availableIPs
	subnet.Status.V6AvailableIPs = v6availableIPs
	subnet.Status.V4UsingIPs = usingIPs
	subnet.Status.V6UsingIPs = usingIPs
	subnet.Status.V4UsingIPRange = v4UsingIPStr
	subnet.Status.V6UsingIPRange = v6UsingIPStr
	subnet.Status.V4AvailableIPRange = v4AvailableIPStr
	subnet.Status.V6AvailableIPRange = v6AvailableIPStr
	bytes, err := subnet.Status.Bytes()
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	newSubnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status")
	return newSubnet, err
}

func (c *Controller) calcSubnetStatusIP(subnet *kubeovnv1.Subnet) (*kubeovnv1.Subnet, error) {
	_, cidr, err := net.ParseCIDR(subnet.Spec.CIDRBlock)
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	var lenIP, lenVip, lenIptablesEip, lenOvnEip int
	podUsedIPs, err := c.ipsLister.List(labels.SelectorFromSet(labels.Set{subnet.Name: ""}))
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	lenIP = len(podUsedIPs)
	usingIPNums := lenIP

	// TODO:// replace ExcludeIps with ip pool and gw to avoid later loop
	noGWExcludeIPs := []string{}
	v4gw, v6gw := util.SplitStringIP(subnet.Spec.Gateway)
	for _, excludeIP := range subnet.Spec.ExcludeIps {
		if v4gw == excludeIP || v6gw == excludeIP {
			// no need to compare gateway ip with pod ip
			continue
		}
		noGWExcludeIPs = append(noGWExcludeIPs, excludeIP)
	}
	if noGWExcludeIPs != nil {
		for _, podUsedIP := range podUsedIPs {
			for _, excludeIP := range noGWExcludeIPs {
				if util.ContainsIPs(excludeIP, podUsedIP.Spec.V4IPAddress) || util.ContainsIPs(excludeIP, podUsedIP.Spec.V6IPAddress) {
					// This ip cr is allocated from subnet.spec.excludeIPs, do not count it as usingIPNums
					usingIPNums--
					break
				}
			}
		}
	}

	// gateway always in excludeIPs
	toSubIPs := util.ExpandExcludeIPs(subnet.Spec.ExcludeIps, subnet.Spec.CIDRBlock)
	availableIPs := util.AddressCount(cidr) - util.CountIPNums(toSubIPs)
	usingIPs := float64(usingIPNums)
	vips, err := c.virtualIpsLister.List(labels.SelectorFromSet(labels.Set{
		util.SubnetNameLabel: subnet.Name,
		util.IPReservedLabel: "",
	}))
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	lenVip = len(vips)
	usingIPs += float64(lenVip)
	if !isOvnSubnet(subnet) {
		eips, err := c.iptablesEipsLister.List(
			labels.SelectorFromSet(labels.Set{util.SubnetNameLabel: subnet.Name}))
		if err != nil {
			klog.Error(err)
			return nil, err
		}
		lenIptablesEip = len(eips)
		usingIPs += float64(lenIptablesEip)
	}
	if subnet.Spec.Vlan != "" {
		ovnEips, err := c.ovnEipsLister.List(labels.SelectorFromSet(labels.Set{
			util.SubnetNameLabel: subnet.Name,
		}))
		if err != nil {
			klog.Error(err)
			return nil, err
		}
		lenOvnEip = len(ovnEips)
		usingIPs += float64(lenOvnEip)
	}

	availableIPs -= usingIPs
	if availableIPs < 0 {
		availableIPs = 0
	}

	v4UsingIPStr, v6UsingIPStr, v4AvailableIPStr, v6AvailableIPStr := c.ipam.GetSubnetIPRangeString(subnet.Name, subnet.Spec.ExcludeIps)
	cachedFloatFields := [4]float64{
		subnet.Status.V4AvailableIPs,
		subnet.Status.V4UsingIPs,
		subnet.Status.V6AvailableIPs,
		subnet.Status.V6UsingIPs,
	}
	cachedStringFields := [4]string{
		subnet.Status.V4UsingIPRange,
		subnet.Status.V4AvailableIPRange,
		subnet.Status.V6UsingIPRange,
		subnet.Status.V6AvailableIPRange,
	}

	if subnet.Spec.Protocol == kubeovnv1.ProtocolIPv4 {
		subnet.Status.V4AvailableIPs = availableIPs
		subnet.Status.V4UsingIPs = usingIPs
		subnet.Status.V4UsingIPRange = v4UsingIPStr
		subnet.Status.V4AvailableIPRange = v4AvailableIPStr
		subnet.Status.V6AvailableIPs = 0
		subnet.Status.V6UsingIPs = 0
	} else {
		subnet.Status.V6AvailableIPs = availableIPs
		subnet.Status.V6UsingIPs = usingIPs
		subnet.Status.V6UsingIPRange = v6UsingIPStr
		subnet.Status.V6AvailableIPRange = v6AvailableIPStr
		subnet.Status.V4AvailableIPs = 0
		subnet.Status.V4UsingIPs = 0
	}
	if cachedFloatFields == [4]float64{
		subnet.Status.V4AvailableIPs,
		subnet.Status.V4UsingIPs,
		subnet.Status.V6AvailableIPs,
		subnet.Status.V6UsingIPs,
	} && cachedStringFields == [4]string{
		subnet.Status.V4UsingIPRange,
		subnet.Status.V4AvailableIPRange,
		subnet.Status.V6UsingIPRange,
		subnet.Status.V6AvailableIPRange,
	} {
		return subnet, nil
	}

	bytes, err := subnet.Status.Bytes()
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	newSubnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Patch(context.Background(), subnet.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status")
	return newSubnet, err
}

func (c *Controller) checkSubnetUsingIPs(subnet *kubeovnv1.Subnet) error {
	if subnet.Status.V4UsingIPs != 0 && subnet.Status.V4UsingIPRange == "" {
		err := fmt.Errorf("subnet %s has %.0f v4 ip in use, while the v4 using ip range is empty", subnet.Name, subnet.Status.V4UsingIPs)
		klog.Error(err)
		return err
	}
	if subnet.Status.V6UsingIPs != 0 && subnet.Status.V6UsingIPRange == "" {
		err := fmt.Errorf("subnet %s has %.0f v6 ip in use, while the v6 using ip range is empty", subnet.Name, subnet.Status.V6UsingIPs)
		klog.Error(err)
		return err
	}
	return nil
}

func isOvnSubnet(subnet *kubeovnv1.Subnet) bool {
	return util.IsOvnProvider(subnet.Spec.Provider)
}

func checkAndFormatsExcludeIPs(subnet *kubeovnv1.Subnet) bool {
	var excludeIPs []string
	mapIPs := make(map[string]*ipam.IPRange, len(subnet.Spec.ExcludeIps))
	for _, excludeIP := range subnet.Spec.ExcludeIps {
		if _, ok := mapIPs[excludeIP]; !ok {
			ips := strings.Split(excludeIP, "..")
			start, _ := ipam.NewIP(ips[0])
			end := start
			if len(ips) != 1 {
				end, _ = ipam.NewIP(ips[1])
			}
			mapIPs[excludeIP] = ipam.NewIPRange(start, end)
		}
	}
	newMap := filterRepeatIPRange(mapIPs)
	for _, v := range newMap {
		if v.Start().Equal(v.End()) {
			excludeIPs = append(excludeIPs, v.Start().String())
		} else {
			excludeIPs = append(excludeIPs, v.Start().String()+".."+v.End().String())
		}
	}
	sort.Strings(excludeIPs)
	if !slices.Equal(subnet.Spec.ExcludeIps, excludeIPs) {
		klog.V(3).Infof("excludeips before format is %v, after format is %v", subnet.Spec.ExcludeIps, excludeIPs)
		subnet.Spec.ExcludeIps = excludeIPs
		return true
	}
	return false
}

func filterRepeatIPRange(mapIPs map[string]*ipam.IPRange) map[string]*ipam.IPRange {
	for ka, a := range mapIPs {
		for kb, b := range mapIPs {
			if ka == kb && a == b {
				continue
			}

			if b.End().LessThan(a.Start()) || b.Start().GreaterThan(a.End()) {
				continue
			}

			if (a.Start().Equal(b.Start()) || a.Start().GreaterThan(b.Start())) &&
				(a.End().Equal(b.End()) || a.End().LessThan(b.End())) {
				delete(mapIPs, ka)
				continue
			}

			if (a.Start().Equal(b.Start()) || a.Start().GreaterThan(b.Start())) &&
				a.End().GreaterThan(b.End()) {
				delete(mapIPs, ka)
				mapIPs[kb] = ipam.NewIPRange(b.Start(), a.End())
				continue
			}

			if (a.End().Equal(b.End()) || a.End().LessThan(b.End())) &&
				a.Start().LessThan(b.Start()) {
				delete(mapIPs, ka)
				mapIPs[kb] = ipam.NewIPRange(a.Start(), b.End())
				continue
			}

			// a contains b
			mapIPs[kb] = a
			delete(mapIPs, ka)
		}
	}
	return mapIPs
}

func (c *Controller) checkGwNodeExists(gatewayNode string) bool {
	found := false
	for gwName := range strings.SplitSeq(gatewayNode, ",") {
		// the format of gatewayNode can be like 'kube-ovn-worker:172.18.0.2, kube-ovn-control-plane:172.18.0.3', which consists of node name and designative egress ip
		if strings.Contains(gwName, ":") {
			gwName = strings.TrimSpace(strings.Split(gwName, ":")[0])
		} else {
			gwName = strings.TrimSpace(gwName)
		}

		gwNode, err := c.nodesLister.Get(gwName)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				klog.Errorf("gw node %s does not exist, %v", gwName, err)
				continue
			}
		}
		if gwNode != nil {
			found = true
			break
		}
	}
	return found
}

func (c *Controller) addCommonRoutesForSubnet(subnet *kubeovnv1.Subnet) error {
	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		if cidr == "" {
			continue
		}

		var gateway string
		protocol := util.CheckProtocol(cidr)
		for gw := range strings.SplitSeq(subnet.Spec.Gateway, ",") {
			if util.CheckProtocol(gw) == protocol {
				gateway = gw
				break
			}
		}
		if gateway == "" {
			return fmt.Errorf("failed to get gateway of CIDR %s", cidr)
		}

		// policy route
		af := 4
		if protocol == kubeovnv1.ProtocolIPv6 {
			af = 6
		}

		var (
			match       = fmt.Sprintf("ip%d.dst == %s", af, cidr)
			action      = kubeovnv1.PolicyRouteActionAllow
			externalIDs = map[string]string{"vendor": util.CniTypeName, "subnet": subnet.Name}
		)
		klog.Infof("add common policy route for router: %s, match %s, action %s, externalID %v", subnet.Spec.Vpc, match, action, externalIDs)
		if err := c.addPolicyRouteToVpc(
			subnet.Spec.Vpc,
			&kubeovnv1.PolicyRoute{
				Priority: util.SubnetRouterPolicyPriority,
				Match:    match,
				Action:   action,
			},
			externalIDs,
		); err != nil {
			klog.Errorf("failed to add logical router policy for CIDR %s of subnet %s: %v", cidr, subnet.Name, err)
			return err
		}
	}
	return nil
}

func getOverlaySubnetsPortGroupName(subnetName, nodeName string) string {
	return strings.ReplaceAll(fmt.Sprintf("%s.%s", subnetName, nodeName), "-", ".")
}

func (c *Controller) createPortGroupForDistributedSubnet(node *v1.Node, subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway {
		return nil
	}
	if subnet.Spec.Vpc != c.config.ClusterRouter || subnet.Name == c.config.NodeSwitch {
		return nil
	}

	pgName := getOverlaySubnetsPortGroupName(subnet.Name, node.Name)
	externalIDs := map[string]string{
		"subnet":         subnet.Name,
		"node":           node.Name,
		"vendor":         util.CniTypeName,
		networkPolicyKey: subnet.Name + "/" + node.Name,
	}
	if err := c.OVNNbClient.CreatePortGroup(pgName, externalIDs); err != nil {
		klog.Errorf("create port group for subnet %s and node %s: %v", subnet.Name, node.Name, err)
		return err
	}

	return nil
}

func (c *Controller) updatePolicyRouteForCentralizedSubnet(subnetName, cidr string, nextHops []string, nameIPMap map[string]string) error {
	ipSuffix := "ip4"
	if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv6 {
		ipSuffix = "ip6"
	}

	var (
		match  = fmt.Sprintf("%s.src == %s", ipSuffix, cidr)
		action = kubeovnv1.PolicyRouteActionReroute
		// there's no way to update policy route when gatewayNode changed for subnet, so delete and readd policy route
		// The delete operation is processed in AddPolicyRoute if the policy route is inconsistent, so no need delete here
		externalIDs = map[string]string{
			"vendor": util.CniTypeName,
			"subnet": subnetName,
		}
	)
	// It's difficult to delete policy route when delete node,
	// add map nodeName:nodeIP to external_ids to help process when delete node
	maps.Copy(externalIDs, nameIPMap)
	klog.Infof("add policy route for router: %s, match %s, action %s, nexthops %v, externalID %s", c.config.ClusterRouter, match, action, nextHops, externalIDs)
	if err := c.addPolicyRouteToVpc(
		c.config.ClusterRouter,
		&kubeovnv1.PolicyRoute{
			Priority:  util.GatewayRouterPolicyPriority,
			Match:     match,
			Action:    action,
			NextHopIP: strings.Join(nextHops, ","),
		},
		externalIDs,
	); err != nil {
		klog.Errorf("failed to add policy route for centralized subnet %s: %v", subnetName, err)
		return err
	}
	return nil
}

func (c *Controller) addPolicyRouteForCentralizedSubnet(subnet *kubeovnv1.Subnet, nodeName string, ipNameMap map[string]string, nodeIPs []string) error {
	for _, nodeIP := range nodeIPs {
		// node v4ip v6ip
		for cidrBlock := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
			if util.CheckProtocol(cidrBlock) != util.CheckProtocol(nodeIP) {
				continue
			}
			// Check for repeat policy route is processed in AddPolicyRoute

			var nextHops []string
			nameIPMap := map[string]string{}
			nextHops = append(nextHops, nodeIP)
			tmpName := nodeName
			if nodeName == "" {
				tmpName = ipNameMap[nodeIP]
			}
			nameIPMap[tmpName] = nodeIP
			if err := c.updatePolicyRouteForCentralizedSubnet(subnet.Name, cidrBlock, nextHops, nameIPMap); err != nil {
				klog.Error(err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) deletePolicyRouteForCentralizedSubnet(subnet *kubeovnv1.Subnet) error {
	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		ipSuffix := "ip4"
		if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv6 {
			ipSuffix = "ip6"
		}
		match := fmt.Sprintf("%s.src == %s", ipSuffix, cidr)
		klog.Infof("delete policy route for router: %s, priority: %d, match %s", c.config.ClusterRouter, util.GatewayRouterPolicyPriority, match)
		if err := c.deletePolicyRouteFromVpc(c.config.ClusterRouter, util.GatewayRouterPolicyPriority, match); err != nil {
			klog.Errorf("failed to delete policy route for centralized subnet %s: %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) addPolicyRouteForDistributedSubnet(subnet *kubeovnv1.Subnet, nodeName, nodeIPv4, nodeIPv6 string) error {
	if subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway {
		return nil
	}
	if subnet.Spec.Vpc != c.config.ClusterRouter || subnet.Name == c.config.NodeSwitch {
		return nil
	}

	pgName := getOverlaySubnetsPortGroupName(subnet.Name, nodeName)
	for cidrBlock := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		ipSuffix, nodeIP := "ip4", nodeIPv4
		if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv6 {
			ipSuffix, nodeIP = "ip6", nodeIPv6
		}
		if nodeIP == "" {
			continue
		}

		var (
			pgAs        = fmt.Sprintf("%s_%s", pgName, ipSuffix)
			match       = fmt.Sprintf("%s.src == $%s", ipSuffix, pgAs)
			action      = kubeovnv1.PolicyRouteActionReroute
			externalIDs = map[string]string{
				"vendor": util.CniTypeName,
				"subnet": subnet.Name,
				"node":   nodeName,
			}
		)

		klog.Infof("add policy route for router: %s, match %s, action %s, externalID %v", c.config.ClusterRouter, match, action, externalIDs)
		if err := c.addPolicyRouteToVpc(
			c.config.ClusterRouter,
			&kubeovnv1.PolicyRoute{
				Priority:  util.GatewayRouterPolicyPriority,
				Match:     match,
				Action:    action,
				NextHopIP: nodeIP,
			},
			externalIDs,
		); err != nil {
			klog.Errorf("failed to add logical router policy for port-group address-set %s: %v", pgAs, err)
			return err
		}
	}
	return nil
}

func (c *Controller) deletePolicyRouteForDistributedSubnet(subnet *kubeovnv1.Subnet, nodeName string) error {
	pgName := getOverlaySubnetsPortGroupName(subnet.Name, nodeName)
	for cidrBlock := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		ipSuffix := "ip4"
		if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv6 {
			ipSuffix = "ip6"
		}
		pgAs := fmt.Sprintf("%s_%s", pgName, ipSuffix)
		match := fmt.Sprintf("%s.src == $%s", ipSuffix, pgAs)
		klog.Infof("delete policy route for router: %s, priority: %d, match: %q", c.config.ClusterRouter, util.GatewayRouterPolicyPriority, match)
		if err := c.deletePolicyRouteFromVpc(c.config.ClusterRouter, util.GatewayRouterPolicyPriority, match); err != nil {
			klog.Errorf("failed to delete policy route for subnet %s: %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) deletePolicyRouteByGatewayType(subnet *kubeovnv1.Subnet, gatewayType string, isDelete bool) error {
	if (subnet.Spec.Vlan != "" && !subnet.Spec.LogicalGateway) || subnet.Spec.Vpc != c.config.ClusterRouter {
		return nil
	}

	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		if cidr == "" || !isDelete {
			continue
		}

		af := 4
		if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv6 {
			af = 6
		}
		match := fmt.Sprintf("ip%d.dst == %s", af, cidr)
		klog.Infof("delete policy route for router: %s, priority: %d, match %s", c.config.ClusterRouter, util.SubnetRouterPolicyPriority, match)
		if err := c.deletePolicyRouteFromVpc(c.config.ClusterRouter, util.SubnetRouterPolicyPriority, match); err != nil {
			klog.Errorf("failed to delete logical router policy for CIDR %s of subnet %s: %v", cidr, subnet.Name, err)
			return err
		}
	}
	if subnet.Name == c.config.NodeSwitch {
		return nil
	}

	if gatewayType == kubeovnv1.GWDistributedType {
		nodes, err := c.nodesLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("list nodes: %v", err)
			return err
		}
		for _, node := range nodes {
			pgName := getOverlaySubnetsPortGroupName(subnet.Name, node.Name)
			if err = c.OVNNbClient.DeletePortGroup(pgName); err != nil {
				klog.Errorf("delete port group for subnet %s and node %s: %v", subnet.Name, node.Name, err)
				return err
			}

			if err = c.deletePolicyRouteForDistributedSubnet(subnet, node.Name); err != nil {
				klog.Errorf("delete policy route for subnet %s and node %s: %v", subnet.Name, node.Name, err)
				return err
			}
		}
	}

	if gatewayType == kubeovnv1.GWCentralizedType {
		klog.Infof("delete policy route for centralized subnet %s", subnet.Name)
		if err := c.deletePolicyRouteForCentralizedSubnet(subnet); err != nil {
			klog.Errorf("delete policy route for subnet %s: %v", subnet.Name, err)
			return err
		}
	}

	return nil
}

func (c *Controller) addPolicyRouteForU2OInterconn(subnet *kubeovnv1.Subnet) error {
	var v4Gw, v6Gw string
	for gw := range strings.SplitSeq(subnet.Spec.Gateway, ",") {
		switch util.CheckProtocol(gw) {
		case kubeovnv1.ProtocolIPv4:
			v4Gw = gw
		case kubeovnv1.ProtocolIPv6:
			v6Gw = gw
		}
	}

	externalIDs := map[string]string{
		"vendor":           util.CniTypeName,
		"subnet":           subnet.Name,
		"isU2ORoutePolicy": "true",
	}

	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}

	var nodesIPv4, nodesIPv6 []string
	for _, node := range nodes {
		nodeIPv4, nodeIPv6 := util.GetNodeInternalIP(*node)

		if nodeIPv4 != "" {
			nodesIPv4 = append(nodesIPv4, nodeIPv4)
		}
		if nodeIPv6 != "" {
			nodesIPv6 = append(nodesIPv6, nodeIPv6)
		}
	}

	u2oExcludeIP4Ag := strings.ReplaceAll(fmt.Sprintf(util.U2OExcludeIPAg, subnet.Name, "ip4"), "-", ".")
	u2oExcludeIP6Ag := strings.ReplaceAll(fmt.Sprintf(util.U2OExcludeIPAg, subnet.Name, "ip6"), "-", ".")

	if err := c.OVNNbClient.CreateAddressSet(u2oExcludeIP4Ag, externalIDs); err != nil {
		klog.Errorf("create address set %s: %v", u2oExcludeIP4Ag, err)
		return err
	}

	if err := c.OVNNbClient.CreateAddressSet(u2oExcludeIP6Ag, externalIDs); err != nil {
		klog.Errorf("create address set %s: %v", u2oExcludeIP6Ag, err)
		return err
	}

	if len(nodesIPv4) > 0 {
		if err := c.OVNNbClient.AddressSetUpdateAddress(u2oExcludeIP4Ag, nodesIPv4...); err != nil {
			klog.Errorf("set v4 address set %s with address %v: %v", u2oExcludeIP4Ag, nodesIPv4, err)
			return err
		}
	}

	if len(nodesIPv6) > 0 {
		if err := c.OVNNbClient.AddressSetUpdateAddress(u2oExcludeIP6Ag, nodesIPv6...); err != nil {
			klog.Errorf("set v6 address set %s with address %v: %v", u2oExcludeIP6Ag, nodesIPv6, err)
			return err
		}
	}

	for cidrBlock := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		ipSuffix := "ip4"
		nextHop := v4Gw
		U2OexcludeIPAs := u2oExcludeIP4Ag
		if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv6 {
			ipSuffix = "ip6"
			nextHop = v6Gw
			U2OexcludeIPAs = u2oExcludeIP6Ag
		}

		match1 := fmt.Sprintf("%s.dst == %s", ipSuffix, cidrBlock)
		match2 := fmt.Sprintf("%s.dst == $%s && %s.src == %s", ipSuffix, U2OexcludeIPAs, ipSuffix, cidrBlock)
		match3 := fmt.Sprintf("%s.src == %s", ipSuffix, cidrBlock)

		/*
			policy1:
			priority 29400 match: "ip4.dst == underlay subnet cidr"                         action: allow

			policy2:
			priority 31000 match: "ip4.dst == node ips && ip4.src == underlay subnet cidr"  action: reroute physical gw

			policy3:
			priority 29000 match: "ip4.src == underlay subnet cidr"                         action: reroute physical gw

			comment:
			policy1 and policy2 allow overlay pod access underlay but when overlay pod access node ip, it should go join subnet,
			policy3: underlay pod first access u2o interconnection lrp and then reroute to physical gw
		*/
		action := kubeovnv1.PolicyRouteActionAllow
		if subnet.Spec.Vpc == c.config.ClusterRouter {
			klog.Infof("add u2o interconnection policy for router: %s, match %s, action %s", subnet.Spec.Vpc, match1, action)
			if err := c.addPolicyRouteToVpc(
				subnet.Spec.Vpc,
				&kubeovnv1.PolicyRoute{
					Priority: util.U2OSubnetPolicyPriority,
					Match:    match1,
					Action:   action,
				},
				externalIDs,
			); err != nil {
				klog.Errorf("failed to add u2o interconnection policy1 for subnet %s %v", subnet.Name, err)
				return err
			}

			action = kubeovnv1.PolicyRouteActionReroute
			klog.Infof("add u2o interconnection policy for router: %s, match %s, action %s", subnet.Spec.Vpc, match2, action)
			if err := c.addPolicyRouteToVpc(
				subnet.Spec.Vpc,
				&kubeovnv1.PolicyRoute{
					Priority:  util.SubnetRouterPolicyPriority,
					Match:     match2,
					Action:    action,
					NextHopIP: nextHop,
				},
				externalIDs,
			); err != nil {
				klog.Errorf("failed to add u2o interconnection policy2 for subnet %s %v", subnet.Name, err)
				return err
			}
		}

		action = kubeovnv1.PolicyRouteActionReroute
		klog.Infof("add u2o interconnection policy for router: %s, match %s, action %s, nexthop %s", subnet.Spec.Vpc, match3, action, nextHop)
		if err := c.addPolicyRouteToVpc(
			subnet.Spec.Vpc,
			&kubeovnv1.PolicyRoute{
				Priority:  util.GatewayRouterPolicyPriority,
				Match:     match3,
				Action:    action,
				NextHopIP: nextHop,
			},
			externalIDs,
		); err != nil {
			klog.Errorf("failed to add u2o interconnection policy3 for subnet %s %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) deletePolicyRouteForU2OInterconn(subnet *kubeovnv1.Subnet) error {
	logicalRouter, err := c.OVNNbClient.GetLogicalRouter(subnet.Spec.Vpc, true)
	if err == nil && logicalRouter == nil {
		klog.Infof("logical router %s already deleted", subnet.Spec.Vpc)
		return nil
	}
	policies, err := c.OVNNbClient.ListLogicalRouterPolicies(subnet.Spec.Vpc, -1, map[string]string{
		"isU2ORoutePolicy": "true",
		"vendor":           util.CniTypeName,
		"subnet":           subnet.Name,
	}, true)
	if err != nil {
		klog.Errorf("failed to list logical router policies: %v", err)
		return err
	}
	if len(policies) == 0 {
		return nil
	}

	lr := subnet.Status.U2OInterconnectionVPC
	if lr == "" {
		// old version field U2OInterconnectionVPC may be "" and then use subnet.Spec.Vpc
		lr = subnet.Spec.Vpc
	}

	for _, policy := range policies {
		klog.Infof("delete u2o interconnection policy for router %s with match %s priority %d", lr, policy.Match, policy.Priority)
		if err = c.OVNNbClient.DeleteLogicalRouterPolicyByUUID(lr, policy.UUID); err != nil {
			klog.Errorf("failed to delete u2o interconnection policy for subnet %s: %v", subnet.Name, err)
			return err
		}
	}

	u2oExcludeIP4Ag := strings.ReplaceAll(fmt.Sprintf(util.U2OExcludeIPAg, subnet.Name, "ip4"), "-", ".")
	u2oExcludeIP6Ag := strings.ReplaceAll(fmt.Sprintf(util.U2OExcludeIPAg, subnet.Name, "ip6"), "-", ".")

	if err := c.OVNNbClient.DeleteAddressSet(u2oExcludeIP4Ag); err != nil {
		klog.Errorf("delete address set %s: %v", u2oExcludeIP4Ag, err)
		return err
	}

	if err := c.OVNNbClient.DeleteAddressSet(u2oExcludeIP6Ag); err != nil {
		klog.Errorf("delete address set %s: %v", u2oExcludeIP6Ag, err)
		return err
	}

	return nil
}

func (c *Controller) addCustomVPCStaticRouteForSubnet(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vpc == "" {
		return nil
	}

	var v4Gw, v6Gw, v4Cidr, v6Cidr string
	for gw := range strings.SplitSeq(subnet.Spec.Gateway, ",") {
		switch util.CheckProtocol(gw) {
		case kubeovnv1.ProtocolIPv4:
			v4Gw = gw
		case kubeovnv1.ProtocolIPv6:
			v6Gw = gw
		}
	}

	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv4 {
			v4Cidr = cidr
		} else {
			v6Cidr = cidr
		}
	}

	if v4Gw != "" && v4Cidr != "" {
		if err := c.addStaticRouteToVpc(
			subnet.Spec.Vpc,
			&kubeovnv1.StaticRoute{
				Policy:    kubeovnv1.PolicySrc,
				CIDR:      v4Cidr,
				NextHopIP: v4Gw,
			},
		); err != nil {
			klog.Errorf("failed to add static route, %v", err)
			return err
		}
	}

	if v6Gw != "" && v6Cidr != "" {
		if err := c.addStaticRouteToVpc(
			subnet.Spec.Vpc,
			&kubeovnv1.StaticRoute{
				Policy:    kubeovnv1.PolicySrc,
				CIDR:      v6Cidr,
				NextHopIP: v6Gw,
			},
		); err != nil {
			klog.Errorf("failed to add static route, %v", err)
			return err
		}
	}
	return nil
}

func (c *Controller) deleteStaticRouteForU2OInterconn(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vpc == "" {
		return nil
	}

	var v4Gw, v6Gw, v4Cidr, v6Cidr string
	for gw := range strings.SplitSeq(subnet.Spec.Gateway, ",") {
		switch util.CheckProtocol(gw) {
		case kubeovnv1.ProtocolIPv4:
			v4Gw = gw
		case kubeovnv1.ProtocolIPv6:
			v6Gw = gw
		}
	}

	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv4 {
			v4Cidr = cidr
		} else {
			v6Cidr = cidr
		}
	}

	if v4Gw != "" && v4Cidr != "" {
		if err := c.deleteStaticRouteFromVpc(
			subnet.Spec.Vpc,
			subnet.Spec.RouteTable,
			v4Cidr,
			v4Gw,
			kubeovnv1.PolicySrc,
		); err != nil {
			klog.Errorf("failed to add static route, %v", err)
			return err
		}
	}

	if v6Gw != "" && v6Cidr != "" {
		if err := c.deleteStaticRouteFromVpc(
			subnet.Spec.Vpc,
			subnet.Spec.RouteTable,
			v6Cidr,
			v6Gw,
			kubeovnv1.PolicySrc,
		); err != nil {
			klog.Errorf("failed to delete static route, %v", err)
			return err
		}
	}
	return nil
}

func (c *Controller) reconcileRouteTableForSubnet(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.Vlan != "" && !subnet.Spec.U2OInterconnection {
		return nil
	}

	routerPortName := ovs.LogicalRouterPortName(subnet.Spec.Vpc, subnet.Name)
	lrp, err := c.OVNNbClient.GetLogicalRouterPort(routerPortName, false)
	if err != nil {
		klog.Error(err)
		return err
	}

	rtb := lrp.Options["route_table"]

	// no need to update
	if rtb == subnet.Spec.RouteTable {
		return nil
	}

	klog.Infof("reconcile route table %q for subnet %s", subnet.Spec.RouteTable, subnet.Name)
	opt := map[string]string{"route_table": subnet.Spec.RouteTable}
	if err = c.OVNNbClient.UpdateLogicalRouterPortOptions(routerPortName, opt); err != nil {
		klog.Errorf("failed to set route table of logical router port %s to %s: %v", routerPortName, subnet.Spec.RouteTable, err)
		return err
	}

	return nil
}

func (c *Controller) addCustomVPCPolicyRoutesForSubnet(subnet *kubeovnv1.Subnet) error {
	return c.addCommonRoutesForSubnet(subnet)
}

func (c *Controller) deleteCustomVPCPolicyRoutesForSubnet(subnet *kubeovnv1.Subnet) error {
	logicalRouter, err := c.OVNNbClient.GetLogicalRouter(subnet.Spec.Vpc, true)
	if err == nil && logicalRouter == nil {
		klog.Infof("logical router %s already deleted", subnet.Spec.Vpc)
		return nil
	}
	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		af := 4
		if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv6 {
			af = 6
		}
		match := fmt.Sprintf("ip%d.dst == %s", af, cidr)
		klog.Infof("delete policy route for router: %s, priority: %d, match %s", subnet.Spec.Vpc, util.SubnetRouterPolicyPriority, match)
		if err := c.deletePolicyRouteFromVpc(subnet.Spec.Vpc, util.SubnetRouterPolicyPriority, match); err != nil {
			klog.Errorf("failed to delete logical router policy for CIDR %s of subnet %s: %v", cidr, subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) clearOldU2OResource(subnet *kubeovnv1.Subnet) error {
	if subnet.Status.U2OInterconnectionVPC != "" &&
		(!subnet.Spec.U2OInterconnection || (subnet.Spec.U2OInterconnection && subnet.Status.U2OInterconnectionVPC != subnet.Spec.Vpc)) {
		// remove old u2o lsp and lrp first
		lspName := fmt.Sprintf("%s-%s", subnet.Name, subnet.Status.U2OInterconnectionVPC)
		lrpName := fmt.Sprintf("%s-%s", subnet.Status.U2OInterconnectionVPC, subnet.Name)
		klog.Infof("clean subnet %s old u2o resource with lsp %s lrp %s", subnet.Name, lspName, lrpName)
		if err := c.OVNNbClient.DeleteLogicalSwitchPort(lspName); err != nil {
			klog.Errorf("failed to delete u2o logical switch port %s: %v", lspName, err)
			return err
		}

		if err := c.OVNNbClient.DeleteLogicalRouterPort(lrpName); err != nil {
			klog.Errorf("failed to delete u2o logical router port %s: %v", lrpName, err)
			return err
		}

		if err := c.deletePolicyRouteForU2OInterconn(subnet); err != nil {
			klog.Errorf("failed to delete u2o policy route for u2o connection %s: %v", subnet.Name, err)
			return err
		}

		if subnet.Status.U2OInterconnectionVPC != c.config.ClusterRouter {
			if err := c.deleteStaticRouteForU2OInterconn(subnet); err != nil {
				klog.Errorf("failed to delete u2o static route for u2o connection %s: %v", subnet.Name, err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) reconcilePolicyRouteForCidrChangedSubnet(subnet *kubeovnv1.Subnet, isCommonRoute bool) error {
	var match string
	var priority int

	if isCommonRoute {
		priority = util.SubnetRouterPolicyPriority
	} else {
		priority = util.GatewayRouterPolicyPriority
	}

	policies, err := c.OVNNbClient.ListLogicalRouterPolicies(subnet.Spec.Vpc, priority, map[string]string{
		"vendor": util.CniTypeName,
		"subnet": subnet.Name,
	}, true)
	if err != nil {
		klog.Errorf("failed to list logical router policies: %v", err)
		return err
	}
	if len(policies) == 0 {
		return nil
	}

	for _, policy := range policies {
		policyProtocol := kubeovnv1.ProtocolIPv4
		if strings.Contains(policy.Match, "ip6") {
			policyProtocol = kubeovnv1.ProtocolIPv6
		}

		for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
			if cidr == "" {
				continue
			}
			if policyProtocol != util.CheckProtocol(cidr) {
				continue
			}

			af := 4
			if util.CheckProtocol(cidr) == kubeovnv1.ProtocolIPv6 {
				af = 6
			}

			if isCommonRoute {
				match = fmt.Sprintf("ip%d.dst == %s", af, cidr)
			} else {
				if subnet.Spec.GatewayType == kubeovnv1.GWCentralizedType {
					match = fmt.Sprintf("ip%d.src == %s", af, cidr)
				} else {
					// distributed subnet does not need process gateway route policy
					continue
				}
			}

			if policy.Match != match {
				klog.Infof("delete old policy route for subnet %s with match %s priority %d, new match %v", subnet.Name, policy.Match, policy.Priority, match)
				if err = c.OVNNbClient.DeleteLogicalRouterPolicyByUUID(subnet.Spec.Vpc, policy.UUID); err != nil {
					klog.Errorf("failed to delete policy route for subnet %s: %v", subnet.Name, err)
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) addPolicyRouteForU2ONoLoadBalancer(subnet *kubeovnv1.Subnet) error {
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}
	for _, node := range nodes {
		pgName := getOverlaySubnetsPortGroupName(subnet.Name, node.Name)
		if err := c.OVNNbClient.CreatePortGroup(pgName, map[string]string{logicalRouterKey: subnet.Spec.Vpc, logicalSwitchKey: subnet.Name, u2oKey: "true"}); err != nil {
			klog.Errorf("failed to create u2o port group for subnet %s and node %s: %v", subnet.Name, node.Name, err)
			return err
		}
		key := util.NodeLspName(node.Name)
		ip, err := c.ipsLister.Get(key)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil
			}
			klog.Error(err)
			return err
		}
		v4Svc, v6Svc := util.SplitStringIP(c.config.ServiceClusterIPRange)
		for cidrBlock := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
			ipSuffix, nodeIP, svcCIDR := "ip4", ip.Spec.V4IPAddress, v4Svc
			if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv6 {
				ipSuffix, nodeIP, svcCIDR = "ip6", ip.Spec.V6IPAddress, v6Svc
			}
			if nodeIP == "" || svcCIDR == "" {
				continue
			}

			var (
				pgAs        = fmt.Sprintf("%s_%s", pgName, ipSuffix)
				match       = fmt.Sprintf("%s.src == $%s && %s.dst == %s", ipSuffix, pgAs, ipSuffix, svcCIDR)
				action      = kubeovnv1.PolicyRouteActionReroute
				externalIDs = map[string]string{
					"vendor":               util.CniTypeName,
					"subnet":               subnet.Name,
					"isU2ORoutePolicy":     "true",
					"isU2ONoLBRoutePolicy": "true",
					"node":                 node.Name,
				}
			)

			klog.Infof("add u2o interconnection policy without enabling loadbalancer for router: %s, match %s, action %s, nexthop %s", subnet.Spec.Vpc, match, action, nodeIP)
			if err := c.addPolicyRouteToVpc(
				c.config.ClusterRouter,
				&kubeovnv1.PolicyRoute{
					Priority:  util.U2OSubnetPolicyPriority,
					Match:     match,
					Action:    action,
					NextHopIP: nodeIP,
				},
				externalIDs,
			); err != nil {
				klog.Errorf("failed to add logical router policy for port-group address-set %s: %v", pgAs, err)
				return err
			}
		}
	}
	lsps, err := c.OVNNbClient.ListNormalLogicalSwitchPorts(true, map[string]string{logicalSwitchKey: subnet.Name})
	if err != nil {
		klog.Errorf("failed to list normal lsps for subnet %s: %v", subnet.Name, err)
		return err
	}
	for _, lsp := range lsps {
		ip, err := c.ipsLister.Get(lsp.Name)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil
			}
			klog.Error(err)
			return err
		}
		pgName := getOverlaySubnetsPortGroupName(subnet.Name, ip.Spec.NodeName)
		if err = c.OVNNbClient.PortGroupAddPorts(pgName, lsp.Name); err != nil {
			klog.Errorf("failed to add port to u2o port group %s: %v", pgName, err)
			return err
		}
	}
	return nil
}

func (c *Controller) deletePolicyRouteForU2ONoLoadBalancer(subnet *kubeovnv1.Subnet) error {
	logicalRouter, err := c.OVNNbClient.GetLogicalRouter(subnet.Spec.Vpc, true)
	if err == nil && logicalRouter == nil {
		klog.Infof("logical router %s already deleted", subnet.Spec.Vpc)
		return nil
	}
	policies, err := c.OVNNbClient.ListLogicalRouterPolicies(subnet.Spec.Vpc, -1, map[string]string{
		"isU2ONoLBRoutePolicy": "true",
		"vendor":               util.CniTypeName,
		"subnet":               subnet.Name,
	}, true)
	if err != nil {
		klog.Errorf("failed to list logical router policies: %v", err)
		return err
	}

	lr := subnet.Status.U2OInterconnectionVPC
	if lr == "" {
		// old version field U2OInterconnectionVPC may be "" and then use subnet.Spec.Vpc
		lr = subnet.Spec.Vpc
	}

	for _, policy := range policies {
		klog.Infof("delete u2o interconnection policy without enabling loadbalancer for router %s with match %s priority %d", lr, policy.Match, policy.Priority)
		if err = c.OVNNbClient.DeleteLogicalRouterPolicyByUUID(lr, policy.UUID); err != nil {
			klog.Errorf("failed to delete u2o interconnection policy for subnet %s: %v", subnet.Name, err)
			return err
		}
	}

	pgs, err := c.OVNNbClient.ListPortGroups(map[string]string{logicalRouterKey: subnet.Spec.Vpc, logicalSwitchKey: subnet.Name, u2oKey: "true"})
	if err != nil {
		klog.Errorf("failed to list u2o port groups with u2oKey is true for subnet %s: %v", subnet.Name, err)
		return err
	}
	for _, pg := range pgs {
		klog.Infof("delete u2o port group %s for subnet %s", pg.Name, subnet.Name)
		if err = c.OVNNbClient.DeletePortGroup(pg.Name); err != nil {
			klog.Errorf("failed to delete u2o port group for subnet %s: %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) findSubnetByNetworkAttachmentDefinition(ns, name string, subnets []*kubeovnv1.Subnet) (*kubeovnv1.Subnet, error) {
	nad, err := c.netAttachLister.NetworkAttachmentDefinitions(ns).Get(name)
	if err != nil {
		klog.Errorf("failed to get net-attach-def %s/%s: %v", ns, name, err)
		return nil, err
	}
	netCfg, err := loadNetConf([]byte(nad.Spec.Config))
	if err != nil {
		klog.Errorf("failed to parse config of net-attach-def %s/%s: %v", ns, name, err)
		return nil, err
	}

	var provider string
	if netCfg.Conf.Type == util.CniTypeName {
		provider = fmt.Sprintf("%s.%s.%s", name, ns, util.OvnProvider)
	} else {
		provider = fmt.Sprintf("%s.%s", name, ns)
	}
	var subnet *kubeovnv1.Subnet
	for _, s := range subnets {
		if s.Spec.Provider == provider {
			subnet = s.DeepCopy()
			break
		}
	}
	if subnet == nil {
		err = fmt.Errorf("failed to get subnet for net-attach-def %s/%s", ns, name)
		klog.Error(err)
		return nil, err
	}

	return subnet, nil
}

func (c *Controller) handleMcastQuerierChange(subnet *kubeovnv1.Subnet) error {
	if subnet.Spec.EnableMulticastSnoop {
		multicastSnoopFlag := map[string]string{
			"mcast_snoop":   "true",
			"mcast_querier": "true",
			"mcast_ip4_src": subnet.Status.McastQuerierIP,
			"mcast_eth_src": subnet.Status.McastQuerierMAC,
		}
		mcastQuerierLspName := fmt.Sprintf(util.McastQuerierName, subnet.Name)
		if err := c.OVNNbClient.CreateLogicalSwitchPort(subnet.Name, mcastQuerierLspName, subnet.Status.McastQuerierIP, subnet.Status.McastQuerierMAC, mcastQuerierLspName, "default", false, "", "", false, nil, ""); err != nil {
			err = fmt.Errorf("failed to create mcast querier lsp %s: %w", mcastQuerierLspName, err)
			klog.Error(err)
			return err
		}

		if err := c.OVNNbClient.LogicalSwitchUpdateOtherConfig(subnet.Name, ovsdb.MutateOperationInsert, multicastSnoopFlag); err != nil {
			klog.Errorf("enable logical switch multicast snoop %s: %v", subnet.Name, err)
			return err
		}
	} else {
		lss, err := c.OVNNbClient.ListLogicalSwitch(false, func(ls *ovnnb.LogicalSwitch) bool {
			return ls.Name == subnet.Name
		})
		if err != nil || len(lss) == 0 {
			klog.Errorf("failed to list logical switch %s: %v", subnet.Name, err)
			return err
		}

		multicastSnoopFlag := map[string]string{
			"mcast_snoop":   lss[0].OtherConfig["mcast_snoop"],
			"mcast_querier": lss[0].OtherConfig["mcast_querier"],
			"mcast_ip4_src": lss[0].OtherConfig["mcast_ip4_src"],
			"mcast_eth_src": lss[0].OtherConfig["mcast_eth_src"],
		}
		mcastQuerierLspName := fmt.Sprintf(util.McastQuerierName, subnet.Name)
		if err := c.OVNNbClient.LogicalSwitchUpdateOtherConfig(subnet.Name, ovsdb.MutateOperationDelete, multicastSnoopFlag); err != nil {
			klog.Errorf("disable logical switch multicast snoop %s: %v", subnet.Name, err)
			return err
		}

		if err := c.OVNNbClient.DeleteLogicalSwitchPort(mcastQuerierLspName); err != nil {
			err = fmt.Errorf("failed to delete mcast querier lsp %s: %w", mcastQuerierLspName, err)
			klog.Error(err)
			return err
		}
	}
	return nil
}
