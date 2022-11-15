package monitor

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"

	"github.com/smartbch/cc-operator/client"
	"github.com/smartbch/cc-operator/sbch"
)

type OperatorsWatcher struct {
	sbchClient *sbch.SimpleRpcClient
	opClients  []*client.Client
}

func (watcher *OperatorsWatcher) Check() error {
	latestNodes, err := watcher.sbchClient.GetSbchdNodes()
	if err != nil {
		fmt.Println("GetSbchdNodes failed:", err)
		return err
	}

	sortNodes(latestNodes)
	for _, opClient := range watcher.opClients {
		currNodes, err := opClient.GetNodes()
		if err != nil {
			fmt.Println("GetNodes from operator failed:", err)
			continue
		}

		newNodes, err := opClient.GetNewNodes()
		if err != nil {
			fmt.Println("GetNewNodes from operator failed:", err)
			continue
		}

		sortNodes(currNodes)
		sortNodes(newNodes)
		if !reflect.DeepEqual(latestNodes, currNodes) &&
			!reflect.DeepEqual(latestNodes, newNodes) {

			fmt.Println("nodes not match: ", opClient.RpcURL())
			// TODO
		}
	}

	return nil
}

func sortNodes(nodes []sbch.NodeInfo) {
	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].PbkHash[:], nodes[j].PbkHash[:]) < 0
	})
}
