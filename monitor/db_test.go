package monitor

import (
	"strings"
	"testing"
	"os"

	"github.com/stretchr/testify/require"
	//"gorm.io/gorm"
)

//func MigrateSchema(db *gorm.DB) {
//func OpenDB(path string) *gorm.DB {
//func getMetaInfo(tx *gorm.DB) (info MetaInfo) {
//func initMetaInfo(tx *gorm.DB, info MetaInfo) error {
//func updateLastRescanTime(tx *gorm.DB, lastRescanTime int64) error {
//func updateMainChainHeight(tx *gorm.DB, mainChainHeight int64) error {
//func updateCovenantAddr(tx *gorm.DB, lastAddr, currAddr string) error {

func (m *MetaInfo) Equals(x MetaInfo) bool {
	return  m.LastRescanTime   == x.LastRescanTime   &&
		m.ScannedHeight    == x.ScannedHeight    &&
		m.MainChainHeight  == x.MainChainHeight  &&
		m.SideChainHeight  == x.SideChainHeight  &&
		m.CurrCovenantAddr == x.CurrCovenantAddr &&
		m.LastCovenantAddr == x.LastCovenantAddr &&
		m.AmountX24        == x.AmountX24        &&
		m.TimestampX24     == x.TimestampX24     &&
		m.IsPaused         == x.IsPaused
}

func TestSlidingWindow(t *testing.T) {
	var info MetaInfo
	info.TimestampX24 = ZeroX24Json
	info.AmountX24 = ZeroX24Json
	for i := 0; i < 24; i++ {
		info.incrAmountInSlidingWindow(1, int64(3600*i))
	}
	sum := info.getSumInSlidingWindow(3600*24)
	require.Equal(t, int64(24), sum)
	sum = info.getSumInSlidingWindow(3600*36)
	require.Equal(t, int64(12), sum)
	info.incrAmountInSlidingWindow(5, int64(3600*25))
	sum = info.getSumInSlidingWindow(3600*24)
	require.Equal(t, int64(28), sum)
	info.incrAmountInSlidingWindow(5, int64(3600*25)+10)
	sum = info.getSumInSlidingWindow(3600*24)
	require.Equal(t, int64(32), sum)
	sum = info.getSumInSlidingWindow(3600*36)
	require.Equal(t, int64(20), sum)
}

func TestDB0(t *testing.T) {
	os.RemoveAll("./testdb.db")
	db := OpenDB("./testdb.db")
	MigrateSchema(db)
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "ccaddr1",
		LastCovenantAddr: "ccaddr0",
	}
	initMetaInfo(db, &info)
	err := sideEvtRedeemable(db, "covenantAddr", "txid", 0)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = sideEvtLostAndFound(db, "covenantAddr", "txid", 0)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = sideEvtRedeem(db, "covenantAddr", "txid", 0, FromRedeemable, "redeemTarget", &MetaInfo{}, 10000)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = mainEvtRedeemOrReturn(db, "txid", 0, "receiver", false)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = mainEvtFinishConverting(db, "txid", 0, "newCovenantAddr", "newTxid", 1, false)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = sideEvtConvert(db, "txid", 0, "newTxid", 1, "newCovenantAddr")
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)
	err = sideEvtDeleted(db, "covenantAddr", "txid", 0, FromRedeeming)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	os.RemoveAll("./testdb.db")
}

	// ToBeRecognized     (sideEvtRedeem)           Redeeming (burning address)
	// Redeeming          (mainEvtRedeemOrReturn)   RedeemingToDel
	// RedeemingToDel     (sideEvtDeleted)          DELETED
func TestDB1(t *testing.T) {
	os.RemoveAll("./testdb.db")
	db := OpenDB("./testdb.db")
	MigrateSchema(db)
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "ccaddr1",
		LastCovenantAddr: "ccaddr0",
	}
	initMetaInfo(db, &info)
	utxo := CcUtxo{
		Type:         ToBeRecognized,
		CovenantAddr: "ccaddr1",
		RedeemTarget: "",
		Amount:       100,
		Txid:         "txid1",
		Vout:         0,
	}
	err := sideEvtRedeemable(db, utxo.CovenantAddr, utxo.Txid, utxo.Vout)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	err = addToBeRecognized(db, utxo)
	require.Nil(t, err)
	err = addToBeRecognized(db, utxo)
	require.True(t, strings.Index(err.Error(), "UTXO was already added") > 0)
	err = sideEvtRedeem(db, "ccaddr1000", "txid1", 0, FromBurnRedeem, BurnAddressMainChain, &info, 3600)
	require.True(t, strings.Index(err.Error(), "recorded covenantAddr") > 0)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromRedeeming, BurnAddressMainChain, &info, 3600)
	require.True(t, strings.Index(err.Error(), "sidechain event has invalid sourceType") > 0)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromRedeemable, BurnAddressMainChain, &info, 3600)
	require.True(t, strings.Index(err.Error(), "old type is not Redeemable") > 0)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromLostAndFound, BurnAddressMainChain, &info, 3600)
	require.True(t, strings.Index(err.Error(), "old type is not LostAndReturn") > 0)
	err = mainEvtRedeemOrReturn(db, "txid1", 0, "target1", true)
	require.True(t, strings.Index(err.Error(), "UTXO's old type is not Redeeming or LostAndReturn") > 0)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromBurnRedeem, BurnAddressMainChain, &info, 3600)
	require.Nil(t, err)
	err = mainEvtRedeemOrReturn(db, "txid1", 0, "target1", true)
	require.Nil(t, err)
	err = sideEvtDeleted(db, "ccaddr1", "txid1", 0, FromRedeeming)
	require.Nil(t, err)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromBurnRedeem, "target1", &info, 3600)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	os.RemoveAll("./testdb.db")
}

	// ToBeRecognized     (sideEvtRedeemable)       Redeemable
	// Redeemable         (sideEvtRedeem)           Redeeming
	// Redeeming          (mainEvtRedeemOrReturn)   RedeemingToDel
	// RedeemingToDel     (sideEvtDeleted)          DELETED
func TestDB2(t *testing.T) {
	os.RemoveAll("./testdb.db")
	db := OpenDB("./testdb.db")
	MigrateSchema(db)
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "ccaddr1",
		LastCovenantAddr: "ccaddr0",
	}
	initMetaInfo(db, &info)
	utxo := CcUtxo{
		Type:         ToBeRecognized,
		CovenantAddr: "ccaddr1",
		RedeemTarget: "",
		Amount:       100,
		Txid:         "txid1",
		Vout:         0,
	}
	err := sideEvtRedeemable(db, utxo.CovenantAddr, utxo.Txid, utxo.Vout)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	// Normal Flow:
	err = addToBeRecognized(db, utxo)
	require.Nil(t, err)
	err = sideEvtRedeemable(db, "ccaddr1", "txid1", 0)
	require.Nil(t, err)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromRedeemable, "target1", &info, 3600)
	require.Nil(t, err)
	err = mainEvtRedeemOrReturn(db, "txid1", 0, "target1", true)
	require.Nil(t, err)
	err = sideEvtDeleted(db, "ccaddr1", "txid1", 0, FromRedeeming)
	require.Nil(t, err)
}

	// ToBeRecognized     (sideEvtLostAndFound)     LostAndFound
	// LostAndFound       (sideEvtRedeem)           LostAndReturn
	// LostAndReturn      (mainEvtRedeemOrReturn)   LostAndReturnToDel
	// LostAndReturnToDel (sideEvtDeleted)          DELETED
func TestDB3(t *testing.T) {
	os.RemoveAll("./testdb.db")
	db := OpenDB("./testdb.db")
	MigrateSchema(db)
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "ccaddr1",
		LastCovenantAddr: "ccaddr0",
	}
	initMetaInfo(db, &info)
	utxo := CcUtxo{
		Type:         ToBeRecognized,
		CovenantAddr: "ccaddr1",
		RedeemTarget: "",
		Amount:       100,
		Txid:         "txid1",
		Vout:         0,
	}
	err := sideEvtRedeemable(db, utxo.CovenantAddr, utxo.Txid, utxo.Vout)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	// Normal Flow:
	err = addToBeRecognized(db, utxo)
	require.Nil(t, err)
	err = sideEvtLostAndFound(db, "ccaddr1", "txid1", 0)
	require.Nil(t, err)
	err = sideEvtRedeem(db, "ccaddr1", "txid1", 0, FromLostAndFound, "target1", &info, 3600)
	require.Nil(t, err)
	err = mainEvtRedeemOrReturn(db, "txid1", 0, "target1", true)
	require.Nil(t, err)
	err = sideEvtDeleted(db, "ccaddr1", "txid1", 0, FromLostAndFound)
	require.Nil(t, err)
}

	// Redeemable         (sideEvtChangeAddr)       HandingOver
	// HandingOver        (mainEvtFinishConverting) HandedOver
	// HandedOver         (sideEvtConvert)          Redeemable
func TestDB4(t *testing.T) {
	os.RemoveAll("./testdb.db")
	db := OpenDB("./testdb.db")
	MigrateSchema(db)
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "ccaddr1",
		LastCovenantAddr: "ccaddr0",
	}
	initMetaInfo(db, &info)
	utxo := CcUtxo{
		Type:         ToBeRecognized,
		CovenantAddr: "ccaddr1",
		RedeemTarget: "",
		Amount:       100,
		Txid:         "txid1",
		Vout:         0,
	}
	err := sideEvtRedeemable(db, utxo.CovenantAddr, utxo.Txid, utxo.Vout)
	require.True(t, strings.Index(err.Error(), "This UTXO cannot be found") > 0)

	// Normal Flow:
	err = addToBeRecognized(db, utxo)
	require.Nil(t, err)
	err = sideEvtRedeemable(db, "ccaddr1", "txid1", 0)
	require.Nil(t, err)
	err = sideEvtChangeAddr(db, "ccaddr1", "ccaddr2")
	require.Nil(t, err)
	err = mainEvtFinishConverting(db, "txid1", 0, "ccaddr2", "txid2", 1, true)
	require.Nil(t, err)
	err = sideEvtConvert(db, "txid1", 0, "txid2", 1, "ccaddr2")
	require.Nil(t, err)

	utxo2 := CcUtxo{
		Type:         ToBeRecognized,
		CovenantAddr: "ccaddr2",
		RedeemTarget: "",
		Amount:       100,
		Txid:         "TXID1",
		Vout:         1,
	}
	err = addToBeRecognized(db, utxo2)
	require.Nil(t, err)
	err = sideEvtRedeemable(db, "ccaddr2", "TXID1", 1)
	require.Nil(t, err)
	err = sideEvtChangeAddr(db, "ccaddr2", "ccaddr3")
	require.Nil(t, err)
	err = mainEvtFinishConverting(db, "txid1", 0, "ccaddr3", "txid2", 2, true)
	require.Nil(t, err)
	err = mainEvtFinishConverting(db, "TXID1", 1, "ccaddr3", "TXID2", 3, true)
	require.Nil(t, err)
	err = sideEvtConvert(db, "txid1", 0, "txid2", 2, "ccaddr3")
	require.Nil(t, err)
	err = sideEvtConvert(db, "TXID1", 1, "TXID2", 3, "ccaddr3")
	require.Nil(t, err)

	os.RemoveAll("./testdb.db")
}

func TestMetaInfo(t *testing.T) {
	os.RemoveAll("./testmeta.db")
	db := OpenDB("./testmeta.db")
	MigrateSchema(db)
	require.Panics(t, func() {
		getMetaInfo(db)
	})
	require.Panics(t, func() {
		updateLastRescanTime(db, 11)
	})
	require.Panics(t, func() {
		updateMainChainHeight(db, 201)
	})
	require.Panics(t, func() {
		updateCovenantAddr(db, "ADDR0", "ADDR1")
	})

	// set the value
	info := MetaInfo{
		LastRescanTime:   10,
		ScannedHeight:    100,
		MainChainHeight:  200,
		SideChainHeight:  300,
		CurrCovenantAddr: "addr1",
		LastCovenantAddr: "addr0",
	}
	initMetaInfo(db, &info)

	// get the value
	info2 := getMetaInfo(db)
	require.True(t, info.Equals(info2))

	// reopen the db and get the value
	db = OpenDB("./test.db")
	info2 = getMetaInfo(db)
	require.True(t, info.Equals(info2))

	// update fields
	err := updateLastRescanTime(db, 11)
	require.Nil(t, err)
	err = updateMainChainHeight(db, 201)
	require.Nil(t, err)
	err = updateCovenantAddr(db, "ADDR0", "ADDR1")
	require.Nil(t, err)
	info.LastRescanTime = 11
	info.MainChainHeight = 201
	info.CurrCovenantAddr = "ADDR1"
	info.LastCovenantAddr = "ADDR0"
	info2 = getMetaInfo(db)
	require.True(t, info.Equals(info2))
	os.RemoveAll("./testmeta.db")
}


