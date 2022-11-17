package monitor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/rpcclient"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	mevmtypes "github.com/smartbch/moeingevm/types"
)

func SendStartRescanAndHandleUTXO(ctx context.Context, client *ethclient.Client, bchClient *rpcclient.Client, lastRescanHeight, lastRescanTime, handleUtxoDelay int64) {
	height := lastRescanHeight + 1
	sendHandleUtxo := false
	for {
		_, err := bchClient.GetBlockHash(height + 9)
		if err != nil {
			time.Sleep(30 * time.Second)
			fmt.Printf("get block hash err:%s\n", err)
			continue
		}
		fmt.Printf("mainnet height:%d\n", height)
		if lastRescanHeight+2 <= height {
			txHash, err := sendStartRescanTransaction(ctx, client, height)
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
			txHash, err := sendHandleUtxoTransaction(ctx, client)
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

func MainLoop(bs *BlockScanner, client *ethclient.Client) {
	ctx := context.Background()
	metaInfo, err := getMetaInfo(bs.db)
	if err != nil {
		panic(err)
	}
	height := metaInfo.SideChainHeight
	for {
		time.Sleep(6 * time.Second)
		metaInfo, err = getMetaInfo(bs.db)
		if err != nil {
			panic(err)
		}
		if height%10 == 0 && metaInfo.LastRescanTime+22*60 < time.Now().Unix() {
			_, err := sendHandleUtxoTransaction(ctx, client)
			if err != nil {
				fmt.Printf("Error in sendHandleUtxoTransaction: %#v\n", err)
			}
		}
		if height%20 == 0 && bs.CheckForRescan() {
			if err != nil {
				_, err := sendStartRescanTransaction(ctx, client, height)
				fmt.Printf("Error in sendHandleUtxoTransaction: %#v\n", err)
			}
		}
		err = bs.ScanBlock(ctx, height)
		if errors.Is(err, ErrNoBlockFoundAtGivenHeight) {
			continue
		}
		if err != nil {
			fmt.Printf("Error during ScanBlock %#v\n", err)
		} else {
			height++
		}
	}
}

// accumulate the cc-transactions on main chain
type CCTxCounter struct {
	currCovenantAddr string
	client           *rpcclient.Client
	ccTxCount        int64
}

// if the block at 'blockHeight' is finalized, analyze its transactions to increase 'ccTxCount'
func (txc *CCTxCounter) CountCCTxInMainchainBlock(blockHeight int64) error {
	hash, err := txc.client.GetBlockHash(blockHeight + 9) // make sure this block is finalized
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight
	}
	hash, err = txc.client.GetBlockHash(blockHeight)
	if err != nil {
		panic(err) //impossible
	}
	blk, err := txc.client.GetBlock(hash) // Get this finalized block's transactions
	if err != nil {
		return ErrNoBlockFoundForGivenHash
	}
	for _, bchTx := range blk.Transactions {
		if txc.isCCTxToSmartBCH(bchTx) {
			txc.ccTxCount++ // this is a cross-chain tx to smartbch
		}
	}
	return err
}

func (txc *CCTxCounter) isCCTxToSmartBCH(bchTx *wire.MsgTx) bool {
	for _, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, &chaincfg.MainNetParams)
		if err != nil {
			continue
		}
		if len(addrs) != 1 {
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

// convert: one-vin in utxoSet one-vout with p2sh (newCovenantAddr)
// redeem&return: one-vin in utxoSet one-vout with p2pkh
// addToBeRecognized: one-vout with p2sh, maybe one-vout with opreturn

func (bw *BlockWatcher) handleTx(gormTx *gorm.DB, bchTx *wire.MsgTx) error {
	h := bchTx.TxHash()
	txid := string(h[:])
	if len(bchTx.TxIn) == 1 && len(bchTx.TxOut) == 1 && bw.inUtxoSet(bchTx.TxIn[0]) {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(bchTx.TxOut[0].PkScript, &chaincfg.MainNetParams)
		if err != nil {
			return nil // ignore this bchTx
		}
		if len(addrs) != 1 {
			return nil // ignore this bchTx
		}
		if scrClass == txscript.ScriptHashTy {
			addr := addrs[0].ScriptAddress()
			err = mainEvtFinishConverting(gormTx, txid, 0, string(addr[:]))
		} else if scrClass == txscript.PubKeyHashTy {
			addr := addrs[0].ScriptAddress()
			err = mainEvtRedeemOrReturn(gormTx, txid, 0, string(addr[:]))
		} else {
			return nil // ignore this bchTx
		}
		return err
	}
	for vout, txout := range bchTx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, &chaincfg.MainNetParams)
		if err != nil {
			continue
		}
		if len(addrs) != 1 {
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
		return addToBeRecognized(bw.db, ccUtxo)
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
		return ErrNoBlockFoundForGivenHash
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

func (bw *BlockWatcher) inUtxoSet(txin *wire.TxIn) bool {
	var key [36]byte
	copy(key[:32], txin.PreviousOutPoint.Hash[:])
	binary.BigEndian.PutUint32(key[32:], txin.PreviousOutPoint.Index)
	_, ok := bw.utxoSet[key]
	return ok
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

func (bs *BlockScanner) CheckForRescan() bool {
	metaInfo, err := getMetaInfo(bs.db)
	if err != nil {
		panic(err)
	}
	height := metaInfo.ScannedHeight + 1
	txCounter := &CCTxCounter{
		currCovenantAddr: metaInfo.CurrCovenantAddr,
		client:           bs.bchClient,
	}
	for {
		err := txCounter.CountCCTxInMainchainBlock(height)
		if !errors.Is(err, ErrNoBlockFoundAtGivenHeight) {
			fmt.Printf("Error of CCTxCounter: %#v\n", err)
			break
		}
		if err == nil {
			height++
		}
	}
	return txCounter.ccTxCount > 0 || metaInfo.ScannedHeight+9 < height
}

func (bs *BlockScanner) GetBlockTime(ctx context.Context, blockHeight int64) (uint64, error) {
	var raw json.RawMessage
	err := bs.rpcClient.CallContext(ctx, &raw, "eth_getBlockByNumber", hexutil.EncodeUint64(uint64(blockHeight)), true)
	if err != nil {
		return 0, err
	}
	var head *types.Header
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, err
	}
	return head.Time, nil
}

func (bs *BlockScanner) ScanBlock(ctx context.Context, blockHeight int64) error {
	var txList []*mevmtypes.Transaction
	err := bs.rpcClient.CallContext(ctx, &txList, "sbch_getTxListByHeight", hexutil.EncodeUint64(uint64(blockHeight)))
	if err != nil {
		return nil //ignore this height
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
			bs.parseStartRescan(ctx, blockHeight, tx)
			txToBeParsed = append(txToBeParsed, tx)
		} else if method.Name == "handleUTXOs" || method.Name == "redeem" {
			txToBeParsed = append(txToBeParsed, tx)
		}
	}
	err = bs.db.Transaction(func(gormTx *gorm.DB) error { // One DB-Transaction to update UTXO set and height
		for _, tx := range txToBeParsed {
			bs.processReceipt(gormTx, tx)
		}
		return updateSideChainHeight(gormTx, blockHeight)
	})
	return err
}

func (bs *BlockScanner) parseStartRescan(ctx context.Context, blockHeight int64, tx *mevmtypes.Transaction) {
	mainChainHeight := int64(binary.BigEndian.Uint64(tx.Input[4+32-8:]))
	metaInfo, err := getMetaInfo(bs.db)
	if err != nil {
		panic(err)
	}
	watcher := &BlockWatcher{
		db:               bs.db,
		client:           bs.bchClient,
		utxoSet:          getUtxoSet(bs.db),
		currCovenantAddr: metaInfo.CurrCovenantAddr,
	}
	for h := metaInfo.MainChainHeight + 1; h <= mainChainHeight; h++ {
		watcher.HandleMainchainBlock(h)
	}
	timestamp, err := bs.GetBlockTime(ctx, blockHeight)
	if err != nil {
		fmt.Printf("Failed to get block time: %#v\n", timestamp)
		return
	}
	updateScannedHeightAndTime(bs.db, mainChainHeight, int64(timestamp))
}

func (bs *BlockScanner) processReceipt(gormTx *gorm.DB, tx *mevmtypes.Transaction) error {
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
			err := sideEvtRedeem(gormTx, string(addr[:]), txid, vout, log.Data[31], redeemTarget)
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
