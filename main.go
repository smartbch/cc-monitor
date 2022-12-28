package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
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
	monitorPrivateKey string
	sideChainUrl      string
	mainnetUrl        string
	mainnetUsername   string
	mainnetPassword   string
	dbPath            string
	lastRescanHeight  int64
	lastRescanTime    int64
)

const (
	handleUtxoDelay int64 = 1 * 60
)

func main() {
	//monitor.DebugWatcher()
	run()
	//simpleRun()
}

func run() {
	parseFlags()
	//monitor.ReadPrivKey()
	monitor.LoadPrivKeyInHex("")
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
	for i, mon := range ccInfo.Monitors {
		fmt.Printf("Monitor %d %#v\n", i, mon)
	}

	//currCovenantAddr := hexutil.MustDecode("0x6Ad3f81523c87aa17f1dFA08271cF57b6277C98e")
	//lastCovenantAddr := hexutil.MustDecode("0x0000000000000000000000000000000000000000")
	currCovenantAddr := hexutil.MustDecode(ccInfo.CurrCovenantAddress)
	lastCovenantAddr := hexutil.MustDecode(ccInfo.LastCovenantAddress)
	fmt.Printf("CovenantAddr %s last %s\n", monitor.ScriptHashToAddr(currCovenantAddr), monitor.ScriptHashToAddr(lastCovenantAddr))

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
	info := monitor.MetaInfo{
		LastRescanTime:   -1,
		ScannedHeight:    int64(ccInfo.RescannedHeight),
		MainChainHeight:  1532624,
		SideChainHeight:  1,
		LastCovenantAddr: string(lastCovenantAddr[:]),
		CurrCovenantAddr: string(currCovenantAddr[:]),
	}
	monitor.MigrateSchema(db)
	monitor.InitMetaInfo(db, &info)
	bs := monitor.NewBlockScanner(bchClient, db, sideChainUrl)

	simpleRpcClient, err := sbch.NewSimpleRpcClient("0x4fE159925585EB891bf165d5ee7945bd871F3A7B",
		sideChainUrl, 10 * time.Second)
	if err != nil {
		panic(err)
	}
	opClients := make([]*opclient.Client, 8)
	opClients[0] = opclient.NewClient("https://3.1.26.210:8801", 10 * time.Second)
	opClients[1] = opclient.NewClient("https://3.1.26.210:8802", 10 * time.Second)
	opClients[2] = opclient.NewClient("https://3.1.26.210:8803", 10 * time.Second)
	opClients[3] = opclient.NewClient("https://3.1.26.210:8804", 10 * time.Second)
	opClients[4] = opclient.NewClient("https://3.1.26.210:8805", 10 * time.Second)
	opClients[5] = opclient.NewClient("https://3.1.26.210:8806", 10 * time.Second)
	opClients[6] = opclient.NewClient("https://3.1.26.210:8807", 10 * time.Second)
	opClients[7] = opclient.NewClient("https://3.1.26.210:8808", 10 * time.Second)

	watcher := monitor.NewOperatorsWatcher(simpleRpcClient, opClients)

	monitor.Catchup(bs)
	monitor.MainLoop(bs, sbchClient, watcher)
}

func parseFlags() {
	flag.StringVar(&monitorPrivateKey, "key", "", "monitor private key")
	flag.StringVar(&sideChainUrl, "sbchUrl", "http://localhost:8545", "side chain rpc url")
	flag.StringVar(&mainnetUrl, "mainnetUrl", "localhost:8332", "mainnet url")
	flag.StringVar(&mainnetUsername, "mainnetUsername", "", "mainnet url username")
	flag.StringVar(&mainnetPassword, "mainnetPassword", "", "mainnet url password")
	flag.StringVar(&dbPath, "dbPath", "", "the path for sqlite database")
	flag.Int64Var(&lastRescanHeight, "lastRescanHeight", 1, "last rescan mainnet height")
	flag.Int64Var(&lastRescanTime, "lastRescanTime", 0, "last rescan time")
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
