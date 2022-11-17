package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gcash/bchd/rpcclient"
	"gorm.io/gorm"

	"github.com/smartbch/cc-monitor/monitor"
)

// ==================

type Context struct {
	db                    *gorm.DB
	lock                  sync.RWMutex
	sideChainBlockScanner *monitor.BlockScanner
	mainnetBlockWatcher   *monitor.BlockWatcher
	operatorsWatcher      *monitor.OperatorsWatcher

	currMainnetHeight      int64
	currSideChainHeight    int64
	totalUnhandledUtxoNums uint32
	prevStartRescanHeight  int64
}

var (
	MaxUnhandledUtxoNums           uint32 = 100
	MaxBlockIntervalBetweenRescans int64  = 300
)

func (c *Context) checkCallStartRescan() bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if c.totalUnhandledUtxoNums >= MaxUnhandledUtxoNums {
		return true
	}
	if c.currMainnetHeight-c.prevStartRescanHeight >= MaxBlockIntervalBetweenRescans {
		return true
	}
	return false
}

func (c *Context) refreshContext() {
	c.totalUnhandledUtxoNums = 0
	c.prevStartRescanHeight = c.currMainnetHeight
}

// retry until success
func sendStartRescan(height int64) {

}

var (
	monitorPrivateKey string
	sideChainUrl      string
	mainnetUrl        string
	mainnetUsername   string
	mainnetPassword   string
	lastRescanHeight  int64
	lastRescanTime    int64
)

const (
	handleUtxoDelay int64 = 1 * 60
)

func main() {
	flag.StringVar(&monitorPrivateKey, "key", "", "monitor private key")
	flag.StringVar(&sideChainUrl, "sbchUrl", "http://localhost:8545", "side chain rpc url")
	flag.StringVar(&mainnetUrl, "mainnetUrl", "http://localhost:8332", "mainnet url")
	flag.StringVar(&mainnetUsername, "mainnetUsername", "", "mainnet url username")
	flag.StringVar(&mainnetPassword, "mainnetPassword", "", "mainnet url password")
	flag.Int64Var(&lastRescanHeight, "lastRescanHeight", 1, "last rescan mainnet height")
	flag.Int64Var(&lastRescanTime, "lastRescanTime", 0, "last rescan time")
	flag.Parse()
	c, err := ethclient.Dial(sideChainUrl)
	if err != nil {
		panic(err)
	}
	connCfg := &rpcclient.ConnConfig{
		Host:         mainnetUrl,
		User:         mainnetUsername,
		Pass:         mainnetPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		panic(err)
	}
	bz, err := hex.DecodeString(monitorPrivateKey)
	if err == nil {
		monitor.MyPrivKey, err = crypto.ToECDSA(bz)
	}
	if err != nil {
		fmt.Println("Cannot decode monitor private key hex string")
		panic(err)
	}
	monitor.MyAddress = crypto.PubkeyToAddress(monitor.MyPrivKey.PublicKey)
	//defer client.Shutdown()
	monitor.SendStartRescanAndHandleUTXO(context.Background(), c, client, lastRescanHeight, lastRescanTime, handleUtxoDelay)
	select {}
}
