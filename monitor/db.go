package monitor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"

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

	MaxAmount = 1000 * 10000_0000 //1000 BCH
)

/* State Transitions:
ToBeRecognized     (sideEvtRedeem)           Redeeming (burning address)
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

	EventNewRedeemable   = crypto.Keccak256Hash([]byte("NewRedeemable(uint256,uint32,address)"))
	EventNewLostAndFound = crypto.Keccak256Hash([]byte("NewLostAndFound(uint256,uint32,address)"))
	EventRedeem          = crypto.Keccak256Hash([]byte("Redeem(uint256,uint32,address,uint8)"))
	EventChangeAddr      = crypto.Keccak256Hash([]byte("ChangeAddr(address,address)"))
	EventConvert         = crypto.Keccak256Hash([]byte("Convert(uint256,uint32,address,uint256,uint32,address)"))
	EventDeleted         = crypto.Keccak256Hash([]byte("Deleted(uint256,uint32,address,uint8)"))

	// main chain burn address legacy format: 1SmartBCHBurnAddressxxxxxxy31qJGb
	MainChainBurningAddress string = "qqzdl8vlah353f0cyvmuapag9xlzyq9w6cul36akp5" //cash address format
	BurnAddressMainChain           = "\x04\xdf\x9d\x9f\xed\xe3\x48\xa5\xf8\x23\x37\xce\x87\xa8\x29\xbe\x22\x00\xae\xd6"
)

const (
	FromRedeemable   = uint8(0) // when it's redeemed
	FromLostAndFound = uint8(1) // when it's redeemed or deleted
	FromRedeeming    = uint8(2) // when it's deleted
	FromBurnRedeem   = uint8(9) // when it's automatically redeemed to burning address

	ZeroX24Json = "[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]"
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
	NewTxid      string
	NewVout      uint32
}

type MetaInfo struct {
	gorm.Model
	LastRescanTime   int64
	ScannedHeight    int64
	MainChainHeight  int64
	SideChainHeight  int64
	CurrCovenantAddr string
	LastCovenantAddr string
	AmountX24        string // The total amount transferred in each hour in the past 24 hours
	TimestampX24     string // the corresponding hour-number of the slots in AmountX24
	IsPaused         bool
}

func (m *MetaInfo) incrAmountInSlidingWindow(amount, currTime int64) {
	fmt.Printf("before incrAmountInSlidingWindow: amount %s\ntimestamp %s\n", m.AmountX24, m.TimestampX24)
	hour := currTime / 3600
	slot := hour % 24
	var timestampX24 []int64
	var amountX24 []int64
	if err := json.Unmarshal([]byte(m.TimestampX24), &timestampX24); err != nil {
		panic(err)
	}
	if err := json.Unmarshal([]byte(m.AmountX24), &amountX24); err != nil {
		panic(err)
	}
	if hour != timestampX24[slot] { // if this slot is out of sliding window (more than 24 hours ago)
		amountX24[slot] = 0       // reset the amount
		timestampX24[slot] = hour // update the hour-number
	}
	amountX24[slot] += amount
	if bz, err := json.Marshal(timestampX24); err != nil {
		panic(err)
	} else {
		m.TimestampX24 = string(bz)
	}
	if bz, err := json.Marshal(amountX24); err != nil {
		panic(err)
	} else {
		m.AmountX24 = string(bz)
	}
	fmt.Printf("after incrAmountInSlidingWindow: amount %s\ntimestamp %s\n", m.AmountX24, m.TimestampX24)
}

func (m *MetaInfo) getSumInSlidingWindow(currTime int64) (sum int64) {
	var timestampX24 []int64
	var amountX24 []int64
	if err := json.Unmarshal([]byte(m.TimestampX24), &timestampX24); err != nil {
		panic(err)
	}
	if err := json.Unmarshal([]byte(m.AmountX24), &amountX24); err != nil {
		panic(err)
	}
	fmt.Printf("getSumInSlidingWindow %d: amount %s\ntimestamp %s\n", currTime, m.AmountX24, m.TimestampX24)
	hour := currTime / 3600
	for i, a := range amountX24 {
		if hour-24 < timestampX24[i] {
			sum += a
		}
	}
	return
}

func MigrateSchema(db *gorm.DB) {
	db.AutoMigrate(&CcUtxo{
		Type:         Redeemable,
		CovenantAddr: "CovenantAddr",
		RedeemTarget: "RedeemTarget",
		Amount:       1,
		Txid:         "txid",
		Vout:         1,
	})
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

func InitMetaInfo(tx *gorm.DB, info *MetaInfo) {
	info.TimestampX24 = ZeroX24Json
	info.AmountX24 = ZeroX24Json
	result := tx.Create(&info)
	if result.Error != nil {
		panic(result.Error)
	}
	out := getMetaInfo(tx)
	fmt.Printf("InitMetaInfo:: %#v\n", out)
}

func updateLastRescanTime(tx *gorm.DB, lastRescanTime int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		panic(result.Error)
	}
	tx.Model(&oldInfo).Update("LastRescanTime", lastRescanTime)
	return nil
}

func updateMainChainHeight(tx *gorm.DB, mainChainHeight int64) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		panic(result.Error)
	}
	tx.Model(&oldInfo).Update("MainChainHeight", mainChainHeight)
	return nil
}

func updateCovenantAddr(tx *gorm.DB, lastAddr, currAddr string) error {
	var oldInfo MetaInfo
	result := tx.First(&oldInfo)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		panic(result.Error)
	}
	tx.Model(&oldInfo).Updates(MetaInfo{CurrCovenantAddr: currAddr, LastCovenantAddr: lastAddr})
	return nil
}

func printUtxoSet(tx *gorm.DB) {
	var utxoList []CcUtxo
	tx.Find(&utxoList)
	for i, utxo := range utxoList {
		fmt.Printf("UTXO#%d %#v\n", i, utxo)
	}
}

func getUtxoSet(tx *gorm.DB, waitingMainChain bool) map[string]struct{} {
	result := make(map[string]struct{})
	var utxoList []CcUtxo
	if waitingMainChain { // only the UTXOs what are waiting to be moved on the main chain
		// utxo.Type == LostAndReturn || utxo.Type == Redeeming || utxo.Type == HandingOver
		tx.Find(&utxoList, "Type IN ?", []int{LostAndReturn, Redeeming, HandingOver})
	} else {
		tx.Find(&utxoList)
	}
	for _, utxo := range utxoList {
		key := fmt.Sprintf("%s-%d", utxo.Txid, utxo.Vout)
		result[key] = struct{}{}
	}
	return result
}

func addToBeRecognized(tx *gorm.DB, newCcUtxo CcUtxo) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", newCcUtxo.Txid, newCcUtxo.Vout)
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(newCcUtxo.Txid)), newCcUtxo.Vout, utxo)
		return NewFatal("[addToBeRecognized] This UTXO was already added: " + s)
	}

	result = tx.Create(&newCcUtxo)

	if result.Error != nil {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(newCcUtxo.Txid)), newCcUtxo.Vout, utxo)
		return NewFatal("[addToBeRecognized] Cannot create new entry: " + s)
	}
	return nil
}

func sideEvtRedeemable(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtRedeemable] This UTXO cannot be found: " + s)
	}
	fmt.Printf("[sideEvtRedeemable] old UTXO %#v\n", utxo)
	if utxo.Type != ToBeRecognized {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		fmt.Println("[sideEvtRedeemable] UTXO's old type is not ToBeRecognized " + s)
		return NewFatal("[sideEvtRedeemable] UTXO's old type is not ToBeRecognized " + s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtRedeemable] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	tx.Model(&utxo).Update("Type", Redeemable)
	tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	fmt.Printf("DBG in sideEvtRedeemable %#v\n", utxo)
	return nil
}

func sideEvtLostAndFound(tx *gorm.DB, covenantAddr string, txid string, vout uint32) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtLostAndFound] This UTXO cannot be found: " + s)
	}
	if utxo.Type != ToBeRecognized {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtLostAndFound] UTXO's old type is not ToBeRecognized " + s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
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
		return NewFatal("[sideEvtRedeem] This UTXO cannot be found: " + s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtRedeem] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	if sourceType == FromRedeemable {
		if utxo.Type != Redeemable {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not Redeemable " + s)
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndFound {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not LostAndFound " + s)
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: LostAndReturn, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	} else if sourceType == FromBurnRedeem {
		if utxo.Type != ToBeRecognized {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's old type is not ToBeRecognized " + s)
		}
		if redeemTarget != BurnAddressMainChain {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtRedeem] UTXO's redeem target is not BurningAddr " + s +
				"target: " + hex.EncodeToString([]byte(redeemTarget)))
		}
		tx.Model(&utxo).Updates(CcUtxo{Type: Redeeming, RedeemTarget: redeemTarget})
		meta.incrAmountInSlidingWindow(utxo.Amount, currTime)
		return nil
	}
	debug.PrintStack()
	s := fmt.Sprintf("sourceType=%d Txid=%s vout=%d\n%#v\n", sourceType, hex.EncodeToString([]byte(txid)), vout, utxo)
	return NewFatal("[sideEvtRedeem] Invalid sidechain event has invalid sourceType: " + s)
}

func mainEvtRedeemOrReturn(tx *gorm.DB, txid string, vout uint32, receiver string, writeBack bool) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[mainEvtRedeemOrReturn] This UTXO cannot be found: " + s)
	}
	if utxo.Type != Redeeming && utxo.Type != LostAndReturn {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[mainEvtRedeemOrReturn] UTXO's old type is not Redeeming or LostAndReturn " + s)
	}
	if writeBack {
		if utxo.Type == Redeeming {
			tx.Model(&utxo).Updates(CcUtxo{Type: RedeemingToDel, RedeemTarget: receiver})
		}
		if utxo.Type == LostAndReturn {
			tx.Model(&utxo).Updates(CcUtxo{Type: LostAndReturnToDel, RedeemTarget: receiver})
		}
	}
	return nil
}

func mainEvtFinishConverting(tx *gorm.DB, txid string, vout uint32, newCovenantAddr string,
	newTxid string, newVout uint32, writeBack bool) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[mainEvtFinishConverting] This UTXO cannot be found: " + s)
	}
	if utxo.Type != HandingOver {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[mainEvtFinishConverting] UTXO's old type is not HandingOver " + s)
	}
	if utxo.CovenantAddr != newCovenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[mainEvtFinishConverting] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(newCovenantAddr))))
	}
	if writeBack {
		tx.Model(&utxo).Updates(CcUtxo{Type: HandedOver, NewTxid: newTxid, NewVout: newVout})
	}
	return nil
}

func sideEvtChangeAddr(tx *gorm.DB, oldCovenantAddr, newCovenantAddr string) error {
	var utxoList []CcUtxo
	result := tx.Find(&utxoList, "Type = ?", Redeemable)
	fmt.Printf("sideEvtChangeAddr result %#v\n%#v\n", result, utxoList)
	if len(utxoList) == 0 {
		return nil
	}
	for _, utxo := range utxoList {
		if utxo.CovenantAddr == oldCovenantAddr {
			tx.Model(&utxo).Updates(CcUtxo{Type: HandingOver, CovenantAddr: newCovenantAddr})
		} else {
			debug.PrintStack()
			return NewFatal("[sideEvtChangeAddr] Redeemable UTXO with wrong oldCovenantAddr: " +
				hex.EncodeToString([]byte(utxo.CovenantAddr)) + " " + utxo.CovenantAddr)
		}
	}
	return updateCovenantAddr(tx, oldCovenantAddr, newCovenantAddr)
}

func sideEvtConvert(tx *gorm.DB, txid string, vout uint32, newTxid string, newVout uint32, newCovenantAddr string) error {
	var utxo CcUtxo
	result := tx.First(&utxo, "txid == ? AND vout == ?", txid, vout)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n", hex.EncodeToString([]byte(txid)), vout)
		return NewFatal("[sideEvtConvert] This UTXO cannot be found: " + s)
	}
	if utxo.Type != HandedOver {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtConvert] UTXO's old type is not HandedOver " + s)
	}
	if utxo.CovenantAddr != newCovenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtConvert] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(newCovenantAddr))))
	}
	if utxo.NewTxid != newTxid || utxo.NewVout != newVout {
		debug.PrintStack()
		s := fmt.Sprintf("newTxid=%s newVout=%d\n%#v\n", hex.EncodeToString([]byte(newTxid)), newVout, utxo)
		return NewFatal("[sideEvtConvert] mismatch of newTxid/newVout: " + s)
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
		return NewFatal("[sideEvtDeleted] This UTXO cannot be found: " + s)
	}
	if utxo.CovenantAddr != covenantAddr {
		debug.PrintStack()
		s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal(fmt.Sprintf("[sideEvtDeleted] UTXO's recorded covenantAddr (%s) is not %s"+s,
			hex.EncodeToString([]byte(utxo.CovenantAddr)), hex.EncodeToString([]byte(covenantAddr))))
	}
	if sourceType == FromRedeeming {
		if utxo.Type != RedeemingToDel {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtDeleted] FromRedeeming-UTXO's old type is not RedeemingToDel " + s)
		}
		tx.Delete(&utxo)
	} else if sourceType == FromLostAndFound {
		if utxo.Type != LostAndReturnToDel {
			debug.PrintStack()
			s := fmt.Sprintf("Txid=%s vout=%d\n%#v\n", hex.EncodeToString([]byte(txid)), vout, utxo)
			return NewFatal("[sideEvtDeleted] FromLostAndFound-UTXO's old type is not LostAndReturnToDel " + s)
		}
		tx.Delete(&utxo)
	} else {
		debug.PrintStack()
		s := fmt.Sprintf("sourceType=%d Txid=%s vout=%d\n%#v\n", sourceType, hex.EncodeToString([]byte(txid)), vout, utxo)
		return NewFatal("[sideEvtDeleted] Invalid sidechain event has invalid sourceType: " + s)
	}
	return nil
}
