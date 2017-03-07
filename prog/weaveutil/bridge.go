package main

import (
	"fmt"
	"strconv"

	weavenet "github.com/weaveworks/weave/net"
)

func detectBridgeType(args []string) error {
	if len(args) != 2 {
		cmdUsage("detect-bridge-type", "<weave-bridge-name> <datapath-name>")
	}
	bridgeType := weavenet.DetectBridgeType(args[0], args[1])
	fmt.Println(bridgeType.String())
	return nil
}

func createBridge(args []string) error {
	if len(args) != 8 {
		cmdUsage("create-bridge", "<docker-bridge-name> <weave-bridge-name> <datapath-name> <mtu> <port> <mac> <no-fastdp> <no-bridged-fastdp>")
	}

	mtu, err := strconv.Atoi(args[3])
	if err != nil && args[3] != "" {
		return err
	}
	port, err := strconv.Atoi(args[4])
	if err != nil {
		return err
	}
	config := weavenet.BridgeConfig{
		DockerBridgeName: args[0],
		WeaveBridgeName:  args[1],
		DatapathName:     args[2],
		MTU:              mtu,
		Port:             port,
		Mac:              args[5],
		NoFastdp:         args[6] != "",
		NoBridgedFastdp:  args[7] != "",
	}
	bridgeType, err := weavenet.CreateBridge(&config)
	fmt.Println(bridgeType.String())
	return err
}

// TODO: destroy-bridge
