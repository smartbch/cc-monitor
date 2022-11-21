package monitor

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"

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

	MaxAmount          = 1000 * 10000_0000 //1000 BCH
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
	ErrNoBlockFoundAtGivenHeight = errors.New("No Block Found at the Given Height")
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

type FatalError struct {
	errStr string
}

func (e FatalError) Error() string {
	return e.errStr
}

func NewFatal(s string) FatalError {
	return FatalError{errStr: s}
}

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
	AmountX24        [24]int64 // The total amount transferred in each hour in the past 24 hours
	TimestampX24     [24]int64 // the corresponding hour of the slots in AmountX24
	IsPaused         bool
}

func (m *MetaInfo) incrAmountInSlidingWindow(amount, currTime int64) {
	hour := currTime/3600
	slot := hour%24;
	if hour != m.TimestampX24[slot] { // if this slot is out of sliding window
		m.AmountX24[slot] = 0 // reset the amount
		m.TimestampX24[slot] = hour // update the time
	}
	m.AmountX24[slot] += amount
}

func (m *MetaInfo) getSumInSlidingWindow(currTime int64) (sum int64) {
	hour := currTime/3600
	for i, a := range m.AmountX24 {
		if hour - 24 <= m.TimestampX24[i] {
			sum += a
		}
	}
	return
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

func getMetaInfo(tx *gorm.DB) (info MetaInfo) {
	result := tx.First(&info)
	if result.Error != nil {
		panic(result.Error)
	}
	return info
}

func updateLastRescanTime(tx *gorm.DB, lastRescanTime int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		return NewFatal("Cannot find MetaInfo, during updateLastRescanTime")
	}
	tx.Model(&oldInfo).Update("LastRescanTime", lastRescanTime)
	return nil
}

func updateMainChainHeight(tx *gorm.DB, mainChainHeight int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		return NewFatal("Cannot find MetaInfo, during updateMainChainHeight")
	}
	tx.Model(&oldInfo).Update("MainChainHeight", mainChainHeight)
	return nil
}

func updateCovenantAddr(tx *gorm.DB, lastAddr, currAddr string) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		return NewFatal("Cannot find MetaInfo, during updateCovenantAddr")
	}
	tx.Model(&oldInfo).Updates(MetaInfo{CurrCovenantAddr: currAddr, LastCovenantAddr: lastAddr})
	return nil
}

func getUtxoSet(tx *gorm.DB, waitingMainChain bool) (map[[36]byte]struct{}) {
	result := make(map[[36]byte]struct{})
	var utxoList []CcUtxo
	if waitingMainChain { // only the UTXOs what are waiting to be moved on the main chain
		// utxo.Type == LostAndReturn || utxo.Type == Redeeming || utxo.Type == HandingOver
		tx.Find(&utxoList, "Type IN ?", []int{LostAndReturn, Redeeming, HandingOver})
	} else {
		tx.Find(&utxoList)
	}
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
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(newCcUtxo.Txid)), newCcUtxo.Vout, utxo)
		return NewFatal("[addToBeRecognized] This UTXO was already already added: "+s)
	}

	result = tx.Create(&newCcUtxo)

	if result.Error != nil {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(newCcUtxo.Txid)), newCcUtxo.Vout, utxo)
		return NewFatal("[addToBeRecognized] Cannot create new entry: "+s)
	}
	return nil
}

func sideEvtRedeemable(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtRedeemable] This UTXO cannot be found: "+s)
	}
	if utxo.Type != ToBeRecognized {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtRedeemable] UTXO's old type is not ToBeRecognized "+s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtRedeemable] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	tx.Model(&utxo).Update("Type", Redeemable)
	return nil
}

func sideEvtLostAndFound(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtLostAndFound] This UTXO cannot be found: "+s)
	}
	if utxo.Type != ToBeRecognized {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtLostAndFound] UTXO's old type is not ToBeRecognized "+s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtLostAndFound] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	tx.Model(&utxo).Update("Type", LostAndFound)
	return nil
}

func sideEvtRedeem(tx *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8, redeemTarget string,
	meta *MetaInfo, currTime int64) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtRedeem] This UTXO cannot be found: "+s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtRedeem] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	if sourceType == FromRedeemable {
		if utxo.Type != Redeemable {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not Redeemable "+s)
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndFound {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not LostAndFound "+s)
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: LostAndReturn, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	} else if sourceType == FromBurnRedeem {
		if utxo.Type != ToBeRecognized {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not ToBeRecognized "+s)
		}
		if redeemTarget != MainChainBurningAddress {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's redeem target is not BurningAddr " + s +
				"target: "+ hex.EncodeToString([]byte(redeemTarget)))
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	}
	debug.PrintStack()
	s := fmt.Sprintf("sourceType=%d Txid=%s vout=%d\n%s\n", sourceType, hex.EncodeToString([]byte(txid)), vout, utxo)
	return NewFatal("[sideEvtRedeem] Invalid sidechain event has invalid sourceType: "+s)
}

func mainEvtRedeemOrReturn(tx *gorm.DB, txid string, vout uint32, receiver string, writeBack bool) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[mainEvtRedeemOrReturn] This UTXO cannot be found: "+s)
	}
	if utxo.Type != Redeeming && utxo.Type != LostAndReturn {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[mainEvtRedeemOrReturn] UTXO's old type is not Redeeming or LostAndReturn "+s)
	}
	if utxo.RedeemTarget != receiver {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[mainEvtRedeemOrReturn] UTXO's recorded target (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.RedeemTarget)), hex.EncodeToString([]byte(receiver))))
	}
	if writeBack {
		if utxo.Type == Redeeming {
			tx.Model(&utxo).Update("Type", RedeemingToDel)
		}
		if utxo.Type == LostAndReturn {
			tx.Model(&utxo).Update("Type", LostAndReturnToDel)
		}
	}
	return nil
}

func mainEvtFinishConverting(tx *gorm.DB, txid string, vout uint32, newCovenantAddr string, writeBack bool) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[mainEvtFinishConverting] This UTXO cannot be found: "+s)
	}
	if utxo.Type != HandingOver {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[mainEvtFinishConverting] UTXO's old type is not HandingOver "+s)
	}
	if utxo.CovenantAddr != newCovenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtRedeemable] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(newCovenantAddr))))
	}
	if writeBack {
		tx.Model(&utxo).Update("Type", HandedOver)
	}
	return nil
}

func sideEvtChangeAddr(tx *gorm.DB, oldCovenantAddr, newCovenantAddr string) error {
	var utxoList []CcUtxo
	result := tx.Find(&utxoList, "CovenantAddr = ? AND Type = ?", oldCovenantAddr, Redeemable)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		return NewFatal("[sideEvtChangeAddr] Cannot find redeemable UTXO with oldCovenantAddr: "+
			hex.EncodeToString([]byte(oldCovenantAddr)))
	}
	for _, utxo := range utxoList {
		tx.Model(&utxo).Updates(CcUtxo{Type: HandingOver, CovenantAddr: newCovenantAddr})
	}
	return updateCovenantAddr(tx, oldCovenantAddr, newCovenantAddr)
}

func sideEvtConvert(tx *gorm.DB, txid string, vout uint32, newTxid string, newVout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtConvert] This UTXO cannot be found: "+s)
	}
	if utxo.Type != HandedOver {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtConvert] UTXO's old type is not HandedOver "+s)
	}
	if utxo.CovenantAddr != newCovenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtConvert] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(newCovenantAddr))))
	}
	tx.Model(&utxo).Updates(CcUtxo{Type: Redeemable, Txid: newTxid, Vout: newVout})
	return nil
}

func sideEvtDeleted(tx *gorm.DB, covenantAddr string, txid string, vout uint32, sourceType uint8) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtDeleted] This UTXO cannot be found: "+s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtDeleted] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	if sourceType == FromRedeeming {
		if utxo.Type != RedeemingToDel {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtDeleted] FromRedeeming-UTXO's old type is not RedeemingToDel "+s)
		}
		tx.Delete(&utxo)
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndReturnToDel {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%s\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtDeleted] FromLostAndFound-UTXO's old type is not LostAndReturnToDel "+s)
		}
		tx.Delete(&utxo)
	} else {
		debug.PrintStack()
		s := fmt.Sprintf("sourceType=%d Txid=%s vout=%d\n%s\n", sourceType, hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtDeleted] Invalid sidechain event has invalid sourceType: "+s)
	}
	return nil
}

