package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"time"

	goecies "github.com/ecies/go"
	"github.com/ethereum/go-ethereum"
	gethacc "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	ccabi "github.com/smartbch/smartbch/crosschain/abi"
)

var (
	MyPrivKey *ecdsa.PrivateKey
	MyAddress common.Address

	CCAddress = common.HexToAddress("0x0000000000000000000000000000000000002714")
)

func readPrivKey() {
	eciesPrivKey, err := goecies.GenerateKey()
	if err != nil {
		panic(err)
	}
	fmt.Printf("The Ecies Pubkey: %s\n", hex.EncodeToString(eciesPrivKey.PublicKey.Bytes(true)))
	var inputHex string
	fmt.Print("Enter the encrypted private key: ")
	fmt.Scanf("%s", &inputHex)
	bz, err := hex.DecodeString(inputHex)
	if err == nil {
		MyPrivKey, err = crypto.ToECDSA(bz)
	}
	if err != nil {
		fmt.Print("Cannot decode hex string\n")
		panic(err)
	}
	MyAddress = crypto.PubkeyToAddress(MyPrivKey.PublicKey)
}

func sendPauseTransaction(ctx context.Context, client *ethclient.Client) error {
	callData := ccabi.PackPauseFunc()
	return sendTransaction(ctx, client, MyAddress, CCAddress, callData)
}

func sendResumeTransaction(ctx context.Context, client *ethclient.Client) error {
	callData := ccabi.PackResumeFunc()
	return sendTransaction(ctx, client, MyAddress, CCAddress, callData)
}

func sendRescanTransaction(ctx context.Context, client *ethclient.Client, height int64) error {
	callData := ccabi.PackStartRescanFunc(big.NewInt(height))
	return sendTransaction(ctx, client, MyAddress, CCAddress, callData)
}

func sendHandleUtxoTransaction(ctx context.Context, client *ethclient.Client) error {
	callData := ccabi.PackHandleUTXOsFunc()
	return sendTransaction(ctx, client, MyAddress, CCAddress, callData)
}

func sendPauseOperator() error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hash, _ := gethacc.TextAndHash([]byte(ts))
	sig, err := crypto.Sign(hash, MyPrivKey)
	if err != nil {
		panic(err)
	}
	return SendSuspendToOperator(hex.EncodeToString(sig), ts)
}

func sendTransaction(ctx context.Context, client *ethclient.Client, from, to common.Address, callData []byte) error {
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return err
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		To:   &to,
		Data: callData,
	})
	if err != nil {
		return err
	}
	value := big.NewInt(0)
	tx := types.NewTransaction(nonce, CCAddress, value, gasLimit, gasPrice, callData)
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return err
	}
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), MyPrivKey)
	if err != nil {
		return err
	}
	return client.SendTransaction(ctx, signedTx)
}
