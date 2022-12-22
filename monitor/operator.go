package monitor

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/smartbch/cc-operator/client"
	"github.com/smartbch/cc-operator/sbch"
	sbchrpctypes "github.com/smartbch/smartbch/rpc/types"
)

const (
	OperatorCheckInterval = 1 * time.Minute
	CheckNodesEveryN      = 5
	ErrCountThreshold     = 3
)

type UtxoLists struct {
	RedeemingUtxosForOperators     []*sbchrpctypes.UtxoInfo
	RedeemingUtxosForMonitors      []*sbchrpctypes.UtxoInfo
	ToBeConvertedUtxosForOperators []*sbchrpctypes.UtxoInfo
	ToBeConvertedUtxosForMonitors  []*sbchrpctypes.UtxoInfo
}

type OperatorsWatcher struct {
	sbchClient       *sbch.SimpleRpcClient
	opClients        []*client.Client
	nodeCmpErrCounts []int
	utxoCmpErrCounts []int
}

func (watcher *OperatorsWatcher) checkErrCountAndSuspend(errCounts []int) {
	for i, errCount := range errCounts {
		if errCount > ErrCountThreshold {
			sig, ts := getSigAndTimestamp()
			watcher.opClients[i].Suspend(sig, ts)
		}
	}
}

func (watcher *OperatorsWatcher) MainLoop() {
	for i := 0; ; i++ {
		time.Sleep(OperatorCheckInterval)
		watcher.CheckUtxoLists()
		watcher.checkErrCountAndSuspend(watcher.utxoCmpErrCounts)

		if i%CheckNodesEveryN != 0 {
			continue
		}
		watcher.CheckNodes()
		watcher.checkErrCountAndSuspend(watcher.nodeCmpErrCounts)
	}
}

func (watcher *OperatorsWatcher) GetUtxoListsFromSbch() (utxoLists UtxoLists, err error) {
	utxoLists.RedeemingUtxosForOperators, err = watcher.sbchClient.GetRedeemingUtxosForOperators()
	if err != nil {
		return
	}
	utxoLists.RedeemingUtxosForMonitors, err = watcher.sbchClient.GetRedeemingUtxosForMonitors()
	if err != nil {
		return
	}
	utxoLists.ToBeConvertedUtxosForOperators, err = watcher.sbchClient.GetToBeConvertedUtxosForOperators()
	if err != nil {
		return
	}
	utxoLists.ToBeConvertedUtxosForMonitors, err = watcher.sbchClient.GetToBeConvertedUtxosForMonitors()
	if err != nil {
		return
	}
	return
}

func GetUtxoListsFromOperator(opClient *client.Client) (utxoLists UtxoLists, err error) {
	utxoLists.RedeemingUtxosForOperators, err = opClient.GetRedeemingUtxosForOperators()
	if err != nil {
		return
	}
	utxoLists.RedeemingUtxosForMonitors, err = opClient.GetRedeemingUtxosForMonitors()
	if err != nil {
		return
	}
	utxoLists.ToBeConvertedUtxosForOperators, err = opClient.GetToBeConvertedUtxosForOperators()
	if err != nil {
		return
	}
	utxoLists.ToBeConvertedUtxosForMonitors, err = opClient.GetToBeConvertedUtxosForMonitors()
	if err != nil {
		return
	}
	return
}

func (watcher *OperatorsWatcher) CheckUtxoLists() error {
	refUtxoLists, err := watcher.GetUtxoListsFromSbch()
	if err != nil {
		fmt.Println("GetUtxoListsFromSbch failed:", err)
		return err
	}

	for i, opClient := range watcher.opClients {
		utxoLists, err := GetUtxoListsFromOperator(opClient)
		if err != nil {
			fmt.Println("GetUtxoListsFromOperator failed:", err)
			continue
		}

		if !reflect.DeepEqual(refUtxoLists, utxoLists) {
			fmt.Println("utxoLists not match: ", opClient.RpcURL())
			watcher.utxoCmpErrCounts[i]++
		} else {
			watcher.utxoCmpErrCounts[i] = 0
		}
	}
	return nil
}

func (watcher *OperatorsWatcher) CheckNodes() error {
	latestNodes, err := watcher.sbchClient.GetSbchdNodes()
	if err != nil {
		fmt.Println("GetSbchdNodes failed:", err)
		return err
	}

	sortNodes(latestNodes)
	for i, opClient := range watcher.opClients {
		opInfo, err := opClient.GetInfo()
		if err != nil {
			fmt.Println("GetInfo from operator failed:", err)
			continue
		}

		currNodes := opInfo.CurrNodes
		newNodes := opInfo.NewNodes
		sortNodes(currNodes)
		sortNodes(newNodes)
		if !reflect.DeepEqual(latestNodes, currNodes) &&
			!reflect.DeepEqual(latestNodes, newNodes) {
			fmt.Println("nodes not match: ", opClient.RpcURL())
			watcher.nodeCmpErrCounts[i]++
		} else {
			watcher.nodeCmpErrCounts[i] = 0
		}
	}

	return nil
}

func sortNodes(nodes []sbch.NodeInfo) {
	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].PbkHash[:], nodes[j].PbkHash[:]) < 0
	})
}
