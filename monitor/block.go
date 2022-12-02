package monitor

import (
	"context"
	"runtime/debug"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"math/big"
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
	sbchrpcclient "github.com/smartbch/smartbch/rpc/client"
)

const (
	ScanSideChainInterval = 6 * time.Second
	SendHandleUtxoDelay   = 22 * 60
	RescanThreshold       = 9
	ThresholdIn24Hours    = 1000_0000_0000 // 1000 BCH
)

var (
	NetParams         = &chaincfg.TestNet3Params //&chaincfg.MainNetParams
	CCContractAddress = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x27, 0x14}
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
	Hash              string  `json:"hash"`
	TransactionIndex  string  `json:"transactionIndex"`
	Nonce             string  `json:"nonce"`
	BlockHash         string  `json:"blockHash"`
	BlockNumber       string  `json:"blockNumber"`
	From              string  `json:"from"`
	To                string  `json:"to"`
	Value             string  `json:"value"`
	GasPrice          string  `json:"gasPrice"`
	Gas               string  `json:"gas"`
	Input             string  `json:"input"`
	CumulativeGasUsed string  `json:"cumulativeGasUsed"`
	GasUsed           string  `json:"gasUsed"`
	ContractAddress   string  `json:"contractAddress"`
	Logs              []Log   `json:"logs"`
	LogsBloom         string  `json:"logsBloom"`
	Status            string  `json:"status"`
	StatusStr         string  `json:"statusStr"`
	OutData           string  `json:"outData"`
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
func SendStartRescanAndHandleUTXO(ctx context.Context, client *ethclient.Client, bchClient *rpcclient.Client, lastRescanHeight, lastRescanTime, handleUtxoDelay int64) {
	height := lastRescanHeight + 1
	sendHandleUtxo := false
	for {
		_, err := bchClient.GetBlockHash(height + 9)
		if err != nil {
			time.Sleep(30 * time.Second)
			fmt.Printf("get block %d hash err:%s\n", height+9, err)
			continue
		}
		fmt.Printf("mainnet height:%d\n", height)
		if lastRescanHeight+2 <= height {
			callData := ccabi.PackStartRescanFunc(big.NewInt(height))
			txHash, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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
			txHash, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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

func TriggerDamageControl(fatalErr FatalError) {
}

// catch up the latest height of smartBCH
func Catchup(bs *BlockScanner) {
	ctx := context.Background()
	metaInfo := getMetaInfo(bs.db)
	fmt.Printf("MetaInfo %#v\n", metaInfo)
	endHeight, err := bs.ethClient.BlockNumber(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("SideChainHeight %d\n", metaInfo.SideChainHeight)
	for height := metaInfo.SideChainHeight; height < int64(endHeight); height++ {
		fmt.Printf("Height %d\n", height)
		endHeight, err = bs.ethClient.BlockNumber(ctx)
		if err != nil {
			panic(err)
		}
		timestamp, err := bs.GetBlockTime(ctx, height)
		if err != nil {
			fmt.Printf("Error in GetBlockTime: %s\n", err.Error())
			continue
		}
		fmt.Printf("Timestamp %d\n", timestamp)
		err = bs.ScanBlock(ctx, timestamp, height)
		if err != nil {
			if fatalErr, ok := err.(FatalError); ok {
				panic(fatalErr)
			} else {
				fmt.Printf("Error in ScanBlock: %s\n", err.Error())
			}
		}
	}
}

func MainLoop(bs *BlockScanner, sbchClient *sbchrpcclient.Client) {
	ctx := context.Background()
	metaInfo := getMetaInfo(bs.db)
	height := metaInfo.SideChainHeight
	for { // process sidechain blocks
		time.Sleep(ScanSideChainInterval)
		timestamp, err := bs.GetBlockTime(ctx, height)
		if err != nil {
			fmt.Printf("Error in GetBlockTime: %s\n", err.Error())
			continue
		}
		metaInfo = getMetaInfo(bs.db) // MetaInfo may get updated during processing, so we reload it
		if metaInfo.LastRescanTime > 0 && metaInfo.LastRescanTime + SendHandleUtxoDelay < time.Now().Unix() {
			sendHandleUtxoTransaction(ctx, bs.ethClient, sbchClient) // it will loop util succeed
		}
		bs.CheckSlidingWindow(ctx, timestamp, &metaInfo)
		// check mainchain's new blocks every 20 sidechain blocks
		if height % 20 == 0 {
			err, needRescan := bs.CheckMainChainForRescan(metaInfo)
			if err != nil {
				if fatalErr, ok := err.(FatalError); ok {
					TriggerDamageControl(fatalErr)
				} else {
					fmt.Printf("Error in CheckMainChainForRescan: %s\n", err.Error())
				}
			}
			if needRescan {
				sendStartRescanTransaction(ctx, bs.ethClient, sbchClient, height, metaInfo.ScannedHeight)
			}
		}
		// scan sidechain's blocks
		err = bs.ScanBlock(ctx, timestamp, height)
		if err != nil {
			if fatalErr, ok := err.(FatalError); ok {
				TriggerDamageControl(fatalErr)
			} else {
				fmt.Printf("Error in ScanBlock: %s\n", err.Error())
			}
		} else {
			height++
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
func (txc *CCTxCounter) CheckMainChainBlock(gormTx *gorm.DB, blockHeight int64) error {
	_, err := txc.client.GetBlockHash(blockHeight + 9) // is this block finalized?
	isFinalized := err == nil
	hash, err := txc.client.GetBlockHash(blockHeight) // even if it is not finalized, we still need checkEvilTx
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight
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
	return txc.checkEvilTx(gormTx, blk)
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
				return NewFatal(fmt.Sprintf("[EVIL] bad mainchain format %#v\n", bchTx))
			}
			scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(bchTx.TxOut[0].PkScript, NetParams)
			if err != nil || len(addrs) != 1 {
				debug.PrintStack()
				return NewFatal(fmt.Sprintf("[EVIL] cannot parse output %#v\n", bchTx))
			}
			if scrClass == txscript.ScriptHashTy { // P2SH
				addr := addrs[0].ScriptAddress()
				oldTxid := hex.EncodeToString(reverse(txin.PreviousOutPoint.Hash[:]))
				oldVout := txin.PreviousOutPoint.Index
				err = mainEvtFinishConverting(gormTx, oldTxid, oldVout, string(addr[:]), txid, 0, false)
			} else if scrClass == txscript.PubKeyHashTy { // P2PKH
				addr := addrs[0].ScriptAddress()
				err = mainEvtRedeemOrReturn(gormTx, txid, 0, string(addr[:]), false)
			} else {
				debug.PrintStack()
				return NewFatal(fmt.Sprintf("[EVIL] invalid srcClass %#v\n", bchTx))
			}
			if err != nil {
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
		fmt.Printf("TXIN %s-%d\n", oldTxid, oldVout)
		if scrClass == txscript.ScriptHashTy { // P2SH
			addr := addrs[0].ScriptAddress()
			err = mainEvtFinishConverting(gormTx, oldTxid, oldVout, string(addr[:]), txid, 0, true)
		} else if scrClass == txscript.PubKeyHashTy { //P2PKH
			addr := addrs[0].ScriptAddress()
			err = mainEvtRedeemOrReturn(gormTx, oldTxid, oldVout, string(addr[:]), true)
		} else {
			debug.PrintStack()
			return false, NewFatal(fmt.Sprintf("[EVIL] invalid srcClass %#v\n", bchTx))
		}
		if err != nil {
			return false, err
		}
	}
	// addToBeRecognized: one-vout with p2sh, maybe one-vout with opreturn
	covenantAddr := ScriptHashToAddr([]byte(bw.currCovenantAddr))
	for vout, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, NetParams)
		if err != nil ||  len(addrs) != 1 {
			continue
		}
		addr := addrs[0].ScriptAddress()
		isAddToBeRecognized := scrClass == txscript.ScriptHashTy && string(addr[:]) == bw.currCovenantAddr
		if scrClass == txscript.ScriptHashTy {
			fmt.Printf("ADDRS: %s vs %s isAddToBeRecognized %v\n", covenantAddr, addrs[0].EncodeAddress(), isAddToBeRecognized)
			fmt.Printf("%#v vs %#v\n", addr[:], []byte(bw.currCovenantAddr))
			fmt.Printf("txid: %s\n", txid)
		}
		if isAddToBeRecognized {
			dbChanged = true
			ccUtxo := CcUtxo{
				Type:         ToBeRecognized,
				CovenantAddr: bw.currCovenantAddr,
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
	blockCount, err := bw.client.GetBlockCount()
	fmt.Printf("main blockCount %d err %#v\n", blockCount, err)
	hash, err := bw.client.GetBlockHash(blockHeight)
	fmt.Printf("main height %d hash %#v\n", blockHeight, hash)
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
		fmt.Printf("AfterMain UTXOs\n")
		printUtxoSet(bw.db)
	}
	return err
}

// ==================

// Scan blocks of side chain and main chain
type BlockScanner struct {
	bchClient *rpcclient.Client
	db        *gorm.DB
	rpcClient *rpc.Client
	ethClient *ethclient.Client
	abi       abi.ABI
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
	return &BlockScanner{
		bchClient:      bchClient,
		db:             db,
		rpcClient:      rpcClient,
		ethClient:      ethClient,
		abi:            ccabi.ABI.GetABI(),
	}

}

// according to the sum in sliding window, pause/resume the shagate logic
func (bs *BlockScanner) CheckSlidingWindow(ctx context.Context, timestamp int64, metaInfo *MetaInfo) {
	sum := metaInfo.getSumInSlidingWindow(timestamp)
	changeIsPaused := false
	if sum > ThresholdIn24Hours && !metaInfo.IsPaused {
		changeIsPaused = true
		metaInfo.IsPaused = true
		sendPauseTransaction(ctx, bs.ethClient)
	}
	if sum < ThresholdIn24Hours && metaInfo.IsPaused {
		changeIsPaused = true
		metaInfo.IsPaused = false
		sendResumeTransaction(ctx, bs.ethClient)
	}
	if changeIsPaused {
		bs.db.Model(&metaInfo).Updates(metaInfo)
	}
}

func (bs *BlockScanner) CheckMainChainForRescan(metaInfo MetaInfo) (error, bool) {
	height := metaInfo.ScannedHeight + 1
	txCounter := &CCTxCounter{
		currCovenantAddr: metaInfo.CurrCovenantAddr,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db, false), // get all the UTXOs for checkEvilTx
	}
	for {
		err := txCounter.CheckMainChainBlock(bs.db, height)
		if errors.Is(err, ErrNoBlockFoundAtGivenHeight) { // no new blocks
			break
		}
		if err != nil {
			return err, false
		}
		height++ //try to find the next new block
	}
	needRescan := txCounter.ccTxCount > 0 || metaInfo.ScannedHeight + RescanThreshold < height
	return nil, needRescan
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
	txList := block.Transactions
	txToBeParsed := make([]*Transaction, 0, len(txList))
	for _, tx := range txList {
		if common.HexToAddress(tx.To) != CCContractAddress {
			continue
		}
		var tmp map[string]any
		bs.rpcClient.CallContext(ctx, &tmp, "eth_getTransactionReceipt", tx.Hash)
		fmt.Printf("DBG tx %#v\n", tmp)
		err := bs.rpcClient.CallContext(ctx, tx, "eth_getTransactionReceipt", tx.Hash)
		if err != nil {
			return err
		}
		input := hexutil.MustDecode(tx.Input)
		status := hexutil.MustDecodeUint64(tx.Status)
		fmt.Printf("tx %#v\ninput: %#v status %d\n", tx, input, status)
		if len(input) < 4 || status != mevmtypes.ReceiptStatusSuccessful {
			continue
		}
		method, _ := bs.abi.MethodById(input[:4])
		fmt.Printf("method %#v\n", method)
		if method == nil {
			continue
		}
		if method.Name == "handleUTXOs" {
			updateLastRescanTime(bs.db, -1) // -1 means no pending startRescan needs to run handleUTXOs
		} else if method.Name == "startRescan" && len(input) == 36 {
			bs.parseStartRescan(ctx, timestamp, tx, input)
		}
		txToBeParsed = append(txToBeParsed, tx)
	}
	fmt.Printf("txToBeParsed %#v\n", txToBeParsed)
	metaInfo := getMetaInfo(bs.db)
	err = bs.db.Transaction(func(gormTx *gorm.DB) error { // One DB-Transaction to update UTXO set and height
		for _, tx := range txToBeParsed {
			fmt.Printf("now tx %#v\n", tx)
			bs.processReceipt(gormTx, tx, &metaInfo, int64(timestamp))
		}
		metaInfo.SideChainHeight = blockHeight
		gormTx.Model(&metaInfo).Updates(metaInfo)
		return nil
	})
	if len(txToBeParsed) != 0 {
		fmt.Printf("AfterSide UTXOs\n")
		printUtxoSet(bs.db)
	}
	return err
}

func (bs *BlockScanner) parseStartRescan(ctx context.Context, timestamp int64, tx *Transaction, input []byte) {
	start := 4+32-8
	if len(input) < start {
		return
	}
	mainChainHeight := int64(binary.BigEndian.Uint64(input[start:]))
	metaInfo := getMetaInfo(bs.db)
	fmt.Printf("mainChainHeight %d metaInfo %#v\n", mainChainHeight, metaInfo)
	watcher := &BlockWatcher{
		db:               bs.db,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db, true), // only the UTXOs waiting to be moved on mainchain
		currCovenantAddr: metaInfo.CurrCovenantAddr,
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
			fmt.Printf("EventNewRedeemable %s\n", log.Topics[1])
			if err != nil {
				return err
			}
		//NewLostAndFound(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr);
		case EventNewLostAndFound:
			fmt.Printf("EventNewLostAndFound\n")
			txid := log.Topics[1][2:]
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			err := sideEvtLostAndFound(gormTx, string(addr[:]), txid, uint32(vout))
			if err != nil {
				return err
			}
		//Redeem(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventRedeem:
			txid := log.Topics[1][2:]
			fmt.Printf("EventRedeem %s\n", txid)
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			redeemTarget := BurnAddressMainChain // transfer-by-burning
			//function redeem(uint256 txid, uint256 index, address targetAddress) external payable;
			if len(input) > 96 {
				redeemTarget = string(input[32+32+12:])
			}
			err := sideEvtRedeem(gormTx, string(addr[:]), txid, uint32(vout), data[31], redeemTarget,
				metaInfo, currTime)
			if err != nil {
				return err
			}
		//ChangeAddr(address indexed oldCovenantAddr, address indexed newCovenantAddr);
		case EventChangeAddr:
			fmt.Printf("EventChangeAddr\n")
			oldCovenantAddr := common.HexToAddress(log.Topics[1][2+12*2:])
			newCovenantAddr := common.HexToAddress(log.Topics[2][2+12*2:])
			err := sideEvtChangeAddr(gormTx, string(oldCovenantAddr[:]), string(newCovenantAddr[:]))
			if err != nil {
				return err
			}
		//Convert(uint256 indexed prevTxid, uint32 indexed prevVout, address indexed oldCovenantAddr, uint256 txid, uint32 vout, address newCovenantAddr);
		case EventConvert:
			fmt.Printf("EventConvert\n")
			prevTxid := log.Topics[1][2:]
			prevVout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			var params ConvertParams
			err := bs.abi.UnpackIntoInterface(&params, "Convert", data)
			if err != nil {
				return err
			}
			err = sideEvtConvert(gormTx, prevTxid[:], uint32(prevVout), hex.EncodeToString(params.Txid[:]), params.Vout,
				string(params.CovenantAddr[:]))
			if err != nil {
				return err
			}
		//Deleted(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventDeleted:
			txid := log.Topics[1][2:]
			fmt.Printf("EventDeleted %s\n", txid)
			vout, _ := strconv.ParseInt(log.Topics[2][2+2+24*2:], 16, 64)
			addr := common.HexToAddress(log.Topics[3][2+12*2:])
			err := sideEvtDeleted(gormTx, string(addr[:]), txid, uint32(vout), data[31])
			if err != nil {
				return err
			}
		}
	}
	return nil
}
