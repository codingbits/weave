package net

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"

	"github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/weaveworks/weave/common/odp"
)

type BridgeType int

const (
	WeaveBridgeName = "weave"
	DatapathName    = "datapath"

	None BridgeType = iota
	Bridge
	Fastdp
	BridgedFastdp
	Inconsistent
)

func DetectBridgeType(weaveBridgeName, datapathName string) BridgeType {
	bridge, _ := netlink.LinkByName(weaveBridgeName)
	datapath, _ := netlink.LinkByName(datapathName)

	switch {
	case bridge == nil && datapath == nil:
		return None
	case isBridge(bridge) && datapath == nil:
		return Bridge
	case isDatapath(bridge) && datapath == nil:
		return Fastdp
	case isDatapath(datapath) && isBridge(bridge):
		return BridgedFastdp
	default:
		return Inconsistent
	}
}

var ErrSetBridgeMac = errors.New("Setting $DOCKER_BRIDGE MAC (mitigate https://github.com/docker/docker/issues/14908)")

func EnforceAddrAssignType(bridgeName string) error {
	addrAssignType, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/addr_assign_type", bridgeName))
	if err != nil {
		return err
	}

	// From include/uapi/linux/netdevice.h
	// #define NET_ADDR_PERM       0   /* address is permanent (default) */
	// #define NET_ADDR_RANDOM     1   /* address is generated randomly */
	// #define NET_ADDR_STOLEN     2   /* address is stolen from other device */
	// #define NET_ADDR_SET        3   /* address is set using dev_set_mac_address() */
	// Note the file typically has a newline at the end, so we just look at the first char
	if addrAssignType[0] != '3' {
		link, err := netlink.LinkByName(bridgeName)
		if err != nil {
			return err
		}

		mac, err := RandomMAC()
		if err != nil {
			return err
		}

		if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
			return err
		}
		return ErrSetBridgeMac
	}

	return nil
}

func isBridge(link netlink.Link) bool {
	_, isBridge := link.(*netlink.Bridge)
	return isBridge
}

func isDatapath(link netlink.Link) bool {
	switch link.(type) {
	case *netlink.GenericLink:
		return link.Type() == "openvswitch"
	case *netlink.Device:
		// Assume it's our openvswitch device, and the kernel has not been updated to report the kind.
		return true
	default:
		return false
	}
}

func DetectHairpin(portIfName string, log *logrus.Logger) error {
	link, err := netlink.LinkByName(portIfName)
	if err != nil {
		return fmt.Errorf("Unable to find link %q: %s", portIfName, err)
	}

	ch := make(chan netlink.LinkUpdate)
	// See EnsureInterface for why done channel is not passed
	if err := netlink.LinkSubscribe(ch, nil); err != nil {
		return fmt.Errorf("Unable to subscribe to netlink updates: %s", err)
	}

	pi, err := netlink.LinkGetProtinfo(link)
	if err != nil {
		return fmt.Errorf("Unable to get link protinfo %q: %s", portIfName, err)
	}
	if pi.Hairpin {
		return fmt.Errorf("Hairpin mode enabled on %q", portIfName)
	}

	go func() {
		for up := range ch {
			if up.Attrs().Name == portIfName && up.Attrs().Protinfo != nil &&
				up.Attrs().Protinfo.Hairpin {
				log.Errorf("Hairpin mode enabled on %q", portIfName)
			}
		}
	}()

	return nil
}

var ErrBridgeNoIP = fmt.Errorf("Bridge has no IP address")

func FindBridgeIP(bridgeName string, subnet *net.IPNet) (net.IP, error) {
	netdev, err := GetBridgeNetDev(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("Failed to get netdev for %q bridge: %s", bridgeName, err)
	}
	if len(netdev.CIDRs) == 0 {
		return nil, ErrBridgeNoIP
	}
	if subnet != nil {
		for _, cidr := range netdev.CIDRs {
			if subnet.Contains(cidr.IP) {
				return cidr.IP, nil
			}
		}
	}
	// No subnet, or none in the required subnet; just return the first one
	return netdev.CIDRs[0].IP, nil
}

type BridgeConfig struct {
	DBPrefix         string
	HostRoot         string
	DockerBridgeName string
	WeaveBridgeName  string
	DatapathName     string
	NoFastdp         bool
	NoBridgedFastdp  bool
	MTU              int
}

func CreateBridge(config *BridgeConfig) (BridgeType, error) {
	bridgeType := DetectBridgeType(config.WeaveBridgeName, config.DatapathName)

	if bridgeType == None {
		bridgeType = Bridge
		if !config.NoFastdp {
			bridgeType = BridgedFastdp
			if config.NoBridgedFastdp {
				bridgeType = Fastdp
				config.DatapathName = config.WeaveBridgeName
			}
			odpSupported, err := odp.CreateDatapath(config.DatapathName)
			if err != nil {
				return None, err
			}
			if !odpSupported {
				bridgeType = Bridge
			}
		}

		var err error
		switch bridgeType {
		case Bridge:
			err = initBridge(config)
		case Fastdp:
			err = initFastdp(config)
		case BridgedFastdp:
			err = initBridgedFastdp(config)
		default:
			err = fmt.Errorf("Cannot initialise bridge type %v", bridgeType)
		}
		if err != nil {
			return None, err
		}

		configureIPTables(config)
	}

	if bridgeType == Bridge {
		if err := EthtoolTXOff(config.WeaveBridgeName); err != nil {
			return bridgeType, err
		}
	}

	if err := linkSetUpByName(config.WeaveBridgeName); err != nil {
		return bridgeType, err
	}

	if err := ConfigureARPCache(config.WeaveBridgeName); err != nil {
		return bridgeType, err
	}

	return bridgeType, nil
}

func initBridge(config *BridgeConfig) error {
	peerName, err := GetSystemPeerName(config.DBPrefix, config.HostRoot)
	if err != nil {
		return err
	}
	mac, err := net.ParseMAC(peerName)
	if err != nil {
		return err
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = config.WeaveBridgeName
	linkAttrs.HardwareAddr = mac
	linkAttrs.MTU = config.MTU // TODO this probably doesn't work - see weave script
	netlink.LinkAdd(&netlink.Bridge{LinkAttrs: linkAttrs})

	return nil
}

func initFastdp(config *BridgeConfig) error {
	datapath, err := netlink.LinkByName(config.DatapathName)
	if err != nil {
		return err
	}
	return netlink.LinkSetMTU(datapath, config.MTU)
}

func initBridgedFastdp(config *BridgeConfig) error {
	if err := initFastdp(config); err != nil {
		return err
	}
	if err := initBridge(config); err != nil {
		return err
	}

	link := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: "vethwe-bridge",
			MTU:  config.MTU},
		PeerName: "vethwe-datapath",
	}

	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	bridge, err := netlink.LinkByName(config.WeaveBridgeName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMasterByIndex(link, bridge.Attrs().Index); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if err := linkSetUpByName(link.PeerName); err != nil {
		return err
	}

	if err := odp.AddDatapathInterface(config.DatapathName, link.PeerName); err != nil {
		return err
	}

	if err := linkSetUpByName(config.DatapathName); err != nil {
		return err
	}

	return nil
}

func configureIPTables(config *BridgeConfig) error {
	return fmt.Errorf("Not implemented")
}

func linkSetUpByName(linkName string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}
