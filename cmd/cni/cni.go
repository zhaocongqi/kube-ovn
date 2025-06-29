package main

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/netconf"
	"github.com/kubeovn/kube-ovn/pkg/request"
	"github.com/kubeovn/kube-ovn/pkg/util"
	"github.com/kubeovn/kube-ovn/versions"
)

func main() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()

	funcs := skel.CNIFuncs{
		Add: cmdAdd,
		Del: cmdDel,
	}
	about := "CNI kube-ovn plugin " + versions.VERSION
	skel.PluginMainFuncs(funcs, version.All, about)
}

func cmdAdd(args *skel.CmdArgs) error {
	netConf, cniVersion, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}
	podName, err := parseValueFromArgs("K8S_POD_NAME", args.Args)
	if err != nil {
		return err
	}
	podNamespace, err := parseValueFromArgs("K8S_POD_NAMESPACE", args.Args)
	if err != nil {
		return err
	}
	if netConf.Provider == "" && netConf.Type == util.CniTypeName && args.IfName == "eth0" {
		netConf.Provider = util.OvnProvider
	}

	if err = sysctlEnableIPv6(args.Netns); err != nil {
		return err
	}

	client := request.NewCniServerClient(netConf.ServerSocket)
	response, err := client.Add(request.CniRequest{
		CniType:                    netConf.Type,
		PodName:                    podName,
		PodNamespace:               podNamespace,
		ContainerID:                args.ContainerID,
		NetNs:                      args.Netns,
		IfName:                     args.IfName,
		Provider:                   netConf.Provider,
		Routes:                     netConf.Routes,
		DNS:                        netConf.DNS,
		DeviceID:                   netConf.DeviceID,
		VfDriver:                   netConf.VfDriver,
		VhostUserSocketVolumeName:  netConf.VhostUserSocketVolumeName,
		VhostUserSocketName:        netConf.VhostUserSocketName,
		VhostUserSocketConsumption: netConf.VhostUserSocketConsumption,
	})
	if err != nil {
		return types.NewError(types.ErrTryAgainLater, "RPC failed", err.Error())
	}

	result := generateCNIResult(response, args.Netns)
	return types.PrintResult(&result, cniVersion)
}

func generateCNIResult(cniResponse *request.CniResponse, netns string) current.Result {
	result := current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		DNS:        cniResponse.DNS,
		Routes:     parseRoutes(cniResponse.Routes),
	}
	_, mask, _ := net.ParseCIDR(cniResponse.CIDR)
	podIface := current.Interface{
		Name:    cniResponse.PodNicName,
		Mac:     cniResponse.MacAddress,
		Mtu:     cniResponse.Mtu,
		Sandbox: netns,
	}
	switch cniResponse.Protocol {
	case kubeovnv1.ProtocolIPv4:
		ip, route := assignV4Address(cniResponse.IPAddress, cniResponse.Gateway, mask)
		result.IPs = []*current.IPConfig{ip}
		if len(result.Routes) == 0 && route != nil {
			result.Routes = []*types.Route{route}
		}
		result.Interfaces = []*current.Interface{&podIface}
	case kubeovnv1.ProtocolIPv6:
		ip, route := assignV6Address(cniResponse.IPAddress, cniResponse.Gateway, mask)
		result.IPs = []*current.IPConfig{ip}
		if len(result.Routes) == 0 && route != nil {
			result.Routes = []*types.Route{route}
		}
		result.Interfaces = []*current.Interface{&podIface}
	case kubeovnv1.ProtocolDual:
		var netMask *net.IPNet
		var gwStr string
		addRoutes := len(result.Routes) == 0
		for cidrBlock := range strings.SplitSeq(cniResponse.CIDR, ",") {
			_, netMask, _ = net.ParseCIDR(cidrBlock)
			gwStr = ""
			if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv4 {
				ipStr := strings.Split(cniResponse.IPAddress, ",")[0]
				if cniResponse.Gateway != "" {
					gwStr = strings.Split(cniResponse.Gateway, ",")[0]
				}

				ip, route := assignV4Address(ipStr, gwStr, netMask)
				result.IPs = append(result.IPs, ip)
				if addRoutes && route != nil {
					result.Routes = append(result.Routes, route)
				}
			} else if util.CheckProtocol(cidrBlock) == kubeovnv1.ProtocolIPv6 {
				ipStr := strings.Split(cniResponse.IPAddress, ",")[1]
				if cniResponse.Gateway != "" {
					gwStr = strings.Split(cniResponse.Gateway, ",")[1]
				}

				ip, route := assignV6Address(ipStr, gwStr, netMask)
				result.IPs = append(result.IPs, ip)
				if addRoutes && route != nil {
					result.Routes = append(result.Routes, route)
				}
			}
		}
		result.Interfaces = []*current.Interface{&podIface}
	}

	return result
}

func parseRoutes(routes []request.Route) []*types.Route {
	parsedRoutes := make([]*types.Route, len(routes))
	for i, r := range routes {
		if r.Destination == "" {
			if util.CheckProtocol(r.Gateway) == kubeovnv1.ProtocolIPv4 {
				r.Destination = "0.0.0.0/0"
			} else {
				r.Destination = "::/0"
			}
		}
		parsedRoutes[i] = &types.Route{GW: net.ParseIP(r.Gateway)}
		if _, cidr, err := net.ParseCIDR(r.Destination); err == nil {
			parsedRoutes[i].Dst = *cidr
		}
	}
	return parsedRoutes
}

func cmdDel(args *skel.CmdArgs) error {
	netConf, _, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	client := request.NewCniServerClient(netConf.ServerSocket)
	podName, err := parseValueFromArgs("K8S_POD_NAME", args.Args)
	if err != nil {
		return err
	}
	podNamespace, err := parseValueFromArgs("K8S_POD_NAMESPACE", args.Args)
	if err != nil {
		return err
	}
	if netConf.Provider == "" && netConf.Type == util.CniTypeName && args.IfName == "eth0" {
		netConf.Provider = util.OvnProvider
	}

	err = client.Del(request.CniRequest{
		CniType:                    netConf.Type,
		PodName:                    podName,
		PodNamespace:               podNamespace,
		ContainerID:                args.ContainerID,
		NetNs:                      args.Netns,
		IfName:                     args.IfName,
		Provider:                   netConf.Provider,
		DeviceID:                   netConf.DeviceID,
		VhostUserSocketVolumeName:  netConf.VhostUserSocketVolumeName,
		VhostUserSocketConsumption: netConf.VhostUserSocketConsumption,
	})
	if err != nil {
		return types.NewError(types.ErrTryAgainLater, "RPC failed", err.Error())
	}
	return nil
}

func loadNetConf(bytes []byte) (*netconf.NetConf, string, error) {
	n := &netconf.NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", types.NewError(types.ErrDecodingFailure, "failed to load netconf", err.Error())
	}

	if n.Type != util.CniTypeName && n.IPAM != nil {
		n.Provider = n.IPAM.Provider
		n.ServerSocket = n.IPAM.ServerSocket
		n.Routes = n.IPAM.Routes
	}

	if n.ServerSocket == "" {
		return nil, "", types.NewError(types.ErrInvalidNetworkConfig, "Invalid Configuration", fmt.Sprintf("server_socket is required in cni.conf, %+v", n))
	}

	if n.Provider == "" {
		n.Provider = util.OvnProvider
	}

	n.PostLoad()
	return n, n.CNIVersion, nil
}

func parseValueFromArgs(key, argString string) (string, error) {
	if argString == "" {
		return "", types.NewError(types.ErrInvalidNetworkConfig, "Invalid Configuration", "CNI_ARGS is required")
	}
	for arg := range strings.SplitSeq(argString, ";") {
		if value, found := strings.CutPrefix(arg, key+"="); found && len(value) != 0 {
			return value, nil
		}
	}
	return "", types.NewError(types.ErrInvalidNetworkConfig, "Invalid Configuration", key+" is required in CNI_ARGS")
}

func assignV4Address(ipAddress, gateway string, mask *net.IPNet) (*current.IPConfig, *types.Route) {
	ip := &current.IPConfig{
		Address:   net.IPNet{IP: net.ParseIP(ipAddress).To4(), Mask: mask.Mask},
		Gateway:   net.ParseIP(gateway).To4(),
		Interface: current.Int(0),
	}

	var route *types.Route
	if gw := net.ParseIP(gateway); gw != nil {
		route = &types.Route{
			Dst: net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			GW:  net.ParseIP(gateway).To4(),
		}
	}

	return ip, route
}

func assignV6Address(ipAddress, gateway string, mask *net.IPNet) (*current.IPConfig, *types.Route) {
	ip := &current.IPConfig{
		Address:   net.IPNet{IP: net.ParseIP(ipAddress).To16(), Mask: mask.Mask},
		Gateway:   net.ParseIP(gateway).To16(),
		Interface: current.Int(0),
	}

	var route *types.Route
	if gw := net.ParseIP(gateway); gw != nil {
		route = &types.Route{
			Dst: net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
			GW:  net.ParseIP(gateway).To16(),
		}
	}

	return ip, route
}
