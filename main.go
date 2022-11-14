package main

import (
	"context"
	"sync"

	"github.com/smartbch/cc-monitor/monitor"
	"gorm.io/gorm"
)

// ==================

type Context struct {
	db                    *gorm.DB
	lock                  sync.RWMutex
	sideChainBlockScanner *monitor.BlockScanner
	mainnetBlockWatcher   *monitor.BlockWatcher

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

func main() {
	// init db
	// init key and side chain client and server
	// recover context
	c := Context{
		sideChainBlockScanner: &monitor.BlockScanner{},
		mainnetBlockWatcher:   &monitor.BlockWatcher{},

		currMainnetHeight:      1,
		currSideChainHeight:    1,
		totalUnhandledUtxoNums: 0,
		prevStartRescanHeight:  0,
	}
	go func() {
		for /* get side chain block height*/ {
			height := int64(0)
			err := c.sideChainBlockScanner.ScanBlock(context.Background(), height)
			if err != nil {
				panic(err)
			}
		}
	}()
	go func() {
		for /* get main chain block height*/ {
			height := int64(0)
			err := c.mainnetBlockWatcher.HandleBlock(height)
			if err != nil {
				panic(err)
			}
			c.lock.Lock()
			c.currMainnetHeight = height
			c.lock.Unlock()
			if c.checkCallStartRescan() {
				sendStartRescan(height)
			}
		}
	}()
	select {}
}
