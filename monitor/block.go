package monitor

import (
	"context"
	"runtime/debug"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"math/big"
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
	NetParams             = &chaincfg.MainNetParams
)

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

func Catchup(bs *BlockScanner, client *ethclient.Client, endHeight int64) {
	ctx := context.Background()
	metaInfo := getMetaInfo(bs.db)
	for height := metaInfo.SideChainHeight; height < endHeight; height++ {
		timestamp, err := bs.GetBlockTime(ctx, height)
		if err != nil {
			fmt.Printf("Error in GetBlockTime: %s\n", err.Error())
			continue
		}
		err = bs.ScanBlock(ctx, timestamp, height, client)
		if err != nil {
			if fatalErr, ok := err.(FatalError); ok {
				panic(fatalErr)
			} else {
				fmt.Printf("Error in ScanBlock: %s\n", err.Error())
			}
		}
	}
}

func MainLoop(bs *BlockScanner, client *ethclient.Client, sbchClient *sbchrpcclient.Client) {
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
			sendHandleUtxoTransaction(ctx, client, sbchClient) // TODO it will loop util succeed
		}
		bs.CheckSlidingWindow(ctx, timestamp, client, &metaInfo)
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
				sendStartRescanTransaction(ctx, client, sbchClient, height, metaInfo.ScannedHeight)
			}
		}
		err = bs.ScanBlock(ctx, timestamp, height, client)
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

func inUtxoSet(utxoSet map[[36]byte]struct{}, txin *wire.TxIn) bool {
	var key [36]byte
	copy(key[:32], txin.PreviousOutPoint.Hash[:])
	binary.BigEndian.PutUint32(key[32:], txin.PreviousOutPoint.Index)
	_, ok := utxoSet[key]
	return ok
}

// accumulate the cc-transactions on main chain
type CCTxCounter struct {
	currCovenantAddr string
	client           *rpcclient.Client
	ccTxCount        int64
	utxoSet          map[[36]byte]struct{}
}

// if the block at 'blockHeight' is finalized, analyze its transactions to increase 'ccTxCount'
func (txc *CCTxCounter) CheckMainchainBlock(gormTx *gorm.DB, blockHeight int64) error {
	_, err := txc.client.GetBlockHash(blockHeight + 9) // is this block finalized?
	isFinalized := err == nil
	hash, err := txc.client.GetBlockHash(blockHeight)
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
		txid := string(h[:])
		for i := range bchTx.TxIn {
			if !inUtxoSet(txc.utxoSet, bchTx.TxIn[i]) {
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
			if scrClass == txscript.ScriptHashTy {
				addr := addrs[0].ScriptAddress()
				err = mainEvtFinishConverting(gormTx, txid, 0, string(addr[:]), false)
			} else if scrClass == txscript.PubKeyHashTy {
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
		if scrClass == txscript.ScriptHashTy && string(addr[:]) == txc.currCovenantAddr {
			return true
		}
	}
	return false
}

// Watches the blocks of main chain
type BlockWatcher struct {
	db               *gorm.DB
	client           *rpcclient.Client
	utxoSet          map[[36]byte]struct{}
	currCovenantAddr string
}

func (bw *BlockWatcher) handleTx(gormTx *gorm.DB, bchTx *wire.MsgTx) error {
	h := bchTx.TxHash()
	txid := string(h[:])
	// convert: one-vin in utxoSet one-vout with p2sh (newCovenantAddr)
	// redeem&return: one-vin in utxoSet one-vout with p2pkh
	if len(bchTx.TxIn) == 1 && len(bchTx.TxOut) == 1 && inUtxoSet(bw.utxoSet, bchTx.TxIn[0]) {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(bchTx.TxOut[0].PkScript, NetParams)
		if err != nil || len(addrs) != 1 {
			debug.PrintStack()
			return NewFatal(fmt.Sprintf("[EVIL] cannot parse output %#v\n", bchTx))
		}
		if scrClass == txscript.ScriptHashTy {
			addr := addrs[0].ScriptAddress()
			err = mainEvtFinishConverting(gormTx, txid, 0, string(addr[:]), true)
		} else if scrClass == txscript.PubKeyHashTy {
			addr := addrs[0].ScriptAddress()
			err = mainEvtRedeemOrReturn(gormTx, txid, 0, string(addr[:]), true)
		} else {
			debug.PrintStack()
			return NewFatal(fmt.Sprintf("[EVIL] invalid srcClass %#v\n", bchTx))
		}
		return err
	}
	// addToBeRecognized: one-vout with p2sh, maybe one-vout with opreturn
	for vout, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, NetParams)
		if err != nil ||  len(addrs) != 1 {
			continue
		}
		addr := addrs[0].ScriptAddress()
		if scrClass != txscript.ScriptHashTy || string(addr[:]) != bw.currCovenantAddr {
			continue
		}
		ccUtxo := CcUtxo{
			Type:         ToBeRecognized,
			CovenantAddr: bw.currCovenantAddr,
			Amount:       txout.Value,
			Txid:         txid,
			Vout:         uint32(vout),
		}
		err = addToBeRecognized(gormTx, ccUtxo)
		if err != nil {
			return err
		}
	}
	return nil
}

func (bw *BlockWatcher) HandleMainchainBlock(blockHeight int64) error {
	hash, err := bw.client.GetBlockHash(blockHeight)
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight
	}
	blk, err := bw.client.GetBlock(hash)
	if err != nil {
		panic(err)
	}
	err = bw.db.Transaction(func(gormTx *gorm.DB) error {
		for _, bchTx := range blk.Transactions {
			err = bw.handleTx(gormTx, bchTx)
			if err != nil {
				return err
			}
		}
		return updateMainChainHeight(gormTx, blockHeight)
	})
	return err
}

// ==================

// Scan blocks of side chain and main chain
type BlockScanner struct {
	bchClient      *rpcclient.Client
	db             *gorm.DB
	rpcClient      *rpc.Client
	ccContractAddr common.Address
	abi            *abi.ABI
}

func (bs *BlockScanner) CheckSlidingWindow(ctx context.Context, timestamp int64, client *ethclient.Client, metaInfo *MetaInfo) {
	sum := metaInfo.getSumInSlidingWindow(timestamp)
	changeIsPaused := false
	if sum > ThresholdIn24Hours && !metaInfo.IsPaused {
		changeIsPaused = true
		metaInfo.IsPaused = true
		sendPauseTransaction(ctx, client)
	}
	if sum < ThresholdIn24Hours && metaInfo.IsPaused {
		changeIsPaused = true
		metaInfo.IsPaused = false
		sendResumeTransaction(ctx, client)
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
		utxoSet:          getUtxoSet(bs.db, true),
	}
	for {
		err := txCounter.CheckMainchainBlock(bs.db, height)
		if errors.Is(err, ErrNoBlockFoundAtGivenHeight) {
			break
		}
		if err != nil {
			return err, false
		}
		height++
	}
	return nil, txCounter.ccTxCount > 0 || metaInfo.ScannedHeight + RescanThreshold < height
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

func (bs *BlockScanner) ScanBlock(ctx context.Context, timestamp, blockHeight int64, client *ethclient.Client) error {
	var txList []*mevmtypes.Transaction
	err := bs.rpcClient.CallContext(ctx, &txList, "sbch_getTxListByHeight", hexutil.EncodeUint64(uint64(blockHeight)))
	if err != nil {
		return err
	}
	txToBeParsed := make([]*mevmtypes.Transaction, 0, len(txList))
	for _, tx := range txList {
		if tx.To != bs.ccContractAddr {
			continue
		}
		if len(tx.Input) < 4 || tx.Status != mevmtypes.ReceiptStatusSuccessful {
			continue
		}
		method, _ := bs.abi.MethodById(tx.Input[:4])
		if method == nil {
			continue
		}
		if method.Name == "handleUTXOs" {
			updateLastRescanTime(bs.db, -1)
		} else if method.Name == "startRescan" && len(tx.Input) == 36 {
			bs.parseStartRescan(ctx, timestamp, tx)
			txToBeParsed = append(txToBeParsed, tx)
		} else if method.Name == "handleUTXOs" || method.Name == "redeem" {
			txToBeParsed = append(txToBeParsed, tx)
		}
	}
	err = bs.db.Transaction(func(gormTx *gorm.DB) error { // One DB-Transaction to update UTXO set and height
		metaInfo := getMetaInfo(bs.db)
		for _, tx := range txToBeParsed {
			bs.processReceipt(gormTx, tx, &metaInfo, int64(timestamp))
		}
		metaInfo.SideChainHeight = blockHeight
		gormTx.Model(&metaInfo).Updates(metaInfo)
		return nil
	})
	return err
}

func (bs *BlockScanner) parseStartRescan(ctx context.Context, timestamp int64, tx *mevmtypes.Transaction) {
	mainChainHeight := int64(binary.BigEndian.Uint64(tx.Input[4+32-8:]))
	metaInfo := getMetaInfo(bs.db)
	watcher := &BlockWatcher{
		db:               bs.db,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db, false),
		currCovenantAddr: metaInfo.CurrCovenantAddr,
	}
	for h := metaInfo.MainChainHeight + 1; h <= mainChainHeight; h++ {
		watcher.HandleMainchainBlock(h)
	}
	bs.db.Model(&metaInfo).Updates(MetaInfo{ScannedHeight: mainChainHeight, LastRescanTime: int64(timestamp)})
}

func (bs *BlockScanner) processReceipt(gormTx *gorm.DB, tx *mevmtypes.Transaction,
	metaInfo *MetaInfo, currTime int64) error {
	for _, log := range tx.Logs {
		switch common.Hash(log.Topics[0]) {
		//NewRedeemable(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr);
		case EventNewRedeemable:
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := log.Topics[3][12:]
			err := sideEvtRedeemable(gormTx, string(addr[:]), txid, vout)
			if err != nil {
				return err
			}
		//NewLostAndFound(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr);
		case EventNewLostAndFound:
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := log.Topics[3][12:]
			err := sideEvtLostAndFound(gormTx, string(addr[:]), txid, vout)
			if err != nil {
				return err
			}
		//Redeem(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventRedeem:
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := log.Topics[3][12:]
			redeemTarget := string(tx.Input[32+32+12:])
			err := sideEvtRedeem(gormTx, string(addr[:]), txid, vout, log.Data[31], redeemTarget,
				metaInfo, currTime)
			if err != nil {
				return err
			}
		//ChangeAddr(address indexed oldCovenantAddr, address indexed newCovenantAddr);
		case EventChangeAddr:
			oldCovenantAddr := log.Topics[1][12:]
			newCovenantAddr := log.Topics[2][12:]
			err := sideEvtChangeAddr(gormTx, string(oldCovenantAddr[:]), string(newCovenantAddr[:]))
			if err != nil {
				return err
			}
		//Convert(uint256 indexed prevTxid, uint32 indexed prevVout, address indexed oldCovenantAddr, uint256 txid, uint32 vout, address newCovenantAddr);
		case EventConvert:
			prevTxid := string(log.Topics[1][:])
			prevVout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			var params ConvertParams
			err := bs.abi.UnpackIntoInterface(&params, "Convert", log.Data)
			if err != nil {
				return err
			}
			err = sideEvtConvert(gormTx, prevTxid, prevVout, string(params.Txid[:]), params.Vout,
				string(params.CovenantAddr[:]))
			if err != nil {
				return err
			}
		//Deleted(uint256 indexed txid, uint32 indexed vout, address indexed covenantAddr, uint8 sourceType);
		case EventDeleted:
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := log.Topics[3][12:]
			err := sideEvtDeleted(gormTx, string(addr[:]), txid, vout, log.Data[31])
			if err != nil {
				return err
			}
		}
	}
	return nil
}
