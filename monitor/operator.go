package monitor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"gorm.io/gorm"
	opclient "github.com/smartbch/cc-operator/client"
	"github.com/smartbch/cc-operator/sbch"
	sbchrpctypes "github.com/smartbch/smartbch/rpc/types"
)

const (
	ErrCountThreshold = 3
)

type UtxoLists struct {
	RedeemingUtxosForOperators     []*sbchrpctypes.UtxoInfo
	RedeemingUtxosForMonitors      []*sbchrpctypes.UtxoInfo
	ToBeConvertedUtxosForOperators []*sbchrpctypes.UtxoInfo
	ToBeConvertedUtxosForMonitors  []*sbchrpctypes.UtxoInfo
}

type OperatorsWatcher struct {
	sbchClient       *sbch.SimpleRpcClient
	opClients        []*opclient.Client
	nodeCmpErrCounts []int
	utxoCmpErrCounts []int
}

func DebugWatcher() {
	sbchClient, err := sbch.NewSimpleRpcClient("0x4fE159925585EB891bf165d5ee7945bd871F3A7B",
		"http://18.141.161.139:8545", 10 * time.Second)
	if err != nil {
		panic(err)
	}
	opClients := make([]*opclient.Client, 8)
	opClients[0] = opclient.NewClient("https://3.1.26.210:8801", 10 * time.Second)
	opClients[1] = opclient.NewClient("https://3.1.26.210:8802", 10 * time.Second)
	opClients[2] = opclient.NewClient("https://3.1.26.210:8803", 10 * time.Second)
	opClients[3] = opclient.NewClient("https://3.1.26.210:8804", 10 * time.Second)
	opClients[4] = opclient.NewClient("https://3.1.26.210:8805", 10 * time.Second)
	opClients[5] = opclient.NewClient("https://3.1.26.210:8806", 10 * time.Second)
	opClients[6] = opclient.NewClient("https://3.1.26.210:8807", 10 * time.Second)
	opClients[7] = opclient.NewClient("https://3.1.26.210:8808", 10 * time.Second)

	watcher := NewOperatorsWatcher(sbchClient, opClients)
	//watcher.MainLoop()
	db := OpenDB("sqlite.db")
	watcher.CheckUtxoListsAgainstDB(db)
}


func NewOperatorsWatcher(sbchClient *sbch.SimpleRpcClient, opClients []*opclient.Client) *OperatorsWatcher {
	res := &OperatorsWatcher{
		sbchClient: sbchClient,
		opClients:  opClients,
	}
	res.nodeCmpErrCounts = make([]int, len(opClients))
	res.utxoCmpErrCounts = make([]int, len(opClients))
	return res
}

func (watcher *OperatorsWatcher) checkErrCountAndSuspend(errCounts []int) {
	for i, errCount := range errCounts {
		fmt.Printf("checkErrCountAndSuspend %d %d\n", i, errCount)
		//if errCount > ErrCountThreshold {
		//	pubkeyHex, err := watcher.opClients[i].GetPubkeyBytes()
		//	if err != nil {
		//		fmt.Printf("Failed to get pubkey from operator: %s\n", err.Error())
		//		continue
		//	}
		//	sig, ts := getSigAndTimestamp(string(pubkeyHex))
		//	watcher.opClients[i].Suspend(sig, ts)
		//}
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

func GetUtxoListsFromOperator(opClient *opclient.Client) (utxoLists UtxoLists, err error) {
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

func (watcher *OperatorsWatcher) CheckUtxoListsAgainstDB(db *gorm.DB) {
	fmt.Println("========== GetToBeConvertedUtxosForMonitors =============")
	utxoList, err := watcher.sbchClient.GetToBeConvertedUtxosForMonitors()
	if err != nil {
		panic(err)
	}
	watcher.checkUtxoList(db, utxoList)
	fmt.Println("========== GetRedeemingUtxosForMonitors =============")
	utxoList, err = watcher.sbchClient.GetRedeemingUtxosForMonitors()
	if err != nil {
		panic(err)
	}
	watcher.checkUtxoList(db, utxoList)
	fmt.Println("========== GetRedeemableUtxos =============")
	utxoList, err = watcher.sbchClient.GetRedeemableUtxos()
	if err != nil {
		panic(err)
	}
	watcher.checkUtxoList(db, utxoList)
}

func (watcher *OperatorsWatcher) checkUtxoList(db *gorm.DB, utxoList []*sbchrpctypes.UtxoInfo) {
	for _, utxo := range utxoList {
		var utxoInDB CcUtxo
		txid := hex.EncodeToString(utxo.Txid[:])
		result := db.First(&utxoInDB, "txid == ? AND vout == ?", txid, utxo.Index)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			fmt.Printf("ErrRecordNotFound %s-%d\n", txid, utxo.Index)
			continue
		}
		fmt.Printf("db %d %d rpc %d %s-%d\n", utxoInDB.Type, utxoInDB.Amount, utxo.Amount, txid, utxo.Index)
	}
}

func (watcher *OperatorsWatcher) CheckUtxoLists() error {
	refUtxoLists, err := watcher.GetUtxoListsFromSbch()
	if err != nil {
		fmt.Println("GetUtxoListsFromSbch failed:", err)
		return err
	}

	fmt.Printf("refUtxoLists: %#v\n", refUtxoLists)
	for i, opClient := range watcher.opClients {
		utxoLists, err := GetUtxoListsFromOperator(opClient)
		if err != nil {
			fmt.Println("GetUtxoListsFromOperator failed:", err)
			continue
		}
		fmt.Printf("utxoLists: %#v\n", utxoLists)

		if !reflect.DeepEqual(refUtxoLists, utxoLists) {
			fmt.Println("utxoLists not match: ", opClient.RpcURL())
			watcher.utxoCmpErrCounts[i]++
		} else {
			watcher.utxoCmpErrCounts[i] = 0
		}
	}
	watcher.checkErrCountAndSuspend(watcher.utxoCmpErrCounts)
	return nil
}

func (watcher *OperatorsWatcher) CheckNodes() error {
	latestNodes, err := watcher.sbchClient.GetSbchdNodes()
	if err != nil {
		fmt.Println("GetSbchdNodes failed:", err)
		return err
	}

	sortNodes(latestNodes)
	fmt.Printf("latestNodes %#v\n", latestNodes)
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
		fmt.Printf("currNodes %#v\n", currNodes)
		fmt.Printf("newNodes %#v\n", newNodes)
		if !reflect.DeepEqual(latestNodes, currNodes) &&
			!reflect.DeepEqual(latestNodes, newNodes) {
			fmt.Println("nodes not match: ", opClient.RpcURL())
			watcher.nodeCmpErrCounts[i]++
		} else {
			watcher.nodeCmpErrCounts[i] = 0
		}
	}

	watcher.checkErrCountAndSuspend(watcher.nodeCmpErrCounts)
	return nil
}

func sortNodes(nodes []sbch.NodeInfo) {
	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].PbkHash[:], nodes[j].PbkHash[:]) < 0
	})
}
