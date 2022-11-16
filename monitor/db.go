package monitor

import (
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
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
)

/* State Transitions:
ToBeRecognized     (sideEvtRedeem)           Redeeming
ToBeRecognized     (sideEvtRedeemable)       Redeemable
ToBeRecognized     (sideEvtLostAndFound)     LostAndFound
Redeemable         (sideEvtRedeem)           Redeeming
Redeemable         (sideEvtChangeAddr)       HandingOver
HandingOver        (mainEvtFinishConverting) HandedOver
HandedOver         (sideEvtConvert)          Redeemable
LostAndFound       (sideEvtRedeem)           LostAndReturn
LostAndReturn      (mainEvtRedeemOrReturn)   LostAndReturnToDel
LostAndReturnToDel (sideEvtDeleted)          DELETED
Redeeming          (mainEvtRedeemOrReturn)   RedeemingToDel
RedeemingToDel     (sideEvtDeleted)          DELETED
*/

var (
	ErrCovenantAddrMismatch      = errors.New("Covenant Address Mismatch")
	ErrDuplicatedUtxo            = errors.New("Find Duplicated Utxo")
	ErrFailedToCreate            = errors.New("Failed to Create")
	ErrInvalidSourceType         = errors.New("Invalid Source Type")
	ErrNotFound                  = errors.New("UTXO Not Found")
	ErrNotHandedOver             = errors.New("Not HandedOver Status")
	ErrNotLostAndFound           = errors.New("Not LostAndFound Status")
	ErrNotLostAndReturn          = errors.New("Not LostAndReturn Status")
	ErrNotLostAndReturnToDel     = errors.New("Not LostAndReturnToDel Status")
	ErrNotRedeemable             = errors.New("Not Redeemable Status")
	ErrNotRedeemOrReturn         = errors.New("Not Redeemable or LostAndReturn Status")
	ErrNotRedeemingToDel         = errors.New("Not Redeeming Status")
	ErrNotToBeRecognized         = errors.New("Not ToBeRecognized Status")
	ErrIncorrectBurningAddress   = errors.New("Incorrect Burning Address")
	ErrReceiverNotOldOwner       = errors.New("Receiver Not Old Owner")
	ErrReceiverNotRedeemTarget   = errors.New("Receiver Not RedeemTarget")
	ErrNoBlockFoundAtGivenHeight = errors.New("No Block Found at the Given Height")
	ErrNoBlockFoundForGivenHash  = errors.New("No Block Found for the Given Hash")
)

var (
	EventNewRedeemable   = crypto.Keccak256Hash([]byte("NewRedeemable(uint256,uint32,address)"))
	EventNewLostAndFound = crypto.Keccak256Hash([]byte("NewLostAndFound(uint256,uint32,address)"))
	EventRedeem          = crypto.Keccak256Hash([]byte("Redeem(uint256,uint32,address,uint8)"))
	EventChangeAddr      = crypto.Keccak256Hash([]byte("ChangeAddr(address,address)"))
	EventConvert         = crypto.Keccak256Hash([]byte("Convert(uint256,uint32,address,uint256,uint32,address)"))
	EventDeleted         = crypto.Keccak256Hash([]byte("Deleted(uint256,uint32,address,uint8)"))

	// main chain burn address legacy format: 1SmartBCHBurnAddressxxxxxxy31qJGb
	MainChainBurningAddress string = "qqzdl8vlah353f0cyvmuapag9xlzyq9w6cul36akp5" //cash address format
	BurnAddressMainChain = []byte("\x04\xdf\x9d\x9f\xed\xe3\x48\xa5\xf8\x23\x37\xce\x87\xa8\x29\xbe\x22\x00\xae\xd6")
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
	LastRescanTime   int64
	ScannedHeight    int64
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

func getMetaInfo(tx *gorm.DB) (info MetaInfo, err error) {
	result := tx.First(&info)
	if result.Error != nil {
		return info, result.Error
	}
	return
}

func updateLastRescanTime(tx *gorm.DB, lastRescanTime int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	tx.Model(&oldInfo).Update("LastRescanTime", lastRescanTime)
	return nil
}

func updateScannedHeightAndTime(tx *gorm.DB, scannedHeight int64, lastRescanTime int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	tx.Model(&oldInfo).Updates(MetaInfo{ScannedHeight: scannedHeight, LastRescanTime: lastRescanTime})
	return nil
}

func updateMainChainHeight(tx *gorm.DB, mainChainHeight int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	tx.Model(&oldInfo).Update("MainChainHeight", mainChainHeight)
	return nil
}

func updateSideChainHeight(tx *gorm.DB, sideChainHeight int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	tx.Model(&oldInfo).Update("SideChainHeight", sideChainHeight)
	return nil
}

func updateCovenantAddr(tx *gorm.DB, currAddr, lastAddr string) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	tx.Model(&oldInfo).Updates(MetaInfo{CurrCovenantAddr: currAddr, LastCovenantAddr: lastAddr})
	return nil
}

func getUtxoSet(tx *gorm.DB) map[[36]byte]struct{} {
	result := make(map[[36]byte]struct{})
	var utxoList []CcUtxo
	tx.Find(&utxoList)
	for _, utxo := range utxoList {
		var id [36]byte
		copy(id[:32], utxo.Txid)
		binary.BigEndian.PutUint32(id[32:], uint32(utxo.Vout))
		result[id] = struct{}{}
	}
	return result
}

func addToBeRecognized(tx *gorm.DB, newCcUtxo CcUtxo) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", newCcUtxo.Txid, newCcUtxo.Vout)
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrDuplicatedUtxo
	}

	result = tx.Create(&newCcUtxo)

	if result.Error != nil {
		return ErrFailedToCreate
	}
	return nil
}

func sideEvtRedeemable(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != ToBeRecognized {
		return ErrNotToBeRecognized
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	tx.Model(&utxo).Update("Type", Redeemable)
	return nil
}

func sideEvtLostAndFound(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != ToBeRecognized {
		return ErrNotToBeRecognized
	}
	if utxo.CovenantAddr != covenantAddr {
		return ErrCovenantAddrMismatch
	}
	tx.Model(&utxo).Update("Type", LostAndFound)
	return nil
}

func sideEvtRedeem(tx *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8, redeemTarget string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
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
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		return nil
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndFound {
			return ErrNotLostAndFound
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: LostAndReturn, RedeemTarget: redeemTarget})
		return nil
	} else if sourceType == FromBurnRedeem {
		if utxo.Type != ToBeRecognized {
			return ErrNotToBeRecognized
		}
		if redeemTarget != MainChainBurningAddress {
			return ErrIncorrectBurningAddress
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		return nil
	}
	return ErrInvalidSourceType
}

func mainEvtRedeemOrReturn(tx *gorm.DB, txid string, vout uint32, receiver string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
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
		tx.Model(&utxo).Update("Type", RedeemingToDel)
	}
	if utxo.Type == LostAndReturn {
		if utxo.RedeemTarget != receiver {
			return ErrReceiverNotOldOwner
		}
		tx.Model(&utxo).Update("Type", LostAndReturnToDel)
	}
	return nil
}

func mainEvtFinishConverting(tx *gorm.DB, txid string, vout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != HandingOver {
		return ErrNotHandedOver
	}
	if utxo.CovenantAddr != newCovenantAddr {
		return ErrCovenantAddrMismatch
	}
	tx.Model(&utxo).Update("Type", HandedOver)
	return nil
}

func sideEvtChangeAddr(tx *gorm.DB, oldCovenantAddr, newCovenantAddr string) error {
	var utxoList []CcUtxo
	result := tx.Find(&utxoList, "CovenantAddr = ? AND Type = ?", oldCovenantAddr, Redeemable)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	for _, utxo := range utxoList {
		tx.Model(&utxo).Updates(CcUtxo{Type: HandingOver, CovenantAddr: newCovenantAddr})
	}
	return nil
}

func sideEvtConvert(tx *gorm.DB, txid string, vout uint32, newTxid string, newVout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if utxo.Type != HandedOver {
		return ErrNotHandedOver
	}
	if utxo.CovenantAddr != newCovenantAddr {
		return ErrCovenantAddrMismatch
	}
	tx.Model(&utxo).Updates(CcUtxo{Type: Redeemable, Txid: newTxid, Vout: newVout})
	return nil
}

func sideEvtDeleted(tx *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
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
		tx.Delete(&utxo)
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndReturnToDel {
			return ErrNotLostAndReturnToDel
		}
		tx.Delete(&utxo)
	} else {
		return ErrInvalidSourceType
	}
	return nil
}

