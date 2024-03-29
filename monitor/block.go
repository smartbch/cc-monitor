package monitor

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"math/big"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/rpcclient"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	mevmtypes "github.com/smartbch/moeingevm/types"
	ccabi "github.com/smartbch/smartbch/crosschain/abi"
        cctypes "github.com/smartbch/smartbch/crosschain/types"
	sbchrpcclient "github.com/smartbch/smartbch/rpc/client"
)

const (
	ScanSideChainInterval = 6 * time.Second
	SendHandleUtxoDelay   = 12 * 60 // 22 * 60
	RescanThreshold       = 3
	ThresholdIn24Hours    = 9_8000_0000 //1000_0000_0000 // 1000 BCH

	finalizeBlockCount = 1
)

var (
	NetParams          = &chaincfg.TestNet3Params //&chaincfg.MainNetParams
	CCContractAddress  = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x27, 0x14}
	afterDamageControl = false
)

func ScriptHashToAddr(h []byte) string {
	addr, err := bchutil.NewAddressScriptHashFromHash(h, NetParams)
	if err != nil {
		panic(err)
	}
	return addr.EncodeAddress()
}

type Block struct {
	Transactions []*Transaction `json:"transactions"`
}

type Transaction struct {
	Hash              string `json:"hash"`
	TransactionIndex  string `json:"transactionIndex"`
	Nonce             string `json:"nonce"`
	BlockHash         string `json:"blockHash"`
	BlockNumber       string `json:"blockNumber"`
	From              string `json:"from"`
	To                string `json:"to"`
	Value             string `json:"value"`
	GasPrice          string `json:"gasPrice"`
	Gas               string `json:"gas"`
	Input             string `json:"input"`
	CumulativeGasUsed string `json:"cumulativeGasUsed"`
	GasUsed           string `json:"gasUsed"`
	ContractAddress   string `json:"contractAddress"`
	Logs              []Log  `json:"logs"`
	LogsBloom         string `json:"logsBloom"`
	Status            string `json:"status"`
	StatusStr         string `json:"statusStr"`
	OutData           string `json:"outData"`
}

type Log struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	TxHash      string   `json:"txHash"`
	TxIndex     string   `json:"txIndex"`
	BlockHash   string   `json:"blockHash"`
	Index       string   `json:"index"`
	Removed     bool     `json:"removed"`
}

// A simple function. Just used during debug.
func SendStartRescanAndHandleUTXO(ctx context.Context, rawclient *rpc.Client, client *ethclient.Client, bchClient *rpcclient.Client, lastRescanHeight, lastRescanTime, handleUtxoDelay int64) {
	height := lastRescanHeight + 1
	sendHandleUtxo := false
	for {
		_, err := bchClient.GetBlockHash(height + int64(finalizeBlockCount))
		if err != nil {
			time.Sleep(30 * time.Second)
			fmt.Printf("get block %d hash err:%s\n", height+int64(finalizeBlockCount), err)
			continue
		}
		fmt.Printf("mainnet height:%d\n", height)
		if lastRescanHeight+2 <= height {
			callData := ccabi.PackStartRescanFunc(big.NewInt(height))
			txHash, err := sendTransaction(ctx, rawclient, client, MyAddress, CCAddress, callData)
			if err != nil {
				fmt.Printf("Error in sendStartRescanTransaction: %#v\n", err)
			}
			time.Sleep(12 * time.Second)
			err = checkTxStatus(ctx, client, txHash)
			if err != nil {
				fmt.Printf("startRescan executed failed: %s,%s\n", err.Error(), txHash.String())
			} else {
				fmt.Printf("startRescan executed success, %s\n", txHash.String())
				lastRescanHeight = height
				lastRescanTime = time.Now().Unix()
				sendHandleUtxo = true
			}
		}
		time.Sleep(30 * time.Second)
		if lastRescanTime+handleUtxoDelay < time.Now().Unix() && sendHandleUtxo {
			callData := ccabi.PackHandleUTXOsFunc()
			txHash, err := sendTransaction(ctx, rawclient, client, MyAddress, CCAddress, callData)
			if err != nil {
				fmt.Printf("Error in sendHandleUtxoTransaction: %#v\n", err)
			}
			// wait tx minted
			time.Sleep(12 * time.Second)
			err = checkTxStatus(ctx, client, txHash)
			if err != nil {
				fmt.Printf("handleUtxo executed failed: %s,%s\n", err.Error(), txHash.String())
			} else {
				fmt.Printf("handleUtxo executed success, %s\n", txHash.String())
				sendHandleUtxo = false
			}
		}
		height++
	}
}

func TriggerDamageControl(ctx context.Context, bs *BlockScanner, watcher *OperatorsWatcher, fatalErr *FatalError) {
	fmt.Printf("[TriggerDamageControl] %#v\n", fatalErr)
	afterDamageControl = true
	sendPauseTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient)
	okCount := watcher.suspendAll()
	fmt.Printf("[TriggerDamageControl] Suspended Operator Count: %#v\n", okCount)
}

// catch up the latest height of smartBCH
func Catchup(bs *BlockScanner) {
	ctx := context.Background()
	metaInfo := getMetaInfo(bs.db)
	fmt.Printf("[Before Catchup] MetaInfo %#v\n", metaInfo)
	endHeight, err := bs.ethClient.BlockNumber(ctx)
	if err != nil {
		panic(err)
	}
	for height := metaInfo.SideChainHeight; height <= int64(endHeight); height++ {
		timestamp, err := bs.GetBlockTime(ctx, height)
		if err != nil {
			fmt.Printf("[Catchup] Error in GetBlockTime: %s\n", err.Error())
			continue
		}
		fmt.Printf("[Catchup] SideChain BlockTime %d Height %d endHeight %d\n", timestamp, height, endHeight)
		err = bs.ScanBlock(ctx, timestamp, height)
		if err != nil {
			fmt.Printf("[Catchup] Error in ScanBlock: %s\n", err.Error())
		}
		endHeight, err = bs.ethClient.BlockNumber(ctx)
		if err != nil {
			panic(err)
		}
	}
	fmt.Printf("[Catchup] Finished, endHeight = %d\n", endHeight)
}

func PrintTotalAmount(totalAmount TotalAmount, info cctypes.CCInfosForTest) {
	div := big.NewInt(10_000_000_000)
	totalRedeemAmountS2M := big.NewInt(0).Div(hexutil.MustDecodeBig(info.TotalRedeemAmountS2M), div).Int64()
	totalLostAndFoundAmountS2M := big.NewInt(0).Div(hexutil.MustDecodeBig(info.TotalLostAndFoundAmountS2M), div).Int64()
	totalTransferAmountM2S := big.NewInt(0).Div(hexutil.MustDecodeBig(info.TotalTransferAmountM2S), div).Int64()
	totalTransferByBurnAmount := big.NewInt(0).Div(hexutil.MustDecodeBig(info.TotalTransferByBurnAmount), div).Int64()

	fmt.Printf("====== TotalAmount =======\n")
	diff := totalAmount.TotalRedeemAmountS2M - totalRedeemAmountS2M
	fmt.Printf("TotalRedeemAmountS2M       %d rpc %d %d\n", totalAmount.TotalRedeemAmountS2M, totalRedeemAmountS2M, diff)
	diff = totalAmount.TotalRedeemNumsS2M - int64(info.TotalRedeemNumsS2M)
	fmt.Printf("TotalRedeemNumsS2M         %d rpc %d %d\n", totalAmount.TotalRedeemNumsS2M, info.TotalRedeemNumsS2M, diff)

	diff = totalAmount.TotalLostAndFoundAmountS2M - totalLostAndFoundAmountS2M
	fmt.Printf("TotalLostAndFoundAmountS2M %d rpc %d %d\n", totalAmount.TotalLostAndFoundAmountS2M, totalLostAndFoundAmountS2M, diff)
	diff = totalAmount.TotalLostAndFoundNumsS2M - int64(info.TotalLostAndFoundNumsS2M)
	fmt.Printf("TotalLostAndFoundNumsS2M   %d rpc %d %d\n", totalAmount.TotalLostAndFoundNumsS2M, info.TotalLostAndFoundNumsS2M, diff)

	diff = totalAmount.TotalTransferAmountM2S - totalTransferAmountM2S
	fmt.Printf("TotalTransferAmountM2S     %d rpc %d %d\n", totalAmount.TotalTransferAmountM2S, totalTransferAmountM2S, diff)
	diff = totalAmount.TotalTransferNumsM2S - int64(info.TotalTransferNumsM2S)
	fmt.Printf("TotalTransferNumsM2S       %d rpc %d %d\n", totalAmount.TotalTransferNumsM2S, info.TotalTransferNumsM2S, diff)

	diff = totalAmount.TotalTransferByBurnAmount - totalTransferByBurnAmount
	fmt.Printf("TotalTransferByBurnAmount     %d rpc %d %d\n", totalAmount.TotalTransferByBurnAmount, totalTransferByBurnAmount, diff)
	diff = totalAmount.TotalTransferByBurnNums - int64(info.TotalTransferByBurnNums)
	fmt.Printf("TotalTransferByBurnNums       %d rpc %d %d\n", totalAmount.TotalTransferByBurnNums, info.TotalTransferByBurnNums, diff)
}

func SendPauseAndResume(bs *BlockScanner) {
	ctx := context.Background()
	fmt.Printf("====== Pause ========\n")
	sendPauseTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient)
	fmt.Printf("====== Resume ========\n")
	sendResumeTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient)
}

func MainLoop(bs *BlockScanner, watcher *OperatorsWatcher) {
	ctx := context.Background()
	metaInfo := getMetaInfo(bs.db)
	height := metaInfo.SideChainHeight + 1
	for i := 0; ; i++ { // process sidechain blocks
		time.Sleep(ScanSideChainInterval)
		ccInfo, err := bs.sbchClient.CcInfo(ctx)
		if err != nil {
			fmt.Printf("[MainLoop] Error in CcInfo %s\n", err.Error())
			continue
		}
		fmt.Printf("[MainLoop] now: %d ccInfo: %#v\n", time.Now().Unix(), ccInfo)

		if ccInfo.RescanTime > 0 && ccInfo.RescanTime+SendHandleUtxoDelay < time.Now().Unix() && !ccInfo.UTXOAlreadyHandled {
			sendHandleUtxoTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient) // it will loop util succeed
		}

		metaInfo = getMetaInfo(bs.db) // MetaInfo may get updated during processing, so we reload it
		timestamp, err := bs.GetBlockTime(ctx, height)
		if err != nil {
			fmt.Printf("[MainLoop] Error in GetBlockTime: %s\n", err.Error())
			continue
		}

		bs.CheckSlidingWindow(ctx, timestamp, &metaInfo)

		// check mainchain's new blocks every 20 sidechain blocks
		if height%20 == 0 {
			err, endRescanHeight := bs.CheckMainChainForRescan(metaInfo)
			if err != nil {
				if fatalErr, ok := err.(*FatalError); ok {
					TriggerDamageControl(ctx, bs, watcher, fatalErr)
				} else {
					fmt.Printf("[MainLoop] Error in CheckMainChainForRescan: %s\n", err.Error())
				}
			}
			if endRescanHeight != 0 {
				sendStartRescanTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient, endRescanHeight)
			}
		}
		for {
			endHeight, err := bs.ethClient.BlockNumber(ctx)
			if err != nil {
				fmt.Printf("[MainLoopLoop] Err in BlockNumber %#v\n", err)
			}
			if height > int64(endHeight) {
				break
			}
			fmt.Printf("[MainLoopLoop] Before ScanSideChainBlock %d %d\n", height, endHeight)
			// scan sidechain's blocks
			err = bs.ScanBlock(ctx, timestamp, height)
			if err != nil {
				if fatalErr, ok := err.(*FatalError); ok {
					TriggerDamageControl(ctx, bs, watcher, fatalErr)
				} else {
					fmt.Printf("[MainLoopLoop] Error in bs.ScanBlock: %s\n", err.Error())
				}
			}
			height++
		}
		if i % 10 == 0 {
			totalAmount := getTotalAmount(bs.db)
			ccInfosForTest := GetCcInfosForTest(ctx, bs.rpcClient)
			PrintTotalAmount(totalAmount, ccInfosForTest)
		}
		if i % 100 == 0 {
			fmt.Println("[MainLoop] watcher.CheckUtxoLists")
			watcher.CheckUtxoLists()
		}
		if i % 500 == 5 {
			fmt.Println("[MainLoop] watcher.CheckNodes")
			watcher.CheckNodes()
		}
		if i % 20 == 2 {
			fmt.Println("[MainLoop] watcher.CheckUtxoListsAgainstDB")
			if err := watcher.CheckUtxoListsAgainstDB(bs.db); err != nil {
				TriggerDamageControl(ctx, bs, watcher, err)
			}
		}
	}
}

func reverse(in []byte) []byte {
	out := make([]byte, len(in))
	for i, c := range in {
		out[len(out)-1-i] = c
	}
	return out
}

func inUtxoSet(utxoSet map[string]struct{}, txin *wire.TxIn) bool {
	key := fmt.Sprintf("%s-%d", hex.EncodeToString(reverse(txin.PreviousOutPoint.Hash[:])),
		txin.PreviousOutPoint.Index)
	_, ok := utxoSet[key]
	return ok
}

// accumulate the cc-transactions on main chain
type CCTxCounter struct {
	currCovenantAddr string
	client           *rpcclient.Client
	ccTxCount        int64
	utxoSet          map[string]struct{}
}

// if the block at 'blockHeight' is finalized, analyze its transactions to increase 'ccTxCount'
// if the block exists (no matter finalized or not), check evil transactions in it.
func (txc *CCTxCounter) CheckMainChainBlock(gormTx *gorm.DB, blockHeight int64) (err error, isFinalized bool) {
	_, err = txc.client.GetBlockHash(blockHeight + finalizeBlockCount) // is this block finalized?
	isFinalized = err == nil
	fmt.Printf("[CheckMainChainBlock] height %d isFinalized %v\n", blockHeight, isFinalized)
	hash, err := txc.client.GetBlockHash(blockHeight) // even if it is not finalized, we still need checkEvilTx
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight, isFinalized
	}
	hash, err = txc.client.GetBlockHash(blockHeight)
	if err != nil {
		panic(err) //impossible
	}
	blk, err := txc.client.GetBlock(hash) // Get this block's transactions
	if err != nil {
		panic(err) //impossible
	}
	if isFinalized {
		for _, bchTx := range blk.Transactions {
			if txc.isCCTxToSmartBCH(bchTx) {
				txc.ccTxCount++ // this is a cross-chain tx to smartbch
			}
		}
	}
	return txc.checkEvilTx(gormTx, blk), isFinalized
}

// If an evil transaction is sent, then very bad thing happend: operators' private keys are stolen
func (txc *CCTxCounter) checkEvilTx(gormTx *gorm.DB, blk *wire.MsgBlock) error {
	for _, bchTx := range blk.Transactions {
		h := bchTx.TxHash()
		txid := hex.EncodeToString(reverse(h[:]))
		for _, txin := range bchTx.TxIn {
			if !inUtxoSet(txc.utxoSet, txin) {
				continue
			}
			isValidFormat := len(bchTx.TxIn) == 1 && len(bchTx.TxOut) == 1
			if !isValidFormat {
				debug.PrintStack()
				return NewFatal(fmt.Sprintf("[checkEvilTx] bad mainchain format %#v\n", bchTx))
			}
			scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(bchTx.TxOut[0].PkScript, NetParams)
			if err != nil || len(addrs) != 1 {
				debug.PrintStack()
				return NewFatal(fmt.Sprintf("[checkEvilTx] cannot parse output %#v\n", bchTx))
			}
			oldTxid := hex.EncodeToString(reverse(txin.PreviousOutPoint.Hash[:]))
			oldVout := txin.PreviousOutPoint.Index
			if scrClass == txscript.ScriptHashTy { // P2SH
				addr := addrs[0].ScriptAddress()
				fmt.Printf("[checkEvilTx] mainEvtFinishConverting %s-%d => %s-%d\n", oldTxid, oldVout, txid, 0)
				err = mainEvtFinishConverting(gormTx, oldTxid, oldVout, string(addr[:]), txid, 0, false)
			} else if scrClass == txscript.PubKeyHashTy { // P2PKH
				addr := addrs[0].ScriptAddress()
				fmt.Printf("[checkEvilTx] mainEvtRedeemOrReturn %s-%d in %s\n", oldTxid, oldVout, txid)
				err = mainEvtRedeemOrReturn(gormTx, oldTxid, oldVout, string(addr[:]), false)
			} else {
				debug.PrintStack()
				return NewFatal(fmt.Sprintf("[checkEvilTx] invalid srcClass %#v\n", bchTx))
			}
			if err != nil {
				fmt.Printf("[EvilTx!] Error: %#v\n", err)
				return err
			}
		}
	}
	return nil
}

func (txc *CCTxCounter) isCCTxToSmartBCH(bchTx *wire.MsgTx) bool {
	for _, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, NetParams)
		if err != nil || len(addrs) != 1 {
			continue
		}
		addr := addrs[0].ScriptAddress()
		if scrClass == txscript.ScriptHashTy && string(addr[:]) == txc.currCovenantAddr { //P2SH
			return true
		}
	}
	return false
}

// Watches the blocks of main chain
type BlockWatcher struct {
	db               *gorm.DB
	client           *rpcclient.Client
	utxoSet          map[string]struct{}
	currCovenantAddr string
	lastCovenantAddr string
}

func (bw *BlockWatcher) handleMainChainTx(gormTx *gorm.DB, bchTx *wire.MsgTx) (bool, error) {
	h := bchTx.TxHash()
	txid := hex.EncodeToString(reverse(h[:]))
	dbChanged := false
	// convert: one-vin in utxoSet one-vout with p2sh (newCovenantAddr)
	// redeem&return: one-vin in utxoSet one-vout with p2pkh
	if len(bchTx.TxIn) == 1 && len(bchTx.TxOut) == 1 && inUtxoSet(bw.utxoSet, bchTx.TxIn[0]) {
		dbChanged = true
		txin := bchTx.TxIn[0]
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(bchTx.TxOut[0].PkScript, NetParams)
		if err != nil || len(addrs) != 1 {
			debug.PrintStack()
			return false, NewFatal(fmt.Sprintf("[EVIL] cannot parse output %#v\n", bchTx))
		}
		oldTxid := hex.EncodeToString(reverse(txin.PreviousOutPoint.Hash[:]))
		oldVout := txin.PreviousOutPoint.Index
		if scrClass == txscript.ScriptHashTy { // P2SH
			addr := addrs[0].ScriptAddress()
			fmt.Printf("[mainEvtFinishConverting] %s-%d => %s-%d\n", oldTxid, oldVout, txid, 0)
			err = mainEvtFinishConverting(gormTx, oldTxid, oldVout, string(addr[:]), txid, 0, true)
		} else if scrClass == txscript.PubKeyHashTy { //P2PKH
			addr := addrs[0].ScriptAddress()
			fmt.Printf("[mainEvtRedeemOrReturn] %s-%d in %s\n", oldTxid, oldVout, txid)
			err = mainEvtRedeemOrReturn(gormTx, oldTxid, oldVout, string(addr[:]), true)
		} else {
			debug.PrintStack()
			return false, NewFatal(fmt.Sprintf("[EVIL] invalid srcClass %#v\n", bchTx))
		}
		if err != nil {
			//fmt.Printf("[handleMainChainTx] Error: %#v\n", err)
			return false, err
		}
	}
	// addToBeRecognized: one-vout with p2sh, maybe one-vout with opreturn
	covenantAddr := ScriptHashToAddr([]byte(bw.currCovenantAddr))
	for vout, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, NetParams)
		if err != nil || len(addrs) != 1 {
			continue
		}
		addr := addrs[0].ScriptAddress()
		isAddToBeRecognized := scrClass == txscript.ScriptHashTy && (
			string(addr[:]) == bw.currCovenantAddr || string(addr[:]) == bw.lastCovenantAddr)
		if scrClass == txscript.ScriptHashTy {
			fmt.Printf("[MainChainTx] P2SH_ADDR: %s vs %s AddToBeRecognized %v\n", covenantAddr, addrs[0].EncodeAddress(), isAddToBeRecognized)
			fmt.Printf("  (%s vs %s/%s)\n", hex.EncodeToString(addr[:]), hex.EncodeToString([]byte(bw.currCovenantAddr)), hex.EncodeToString([]byte(bw.lastCovenantAddr)))
			fmt.Printf("  txid: %s\n", txid)
		}
		if isAddToBeRecognized {
			dbChanged = true
			ccUtxo := CcUtxo{
				Type:         ToBeRecognized,
				CovenantAddr: string(addr[:]),
				Amount:       txout.Value,
				Txid:         txid,
				Vout:         uint32(vout),
			}
			if err = addToBeRecognized(gormTx, ccUtxo); err != nil {
				return false, err
			}
		}
	}
	return dbChanged, nil
}

func (bw *BlockWatcher) HandleMainChainBlock(blockHeight int64) error {
	hash, err := bw.client.GetBlockHash(blockHeight)
	fmt.Printf("[HandleMainChainBlock] height %d hash %#v\n", blockHeight, hash)
	if err != nil { // block not ready
		return ErrNoBlockFoundAtGivenHeight
	}
	blk, err := bw.client.GetBlock(hash)
	if err != nil {
		panic(err) //impossible
	}
	dbChanged := false
	err = bw.db.Transaction(func(gormTx *gorm.DB) error { // One DB-Transaction to update UTXO set and height
		for _, bchTx := range blk.Transactions {
			changed, err := bw.handleMainChainTx(gormTx, bchTx)
			dbChanged = dbChanged || changed
			if err != nil {
				return err
			}
		}
		return updateMainChainHeight(gormTx, blockHeight)
	})
	if dbChanged {
		fmt.Printf("=== AfterMain UTXOs {\n")
		printUtxoSet(bw.db)
		fmt.Printf("} // AfterMain UTXOs\n")
	}
	return err
}

// ==================

// Scan blocks of side chain and main chain
type BlockScanner struct {
	bchClient  *rpcclient.Client
	db         *gorm.DB
	rpcClient  *rpc.Client
	ethClient  *ethclient.Client
	abi        abi.ABI
	sbchClient *sbchrpcclient.Client
}

func NewBlockScanner(bchClient *rpcclient.Client, db *gorm.DB, sideChainUrl string) *BlockScanner {
	rpcClient, err := rpc.Dial(sideChainUrl)
	if err != nil {
		panic(err)
	}
	ethClient, err := ethclient.Dial(sideChainUrl)
	if err != nil {
		panic(err)
	}
        sbchClient, err := sbchrpcclient.Dial(sideChainUrl)
        if err != nil {
                panic(err)
        }

	return &BlockScanner{
		bchClient:  bchClient,
		db:         db,
		rpcClient:  rpcClient,
		ethClient:  ethClient,
		abi:        ccabi.ABI.GetABI(),
		sbchClient: sbchClient,
	}

}

// according to the sum in sliding window, pause/resume the shagate logic
func (bs *BlockScanner) CheckSlidingWindow(ctx context.Context, timestamp int64, metaInfo *MetaInfo) {
	sum := metaInfo.getSumInSlidingWindow(timestamp)
	fmt.Printf("[CheckSlidingWindow] sum %d ThresholdIn24Hours %d afterDamageControl %v\n", sum, ThresholdIn24Hours, afterDamageControl)
	if sum > ThresholdIn24Hours {
		sendPauseTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient)
	}
	if sum < ThresholdIn24Hours && !afterDamageControl {
		sendResumeTransaction(ctx, bs.rpcClient, bs.ethClient, bs.sbchClient)
	}
}

func (bs *BlockScanner) CheckMainChainForRescan(metaInfo MetaInfo) (err error, lastFinalizedHeight int64) {
	height := metaInfo.ScannedHeight + 1
	txCounter := &CCTxCounter{
		currCovenantAddr: metaInfo.CurrCovenantAddr,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db, false), // get all the UTXOs for checkEvilTx
	}
	for {
		err, isFinalized := txCounter.CheckMainChainBlock(bs.db, height)
		if isFinalized {
			lastFinalizedHeight = height
		}
		if errors.Is(err, ErrNoBlockFoundAtGivenHeight) { // no new blocks
			break
		}
		if err != nil {
			return err, 0
		}
		height++ //try to find the next new block
	}
	needRescan := txCounter.ccTxCount > 0 || metaInfo.ScannedHeight+RescanThreshold < lastFinalizedHeight
	fmt.Printf("[CheckMainChainForRescan] needRescan=%v txCounter %d ScannedHeight %d LatestHeight %d\n", needRescan, txCounter.ccTxCount, metaInfo.ScannedHeight, height)
	if needRescan {
		return nil, lastFinalizedHeight
	}
	return nil, 0
}

func (bs *BlockScanner) GetBlockTime(ctx context.Context, blockHeight int64) (int64, error) {
	var raw json.RawMessage
	err := bs.rpcClient.CallContext(ctx, &raw, "eth_getBlockByNumber", hexutil.EncodeUint64(uint64(blockHeight)), false)
	if err != nil {
		return 0, err
	}
	var head *gethtypes.Header
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, err
	}
	return int64(head.Time), nil
}

func (bs *BlockScanner) ScanBlock(ctx context.Context, timestamp, blockHeight int64) error {
	var block Block
	err := bs.rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", hexutil.EncodeUint64(uint64(blockHeight)), true)
	if err != nil {
		return err
	}
	fmt.Printf("[ScanSideChainBlock] Height %d\n", blockHeight)
	txList := block.Transactions
	txToBeParsed := make([]*Transaction, 0, len(txList))
	for _, tx := range txList {
		if common.HexToAddress(tx.To) != CCContractAddress {
			continue
		}
		//var tmp map[string]any
		//bs.rpcClient.CallContext(ctx, &tmp, "eth_getTransactionReceipt", tx.Hash)
		//fmt.Printf("DBG tx %#v\n", tmp)
		err := bs.rpcClient.CallContext(ctx, tx, "eth_getTransactionReceipt", tx.Hash)
		if err != nil {
			return err
		}
		input := hexutil.MustDecode(tx.Input)
		status := hexutil.MustDecodeUint64(tx.Status)
		if len(input) < 4 {
			continue
		}
		method, _ := bs.abi.MethodById(input[:4])
		fmt.Printf("[CC-Contract] tx %#v\n method: %#v input: %#v status %d\n", tx, method, input, status)
		if method == nil {
			continue
		}
		if status != mevmtypes.ReceiptStatusSuccessful {
			continue
		}
		if method.Name == "handleUTXOs" {
			updateLastRescanTime(bs.db, -1) // -1 means no pending startRescan needs to run handleUTXOs
		} else if method.Name == "startRescan" && len(input) == 36 {
			bs.parseStartRescan(ctx, timestamp, tx, input)
		}
		txToBeParsed = append(txToBeParsed, tx)
	}
	metaInfo := getMetaInfo(bs.db)
	err = bs.db.Transaction(func(gormTx *gorm.DB) error { // One DB-Transaction to update UTXO set and height
		for _, tx := range txToBeParsed {
			err := bs.processReceipt(gormTx, tx, &metaInfo, int64(timestamp))
			if err != nil {
				return err
			}
		}
		metaInfo.SideChainHeight = blockHeight
		gormTx.Model(&metaInfo).Updates(metaInfo)
		return nil
	})
	if len(txToBeParsed) != 0 {
		fmt.Printf("===== AfterSide UTXOs {\n")
		printUtxoSet(bs.db)
		fmt.Printf("} // AfterSide UTXOs\n")
	}
	return err
}

func (bs *BlockScanner) parseStartRescan(ctx context.Context, timestamp int64, tx *Transaction, input []byte) {
	start := 4 + 32 - 8
	if len(input) < start {
		return
	}
	mainChainHeight := int64(binary.BigEndian.Uint64(input[start:]))
	metaInfo := getMetaInfo(bs.db)
	fmt.Printf("[parseStartRescan] StopHeight %d metaInfo %#v\n", mainChainHeight, metaInfo)
	blockCount, err := bs.bchClient.GetBlockCount()
	fmt.Printf("[parseStartRescan] MainChainLatestHeight %d err %#v\n", blockCount, err)
	watcher := &BlockWatcher{
		db:               bs.db,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db, true), // only the UTXOs waiting to be moved on mainchain
		currCovenantAddr: metaInfo.CurrCovenantAddr,
		lastCovenantAddr: metaInfo.LastCovenantAddr,
	}
	for h := metaInfo.MainChainHeight + 1; h <= mainChainHeight; h++ {
		watcher.HandleMainChainBlock(h)
	}
	bs.db.Model(&metaInfo).Updates(MetaInfo{ScannedHeight: mainChainHeight, LastRescanTime: int64(timestamp)})
}

type ConvertParams struct {
	Txid         common.Hash
	Vout         uint32
	CovenantAddr common.Address
}

func (bs *BlockScanner) processReceipt(gormTx *gorm.DB, tx *Transaction, metaInfo *MetaInfo, currTime int64) error {
	fmt.Printf("[processReceipt] tx %#v\n", tx)
	input := hexutil.MustDecode(tx.Input)
	for _, log := range tx.Logs {
		data := hexutil.MustDecode(log.Data)
		switch common.HexToHash(log.Topics[0]) {
		//NewRedeemable(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr);
		case EventNewRedeemable:
			txid := log.Topics[1][2:]
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			err := sideEvtRedeemable(gormTx, string(addr[:]), txid, uint32(vout))
			fmt.Printf("[sideEvtRedeemable] %s\n", log.Topics[1])
			if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
			}
		//NewLostAndFound(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr);
		case EventNewLostAndFound:
			txid := log.Topics[1][2:]
			fmt.Printf("[sideEvtLostAndFound] %s\n", txid)
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			err := sideEvtLostAndFound(gormTx, string(addr[:]), txid, uint32(vout))
			if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
			}
		//Redeem(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventRedeem:
			txid := log.Topics[1][2:]
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			redeemTarget := BurnAddressMainChain // transfer-by-burning
			//function redeem(uint256 txid, uint256 index, address targetAddress) external payable;
			if len(input) > 96 {
				redeemTarget = hex.EncodeToString(input[4+32+32+12:])
			}
			fmt.Printf("[sideEvtRedeem] %s target %s source %d\n", txid, redeemTarget, int(data[31]))
			err := sideEvtRedeem(gormTx, string(addr[:]), txid, uint32(vout), data[31], redeemTarget,
				metaInfo, currTime)
			if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
			}
		//ChangeAddr(address indexed oldCovenantAddr, address indexed newCovenantAddr);
		case EventChangeAddr:
			fmt.Printf("[sideEvtChangeAddr] old %s new %s\n", log.Topics[1][2+12*2:], log.Topics[2][2+12*2:])
			oldCovenantAddr := common.HexToAddress(log.Topics[1][2+12*2:])
			newCovenantAddr := common.HexToAddress(log.Topics[2][2+12*2:])
			if metaInfo.CurrCovenantAddr != string(oldCovenantAddr[:]) {
				return NewFatal(fmt.Sprintf("[sideEvtChangeAddr] Wrong oldCovenantAddr: %s (should be %s)",
					log.Topics[1][2+12*2:], hex.EncodeToString([]byte(metaInfo.CurrCovenantAddr))))
			}
			metaInfo.LastCovenantAddr = string(oldCovenantAddr[:])
			metaInfo.CurrCovenantAddr = string(newCovenantAddr[:])
			err := sideEvtChangeAddr(gormTx, string(oldCovenantAddr[:]), string(newCovenantAddr[:]))
			if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
			}
		//Convert(uint256 indexed prevTxid, uint32 indexed prevVout, address indexed oldCovenantAddr, uint256 txid, uint32 vout, address newCovenantAddr);
		case EventConvert:
                         prevTxid := log.Topics[1][2:]
                         prevVout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
                         txid := hex.EncodeToString(data[:32])
                         vout := binary.BigEndian.Uint32(data[32+28:32+32])
                         newCovenantAddr := string(data[64+12:64+32])
                         fmt.Printf("[sideEvtConvert] txid %s vout %d newCovenantAddr %s\n", txid, vout, hex.EncodeToString([]byte(newCovenantAddr)))
                         err := sideEvtConvert(gormTx, prevTxid[:], uint32(prevVout), txid, vout, newCovenantAddr)
                         if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
                         }
		//Deleted(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventDeleted:
			txid := log.Topics[1][2:]
			fmt.Printf("[sideEvtDeleted] %s\n", txid)
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			err := sideEvtDeleted(gormTx, string(addr[:]), txid, uint32(vout), data[31])
			if err != nil {
				//fmt.Printf("[processReceipt] Error %#v\n", err)
				return err
			}
		}
	}
	return nil
}
