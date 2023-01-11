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
	opClients := make([]*opclient.Client, 1)
	opClients[0] = opclient.NewClient("https://3.1.26.210:8810", 10 * time.Second)

	/*
	opClients := make([]*opclient.Client, 8)
	opClients[0] = opclient.NewClient("https://3.1.26.210:8801", 10 * time.Second)
	opClients[1] = opclient.NewClient("https://3.1.26.210:8802", 10 * time.Second)
	opClients[2] = opclient.NewClient("https://3.1.26.210:8803", 10 * time.Second)
	opClients[3] = opclient.NewClient("https://3.1.26.210:8804", 10 * time.Second)
	opClients[4] = opclient.NewClient("https://3.1.26.210:8805", 10 * time.Second)
	opClients[5] = opclient.NewClient("https://3.1.26.210:8806", 10 * time.Second)
	opClients[6] = opclient.NewClient("https://3.1.26.210:8807", 10 * time.Second)
	opClients[7] = opclient.NewClient("https://3.1.26.210:8808", 10 * time.Second)
	*/

	watcher := NewOperatorsWatcher(sbchClient, opClients)

	/*
	errCounts := make([]int, len(opClients))
	for i := 0; i < 1; i++ {
		errCounts[i] = ErrCountThreshold + 1
	}
	watcher.checkErrCountAndSuspend(errCounts)
	*/

	watcher.suspendAll()
	//watcher.MainLoop()
	//db := OpenDB("sqlite.db")
	//watcher.CheckUtxoListsAgainstDB(db)
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

func (watcher *OperatorsWatcher) suspendAll() (okCount int) {
	for i := range watcher.opClients {
		fmt.Printf("[suspendAll] #%d\n", i)
		pubkeyHex, err := watcher.opClients[i].GetPubkeyBytes()
		if err != nil {
			fmt.Printf("[suspendAll] Failed to get pubkey from operator: %s\n", err.Error())
			continue
		}
		sig, ts := getSigAndTimestamp(string(pubkeyHex))
		result, err := watcher.opClients[i].Suspend(sig, ts)
		fmt.Printf("[suspendAll] Suspend Result %s err %#v\n", string(result), err)
		if err != nil {
			continue
		}
		info, err := watcher.opClients[i].GetInfo()
		if err != nil {
			fmt.Printf("[suspendAll] Error After Suspend #%d: %#v\n", i, err)
		} else {
			okCount++
			fmt.Printf("[suspendAll] After Suspend #%d: %#v\n", i, info)
		}
	}
	return
}

func (watcher *OperatorsWatcher) checkErrCountAndSuspend(errCounts []int) {
	for i, errCount := range errCounts {
		fmt.Printf("[checkErrCountAndSuspend] %d %d\n", i, errCount)
		if errCount > ErrCountThreshold {
			pubkeyHex, err := watcher.opClients[i].GetPubkeyBytes()
			if err != nil {
				fmt.Printf("[checkErrCountAndSuspend] Failed to get pubkey from operator: %s\n", err.Error())
				continue
			}
			sig, ts := getSigAndTimestamp(string(pubkeyHex))
			result, err := watcher.opClients[i].Suspend(sig, ts)
			fmt.Printf("[checkErrCountAndSuspend] Suspend Result %s err %#v\n", string(result), err)
			info, err := watcher.opClients[i].GetInfo()
			if err != nil {
				fmt.Printf("[checkErrCountAndSuspend] Error After Suspend #%d: %#v\n", i, err)
			} else {
				fmt.Printf("[checkErrCountAndSuspend] After Suspend #%d: %#v\n", i, info)
			}
		}
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

func (watcher *OperatorsWatcher) CheckUtxoListsAgainstDB(db *gorm.DB) *FatalError {
	fmt.Println("========== GetToBeConvertedUtxosForMonitors =============")
	utxoList, err := watcher.sbchClient.GetToBeConvertedUtxosForMonitors()
	if err != nil {
		panic(err)
	}
	if err := watcher.checkUtxoList(db, utxoList, "ToBeConverted", HandingOver); err != nil {
		//fmt.Printf("[CheckUtxoListsAgainstDB] Error %#v\n", err)
		return err
	}
	fmt.Println("========== GetRedeemableUtxos =============")
	utxoList, err = watcher.sbchClient.GetRedeemableUtxos()
	if err != nil {
		panic(err)
	}
	if err := watcher.checkUtxoList(db, utxoList, "Redeemable", Redeemable); err != nil {
		//fmt.Printf("[CheckUtxoListsAgainstDB] Error %#v\n", err)
		return err
	}
	fmt.Println("========== GetRedeemingUtxosForMonitors =============")
	utxoList, err = watcher.sbchClient.GetRedeemingUtxosForMonitors()
	if err != nil {
		panic(err)
	}
	if err := watcher.checkUtxoList(db, utxoList, "Redeeming|LostAndReturn", Redeeming); err != nil {
		//fmt.Printf("[CheckUtxoListsAgainstDB] Error %#v\n", err)
		return err
	}
	fmt.Println("========== GetLostAndFoundUtxos =============")
	utxoList, err = watcher.sbchClient.GetLostAndFoundUtxos()
	if err != nil {
		panic(err)
	}
	if err := watcher.checkUtxoList(db, utxoList, "LostAndFound", LostAndFound); err != nil {
		//fmt.Printf("[CheckUtxoListsAgainstDB] Error %#v\n", err)
		return err
	}
	return nil
}

func (watcher *OperatorsWatcher) checkUtxoList(db *gorm.DB, utxoList []*sbchrpctypes.UtxoInfo, typStr string, typ int) *FatalError {
	for _, utxo := range utxoList {
		var utxoInDB CcUtxo
		txid := hex.EncodeToString(utxo.Txid[:])
		result := db.First(&utxoInDB, "txid == ? AND vout == ?", txid, utxo.Index)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			s := fmt.Sprintf("[CheckUtxoListsAgainstDB] Cannot find UTXO %s %s-%d\n", typStr, txid, utxo.Index)
			return NewFatal(s)
		}
		mismatch := utxoInDB.Type != typ
		if typ == Redeeming {
			mismatch = utxoInDB.Type != Redeeming && utxoInDB.Type != RedeemingToDel && utxoInDB.Type != LostAndReturn && utxoInDB.Type != LostAndReturnToDel
		}
		if typ == HandingOver {
			mismatch = utxoInDB.Type != HandingOver && utxoInDB.Type != HandedOver
		}
		if mismatch {
			return NewFatal(fmt.Sprintf("[CheckUtxoListsAgainstDB] UTXO is not %s (it's %d) %s-%d\n", typStr, utxoInDB.Type, txid, utxo.Index))
		}
		if typ == Redeeming {
			hexTarget := hex.EncodeToString(utxo.RedeemTarget[:])
			if utxoInDB.RedeemTarget != hexTarget {
				s := fmt.Sprintf("[CheckUtxoListsAgainstDB] redeemtarget is not %s (it's %s) %s-%d\n",
					utxoInDB.RedeemTarget, hexTarget, txid, utxo.Index)
				return NewFatal(s)
			//} else {
			//	s := fmt.Sprintf("[CheckUtxoListsAgainstDB] redeemtarget is ok. %s (it's %s) %s-%d\n",
			//		utxoInDB.RedeemTarget, hexTarget, txid, utxo.Index)
			//	fmt.Println(s)
			}
		}
		fmt.Printf("db %d %d rpc %d %s-%d\n", utxoInDB.Type, utxoInDB.Amount, utxo.Amount, txid, utxo.Index)
	}
	return nil
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
