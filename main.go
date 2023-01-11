package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"strings"
	"time"

	opclient "github.com/smartbch/cc-operator/client"
	"github.com/smartbch/cc-operator/sbch"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gcash/bchd/rpcclient"
	sbchrpcclient "github.com/smartbch/smartbch/rpc/client"

	"github.com/smartbch/cc-monitor/monitor"
)

// ==================

var (
	withCatchup       bool
	monitorPrivateKey string
	operatorList      string
	currCovAddr       string
	nodeGovAddr       string
	sideChainUrl      string
	mainnetUrl        string
	mainnetUsername   string
	mainnetPassword   string
	dbPath            string
	lastRescanTime    int64
	lastRescanHeight  int64
	mainChainHeight   int64
)

const (
	handleUtxoDelay int64 = 1 * 60
)

func main() {
	//monitor.LoadPrivKeyInHex("308b3d6401d489ec3d6f11b9be8dd3627d274d0b830340fb23b4abb55a6895d1")
	//monitor.DebugWatcher()
	run()
	//simpleRun()
}

func run() {
	parseFlags()
	if len(monitorPrivateKey) != 0 {
		monitor.LoadPrivKeyInHex(monitorPrivateKey)
	} else {
		monitor.ReadPrivKey()
	}
	//monitor.LoadPrivKeyInHex("d248df3c728282a66521c94a4852c2d4c7b3c3612ba5ce0baf43e64b2ecc49fb")
	//monitor.LoadPrivKeyInHex("308b3d6401d489ec3d6f11b9be8dd3627d274d0b830340fb23b4abb55a6895d1")
	sbchClient, err := sbchrpcclient.Dial(sideChainUrl)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	ccInfo, err := sbchClient.CcInfo(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%#v\n", ccInfo)
        rpcClient, err := rpc.Dial(sideChainUrl)
        if err != nil {
                panic(err)
        }
	ccInfosForTest := monitor.GetCcInfosForTest(ctx, rpcClient)
	fmt.Printf("%#v\n", ccInfosForTest)
	for i, mon := range ccInfo.Monitors {
		fmt.Printf("Monitor %d %#v\n", i, mon)
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         mainnetUrl,
		User:         mainnetUsername,
		Pass:         mainnetPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	fmt.Printf("connCfg %#v\n", connCfg)
	bchClient, err := rpcclient.New(connCfg, nil)
	if err != nil {
		panic(err)
	}
	db := monitor.OpenDB(dbPath)

	//monitor.PrintAllUtxo(db)
	//panic("Stop")

	if withCatchup {
		lastCovenantAddr := hexutil.MustDecode("0x0000000000000000000000000000000000000000")
		currCovenantAddr := hexutil.MustDecode(currCovAddr)
		fmt.Printf("CovenantAddr %s last %s\n", monitor.ScriptHashToAddr(currCovenantAddr), monitor.ScriptHashToAddr(lastCovenantAddr))

		info := monitor.MetaInfo{
			LastRescanTime:   -1,
			ScannedHeight:    int64(ccInfo.RescannedHeight),
			MainChainHeight:  mainChainHeight, //1534052,
			SideChainHeight:  1,
			LastCovenantAddr: string(lastCovenantAddr[:]),
			CurrCovenantAddr: string(currCovenantAddr[:]),
		}
		monitor.MigrateSchema(db)
		monitor.InitMetaInfo(db, &info)
		monitor.InitTotalAmount(db)
	}

	bs := monitor.NewBlockScanner(bchClient, db, sideChainUrl)

	//"0xb56393d9c5fae775c8B846e8E03B2954a68D3094",
	simpleRpcClient, err := sbch.NewSimpleRpcClient(nodeGovAddr,
		sideChainUrl, 10 * time.Second)
	if err != nil {
		panic(err)
	}
	opList := strings.Split(operatorList, ",")
	opClients := make([]*opclient.Client, len(opList))
	for i, op := range opList {
		opClients[i] = opclient.NewClient(op, 10 * time.Second)
	}

	//monitor.SendPauseAndResume(bs)
	//panic("Stop Here")

	watcher := monitor.NewOperatorsWatcher(simpleRpcClient, opClients)
	if withCatchup {
		monitor.Catchup(bs)
	}
	monitor.MainLoop(bs, watcher)
}

func parseFlags() {
	flag.BoolVar(&withCatchup, "withCatchup", false, "Starts with a catching-up step")
	flag.StringVar(&monitorPrivateKey, "key", "", "monitor private key")
	flag.StringVar(&operatorList, "operatorList", "", "list of operators")
	flag.StringVar(&currCovAddr, "currCovAddr", "", "current covenant address")
	flag.StringVar(&nodeGovAddr, "nodeGovAddr", "", "node governance contract address")
	flag.StringVar(&sideChainUrl, "sbchUrl", "http://localhost:8545", "side chain rpc url")
	flag.StringVar(&mainnetUrl, "mainnetUrl", "localhost:8332", "mainnet url")
	flag.StringVar(&mainnetUsername, "mainnetUsername", "", "mainnet url username")
	flag.StringVar(&mainnetPassword, "mainnetPassword", "", "mainnet url password")
	flag.StringVar(&dbPath, "dbPath", "", "the path for sqlite database")
	flag.Int64Var(&lastRescanHeight, "lastRescanHeight", 1, "last rescan mainnet height")
	flag.Int64Var(&lastRescanTime, "lastRescanTime", 0, "last rescan time")
	flag.Int64Var(&mainChainHeight, "mainChainHeight", 0, "main chain height to scan the first cc tx")
	flag.Parse()
}

func simpleRun() {
	parseFlags()
	rpcClient, err := rpc.Dial(sideChainUrl)
	if err != nil {
		panic(err)
	}
	ethClient, err := ethclient.Dial(sideChainUrl)
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
	fmt.Printf("monitor: %s\n", monitor.MyAddress.String())
	//defer client.Shutdown()
	monitor.SendStartRescanAndHandleUTXO(context.Background(), rpcClient, ethClient, client, lastRescanHeight, lastRescanTime, handleUtxoDelay)
	select {}
}
