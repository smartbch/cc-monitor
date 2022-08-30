package monitor

import (
	"context"
	"errors"
	"encoding/binary"
	"math/big"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/rpcclient"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	ToBeRecognized     = 1
	Redeemable         = 2
	LostAndFound       = 3
	LostAndReturn      = 4
	LostAndReturnToDel = 5
	Redeeming          = 6
	RedeemingToDel     = 7
	HandingOver        = 8
	HandedOver         = 9

	MainChainBurningAddress string = ""
)

var (
	ErrCovenantAddrMismatch    = errors.New("Covenant Address Mismatch")
	ErrDuplicatedUtxo          = errors.New("Find Duplicated Utxo")
	ErrFailedToCreate          = errors.New("Failed to Create")
	ErrInvalidSourceType       = errors.New("Invalid Source Type")
	ErrNotFound                = errors.New("UTXO Not Found")
	ErrNotHandedOver           = errors.New("Not HandedOver Status")
	ErrNotLostAndFound         = errors.New("Not LostAndFound Status")
	ErrNotLostAndReturn        = errors.New("Not LostAndReturn Status")
	ErrNotLostAndReturnToDel   = errors.New("Not LostAndReturnToDel Status")
	ErrNotRedeemable           = errors.New("Not Redeemable Status")
	ErrNotRedeemOrReturn       = errors.New("Not Redeemable or LostAndReturn Status")
	ErrNotRedeemingToDel       = errors.New("Not Redeeming Status")
	ErrNotToBeRecognized       = errors.New("Not ToBeRecognized Status")
	ErrIncorrectBurningAddress = errors.New("Incorrect Burning Address")
	ErrReceiverNotOldOwner     = errors.New("Receiver Not Old Owner")
	ErrReceiverNotRedeemTarget = errors.New("Receiver Not RedeemTarget")
	ErrNoBlockFoundAtGivenHeight = errors.New("No Block Found at the Given Height")
	ErrNoBlockFoundForGivenHash = errors.New("No Block Found for the Given Hash")
)

var (
	EventNewRedeemable = crypto.Keccak256Hash([]byte("NewRedeemable(uint256,uint32,address)"))
	EventNewLostAndFound = crypto.Keccak256Hash([]byte("NewLostAndFound(uint256,uint32,address)"))
	EventRedeem = crypto.Keccak256Hash([]byte("Redeem(uint256,uint32,address,uint8)"))
	EventChangeAddr = crypto.Keccak256Hash([]byte("ChangeAddr(address,address)"))
	EventConvert = crypto.Keccak256Hash([]byte("Convert(uint256,uint32,address,uint256,uint32,address)"))
	EventDeleted = crypto.Keccak256Hash([]byte("Deleted(uint256,uint32,address,uint8)"))
)

const (
	FromRedeemable   = uint8(0) // when it's redeemed
	FromLostAndFound = uint8(1) // when it's redeemed or deleted
	FromRedeeming    = uint8(2) // when it's deleted
	FromBurnRedeem   = uint8(9) // when it's automatically redeemed to burning address
)

type CcUtxo struct {
	gorm.Model
	Type         int
	CovenantAddr string
	RedeemTarget string
	Amount       int64
	Txid         string
	Vout         uint32
}

type MetaInfo struct {
	gorm.Model
	MainChainHeight  int64
	SideChainHeight  int64
	CurrCovenantAddr string
	LastCovenantAddr string
}

type ConvertParams struct {
	Txid         common.Hash
	Vout         uint32
	CovenantAddr common.Address
}

func MigrateSchema(db *gorm.DB) {
	db.AutoMigrate(&CcUtxo{})
	db.AutoMigrate(&MetaInfo{})
}

func OpenDB(path string) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	return db
}

func getRedeemableSet(db *gorm.DB) map[[36]byte]struct{} {
	result := make(map[[36]byte]struct{})
	var utxoList []CcUtxo
	db.Find(&utxoList, "Type = ?", Redeemable)
	for _, utxo := range utxoList {
		var id [36]byte
		copy(id[:32], utxo.Txid)
		binary.BigEndian.PutUint32(id[32:], uint32(utxo.Vout))
		result[id] = struct{}{}
	}
	return result
}

func addToBeRecognized(db *gorm.DB, newCcUtxo CcUtxo) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", newCcUtxo.Txid, newCcUtxo.Vout)
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrDuplicatedUtxo
	}

	result = db.Create(&newCcUtxo)

	if result.Error != nil {
		return ErrFailedToCreate
	}
	return nil
}

func sideEvtRedeemable(db *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != ToBeRecognized {
		return ErrNotToBeRecognized
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	db.Model(&utxo).Update("Type", Redeemable)
	return nil
}

func sideEvtLostAndFound(db *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != ToBeRecognized {
		return ErrNotToBeRecognized
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	db.Model(&utxo).Update("Type", LostAndFound)
	return nil
}

func sideEvtRedeem(db *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8, redeemTarget string) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	if sourceType == FromRedeemable {
		if utxo.Type != Redeemable {
			return ErrNotRedeemable
		}
		db.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndFound {
			return ErrNotLostAndFound
		}
		db.Model(&utxo).Updates(CcUtxo{Type: LostAndReturn, RedeemTarget: redeemTarget})
	} else if sourceType == FromBurnRedeem {
		if utxo.Type != ToBeRecognized {
			return ErrNotToBeRecognized
		}
		if redeemTarget != MainChainBurningAddress {
			return ErrIncorrectBurningAddress
		}
		db.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
	}
	return ErrInvalidSourceType
}

func mainEvtRedeemOrReturn(db *gorm.DB, txid string, vout uint32, receiver string) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != Redeeming && utxo.Type != LostAndReturn {
		return ErrNotRedeemOrReturn
	}
	if utxo.Type == Redeeming {
		if utxo.RedeemTarget != receiver {
			return ErrReceiverNotRedeemTarget
		}
		db.Model(&utxo).Update("Type", RedeemingToDel)
	}
	if utxo.Type == LostAndReturn {
		if utxo.RedeemTarget != receiver {
			return ErrReceiverNotOldOwner
		}
		db.Model(&utxo).Update("Type", LostAndReturnToDel)
	}
	return nil
}

func mainEvtFinishConverting(db *gorm.DB, txid string, vout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != HandingOver {
		return ErrNotHandedOver
	}
	if utxo.CovenantAddr != newCovenantAddr {
		return ErrCovenantAddrMismatch
	}
	db.Model(&utxo).Update("Type", HandedOver)
	return nil
}

func sideEvtChangeAddr(db *gorm.DB, oldCovenantAddr, newCovenantAddr string) error {
	var utxoList []CcUtxo
	result := db.Find(&utxoList, "CovenantAddr = ?", oldCovenantAddr)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	for _, utxo := range utxoList {
		db.Model(&utxo).Updates(CcUtxo{Type: HandingOver, CovenantAddr: newCovenantAddr})
	}
	return nil
}

func sideEvtConvert(db *gorm.DB, txid string, vout uint32, newTxid string, newVout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != HandedOver {
		return ErrNotHandedOver
	}
	if utxo.CovenantAddr != newCovenantAddr {
		return ErrCovenantAddrMismatch
	}
	db.Model(&utxo).Updates(CcUtxo{Type: Redeemable, Txid: newTxid, Vout: newVout})
	return nil
}

func sideEvtDeleted(db *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8) error {
	var utxo CcUtxo
	result := db.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	if sourceType == FromRedeeming {
		if utxo.Type != RedeemingToDel {
			return ErrNotRedeemingToDel
		}
		db.Delete(&utxo)
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndReturnToDel {
			return ErrNotLostAndReturnToDel
		}
		db.Delete(&utxo)
	} else {
		return ErrInvalidSourceType
	}
	return nil
}

// ==================

type BlockWatcher struct {
	db               *gorm.DB
	client           *rpcclient.Client
	redeemableSet    map[[36]byte]struct{}
	currCovenantAddr string
}

// convert: one-vin with redeemableSet one-vout with p2sh (newCovenantAddr)
// redeem&return: one-vin with redeemableSet one-vout with p2pkh
// addToBeRecognized: one-vout with p2sh, maybe one-vout with opreturn

func (bw *BlockWatcher) handleTx(tx *wire.MsgTx) error {
	h := tx.TxHash()
	txid := string(h[:])
	if len(tx.TxIn) == 1 && len(tx.TxOut) == 1 && bw.isFromRedeemableSet(tx.TxIn[0]) {
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
	for _, tx := range blk.Transactions {
		err = bw.handleTx(tx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (bw *BlockWatcher) isFromRedeemableSet(txin *wire.TxIn) bool {
	var key [36]byte
	copy(key[:32], txin.PreviousOutPoint.Hash[:])
	binary.BigEndian.PutUint32(key[32:], txin.PreviousOutPoint.Index)
	_, ok := bw.redeemableSet[key]
	return ok
}

// ==================

type BlockScanner struct {
	db             *gorm.DB
	client         *ethclient.Client
	ccContractABI  *abi.ABI
	ccContractAddr common.Address
}

func (bs *BlockScanner) ScanBlock(blockHeight int64) error {
	blk, err := bs.client.BlockByNumber(context.Background(), big.NewInt(blockHeight))
	if err != nil {
		return nil //ignore this height
	}
	for _, tx := range blk.Transactions() {
		to := *tx.To()
		if to != bs.ccContractAddr {
			continue
		}
		if len(tx.Data()) < 4 {
			continue
		}
		method, _ := bs.ccContractABI.MethodById(tx.Data()[:4])
		if method == nil {
			continue
		}
		if method.Name == "startRescan" {
			bs.parseStartRescan(tx)
		} else if method.Name == "handleUTXOs" {
			bs.parseHandleUTXOs(tx)
		} else if method.Name == "redeem" {
			bs.parseRedeem(tx)
		}
	}
	return nil
}

func (bs *BlockScanner) parseStartRescan(tx *ethtypes.Transaction) {
	bs.processReceipt(tx)
}

func (bs *BlockScanner) parseHandleUTXOs(tx *ethtypes.Transaction) {
	bs.processReceipt(tx)
}

func (bs *BlockScanner) parseRedeem(tx *ethtypes.Transaction) {
	bs.processReceipt(tx)
}

func (bs *BlockScanner) processReceipt(tx *ethtypes.Transaction) error {
	receipt, err := bs.client.TransactionReceipt(context.Background(), tx.Hash())
	if err != nil {
		return err
	}
	for _, log := range receipt.Logs {
		switch log.Topics[0].Hex() {
		case EventNewRedeemable.Hex():
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := common.HexToAddress(log.Topics[3].Hex())
			err := sideEvtRedeemable(bs.db, string(addr[:]), txid, vout)
			if err != nil {
				return err
			}
		case EventNewLostAndFound.Hex():
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := common.HexToAddress(log.Topics[3].Hex())
			err := sideEvtLostAndFound(bs.db, string(addr[:]), txid, vout)
			if err != nil {
				return err
			}
		case EventRedeem.Hex():
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := common.HexToAddress(log.Topics[3].Hex())
			redeemTarget := string(tx.Data()[32+32+12:])
			err := sideEvtRedeem(bs.db, string(addr[:]), txid, vout, log.Data[31], redeemTarget)
			if err != nil {
				return err
			}
		case EventChangeAddr.Hex():
			oldCovenantAddr := common.HexToAddress(log.Topics[1].Hex())
			newCovenantAddr := common.HexToAddress(log.Topics[2].Hex())
			err := sideEvtChangeAddr(bs.db, string(oldCovenantAddr[:]), string(newCovenantAddr[:]))
			if err != nil {
				return err
			}
		case EventConvert.Hex():
			prevTxid := string(log.Topics[1][:])
			prevVout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			// useless: oldCovenantAddr := common.HexToAddress(log.Topics[3].Hex())
			var params ConvertParams
			err := bs.ccContractABI.UnpackIntoInterface(&params, "Convert", log.Data)
			if err != nil {
				return err
			}
			err = sideEvtConvert(bs.db, prevTxid, prevVout, string(params.Txid[:]), params.Vout,
				string(params.CovenantAddr[:]))
			if err != nil {
				return err
			}
		case EventDeleted.Hex():
			txid := string(log.Topics[1][:])
			vout := uint32(binary.BigEndian.Uint32(log.Topics[2][28:]))
			addr := common.HexToAddress(log.Topics[3].Hex())
			err := sideEvtDeleted(bs.db, string(addr[:]), txid, vout, log.Data[31])
			if err != nil {
				return err
			}
		}
	}
	return nil
}
