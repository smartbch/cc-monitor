package monitor

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/rpcclient"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	mevmtypes "github.com/smartbch/moeingevm/types"
	"gorm.io/gorm"
)

// accumulate the cc-transactions on main chain
type CCTxCounter struct {
	currCovenantAddr string
	client           *rpcclient.Client
	ccTxCount        int64
}

func (txc *CCTxCounter) HandleBlock(blockHeight int64) error {
	hash, err := txc.client.GetBlockHash(blockHeight+9)
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight
	}
	hash, err = txc.client.GetBlockHash(blockHeight) // Get mature block
	if err != nil {
		panic(err) //impossible
	}
	blk, err := txc.client.GetBlock(hash)
	if err != nil {
		return ErrNoBlockFoundForGivenHash
	}
	for _, gormTx := range blk.Transactions {
		err = txc.handleTx(gormTx)
		if err != nil {
			return err
		}
	}
	return err
}

func (txc *CCTxCounter) handleTx(tx *wire.MsgTx) error {
	for _, txout := range tx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, &chaincfg.MainNetParams)
		if err != nil {
			continue
		}
		addr := *(addrs[0].(*bchutil.AddressPubKeyHash).Hash160())
		if scrClass != txscript.ScriptHashTy || string(addr[:]) != txc.currCovenantAddr {
			continue
		}
		txc.ccTxCount++
	}
	return nil
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

func (bw *BlockWatcher) handleTx(tx *wire.MsgTx) error {
	h := tx.TxHash()
	txid := string(h[:])
	if len(tx.TxIn) == 1 && len(tx.TxOut) == 1 && bw.inUtxoSet(tx.TxIn[0]) {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(tx.TxOut[0].PkScript, &chaincfg.MainNetParams)
		if err != nil {
			return nil // ignore this tx
		}
		if scrClass == txscript.ScriptHashTy {
			addr := *(addrs[0].(*bchutil.AddressPubKeyHash).Hash160())
			err = mainEvtFinishConverting(bw.db, txid, 0, string(addr[:]))
		} else if scrClass == txscript.PubKeyHashTy {
			addr := *(addrs[0].(*bchutil.AddressPubKeyHash).Hash160())
			err = mainEvtRedeemOrReturn(bw.db, txid, 0, string(addr[:]))
		} else {
			return nil // ignore this tx
		}
		return err
	}
	for vout, txout := range tx.TxOut {
		scrClass, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.PkScript, &chaincfg.MainNetParams)
		if err != nil {
			continue
		}
		addr := *(addrs[0].(*bchutil.AddressPubKeyHash).Hash160())
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

func (bw *BlockWatcher) HandleBlock(blockHeight int64) error {
	hash, err := bw.client.GetBlockHash(blockHeight)
	if err != nil {
		return ErrNoBlockFoundAtGivenHeight
	}
	blk, err := bw.client.GetBlock(hash)
	if err != nil {
		return ErrNoBlockFoundForGivenHash
	}
	err = bw.db.Transaction(func(gormTx *gorm.DB) error {
		for _, gormTx := range blk.Transactions {
			err = bw.handleTx(gormTx)
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
	txCounter      *CCTxCounter
	watcher        *BlockWatcher
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
	height := metaInfo.ScannedHeight+1
	bs.txCounter.currCovenantAddr = metaInfo.CurrCovenantAddr
	for {
		err := bs.txCounter.HandleBlock(height)
		if !errors.Is(err, ErrNoBlockFoundAtGivenHeight) {
			break
		}
		height++
	}
	return bs.txCounter.ccTxCount > 0 || metaInfo.ScannedHeight + 9 < height
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
		if method.Name == "startRescan" && len(tx.Input) == 36 {
			bs.parseStartRescan(tx)
			txToBeParsed = append(txToBeParsed, tx)
		} else if method.Name == "handleUTXOs" || method.Name == "redeem" {
			txToBeParsed = append(txToBeParsed, tx)
		}
	}
	err = bs.db.Transaction(func(gormTx *gorm.DB) error {
		for _, tx := range txToBeParsed {
			bs.processReceipt(gormTx, tx)
		}
		return nil
	})
	return err
}

func (bs *BlockScanner) parseStartRescan(tx *mevmtypes.Transaction) {
	mainChainHeight := int64(binary.BigEndian.Uint64(tx.Input[4+32-8:]))
	metaInfo, err := getMetaInfo(bs.db)
	if err != nil {
		panic(err)
	}
	bs.watcher.utxoSet = getUtxoSet(bs.db)
	bs.txCounter.ccTxCount = 0
	for h := metaInfo.MainChainHeight+1; h <= mainChainHeight; h++ {
		bs.watcher.HandleBlock(h)
	}
	err = updateScannedHeight(bs.db, mainChainHeight)
	if err != nil {
		panic(err)
	}
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

