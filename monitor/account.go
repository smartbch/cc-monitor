package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	goecies "github.com/ecies/go"
	"github.com/ethereum/go-ethereum"
	gethacc "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	ccabi "github.com/smartbch/smartbch/crosschain/abi"
	sbchrpcclient "github.com/smartbch/smartbch/rpc/client"
)

const RetryThreshold = 10

var (
	MyPrivKey *ecdsa.PrivateKey
	MyAddress common.Address

	CCAddress = common.HexToAddress("0x0000000000000000000000000000000000002714")
)

// Used by the person who keeps the monitor's private key
func EncryptPrivKey() {
	var inputHex string
	fmt.Print("Enter the private key: ")
	fmt.Scanf("%s", &inputHex)
	privKeyBz, err := hex.DecodeString(inputHex)
	if err != nil {
		fmt.Print("Cannot decode hex string\n")
		panic(err)
	}
	fmt.Print("Enter the Ecies Pubkey: ")
	fmt.Scanf("%s", &inputHex)
	pubkey, err := goecies.NewPublicKeyFromHex(inputHex)
	if err != nil {
		fmt.Print("Cannot decode ecies pubkey\n")
		panic(err)
	}
	out, err := goecies.Encrypt(pubkey, privKeyBz)
	if err != nil {
		fmt.Print("Cannot encrypt pubkey\n")
		panic(err)
	}

	fmt.Printf("The Encrypted Private Key: %s", hex.EncodeToString(out))
}

func ReadPrivKey() {
	eciesPrivKey, err := goecies.GenerateKey()
	if err != nil {
		panic(err)
	}
	fmt.Printf("The Ecies Pubkey: %s\n", hex.EncodeToString(eciesPrivKey.PublicKey.Bytes(true)))
	var inputHex string
	fmt.Print("Enter the encrypted private key: ")
	fmt.Scanf("%s", &inputHex)
	bz, err := hex.DecodeString(inputHex)
	if err != nil {
		fmt.Print("Cannot decode hex string\n")
		panic(err)
	}
	bz, err = goecies.Decrypt(eciesPrivKey, bz)
	if err != nil {
		fmt.Print("Cannot decrypt\n")
		panic(err)
	}
	MyPrivKey, err = crypto.ToECDSA(bz)
	if err != nil {
		panic(err)
	}
	MyAddress = crypto.PubkeyToAddress(MyPrivKey.PublicKey)
	fmt.Printf("MyAddress %s\n", MyAddress)
}

func LoadPrivKeyInHex(inputHex string) {
	bz, err := hex.DecodeString(inputHex)
	if err != nil {
		panic(err)
	}
	MyPrivKey, err = crypto.ToECDSA(bz)
	if err != nil {
		panic(err)
	}
	MyAddress = crypto.PubkeyToAddress(MyPrivKey.PublicKey)
	fmt.Printf("MyAddress %s\n", MyAddress)
}

func sendPauseTransaction(ctx context.Context, rpcclient *rpc.Client, client *ethclient.Client) error {
	errCount := 0
	callData := ccabi.PackPauseFunc()
	for errCount < RetryThreshold {
		txHash, err := sendTransaction(ctx, rpcclient, client, MyAddress, CCAddress, callData)
		if err != nil {
			fmt.Printf("Error in sendPauseTransaction: %s\n", err.Error())
			errCount++
			continue
		}
		time.Sleep(12 * time.Second)
		err = checkTxStatus(ctx, client, txHash)
		if err != nil {
			fmt.Printf("Error in sendPauseTransaction-checkTxStatus: %s\n", err.Error())
			errCount++
			continue
		} else {
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func sendResumeTransaction(ctx context.Context, rpcclient *rpc.Client, client *ethclient.Client) error {
	errCount := 0
	callData := ccabi.PackResumeFunc()
	for errCount < RetryThreshold {
		txHash, err := sendTransaction(ctx, rpcclient, client, MyAddress, CCAddress, callData)
		if err != nil {
			fmt.Printf("Error in sendResumeTransaction: %s\n", err.Error())
			errCount++
			continue
		}
		time.Sleep(12 * time.Second)
		err = checkTxStatus(ctx, client, txHash)
		if err != nil {
			fmt.Printf("Error in sendResumeTransaction-checkTxStatus: %s\n", err.Error())
			errCount++
			continue
		} else {
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func sendStartRescanTransaction(ctx context.Context, rpcclient *rpc.Client, client *ethclient.Client, sbchClient *sbchrpcclient.Client,
	lastRescanHeight, rescanHeight int64) error {
	fmt.Printf("sendStartRescanTransaction lastRescanHeight %d rescanHeight %d \n", lastRescanHeight, rescanHeight)
	errCount := 0
	callData := ccabi.PackStartRescanFunc(big.NewInt(rescanHeight))
	for errCount < RetryThreshold {
		_, err := sendTransaction(ctx, rpcclient, client, MyAddress, CCAddress, callData)
		if err != nil {
			fmt.Printf("Error in sendStartRescanTransaction: %s\n", err.Error())
			errCount++
			continue
		}
		time.Sleep(12 * time.Second)
		ccInfo, err := sbchClient.CcInfo(ctx)
		if err != nil {
			fmt.Printf("Error in sendStartRescanTransaction-CcInfo: %s\n", err.Error())
			errCount++
			continue
		}
		fmt.Printf("sendStartRescanTransaction-CcInfo: %#v\n", ccInfo)
		if int64(ccInfo.RescannedHeight) == rescanHeight {
			fmt.Printf("The new rescanHeight=%d, we can break\n", rescanHeight)
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func sendHandleUtxoTransaction(ctx context.Context, rpcclient *rpc.Client, client *ethclient.Client, sbchClient *sbchrpcclient.Client) error {
	errCount := 0
	callData := ccabi.PackHandleUTXOsFunc()
	for errCount < RetryThreshold {
		_, err := sendTransaction(ctx, rpcclient, client, MyAddress, CCAddress, callData)
		if err != nil {
			fmt.Printf("Error in sendHandleUtxoTransaction: %s\n", err.Error())
			errCount++
			continue
		}
		time.Sleep(12 * time.Second)
		ccInfo, err := sbchClient.CcInfo(ctx)
		if err != nil {
			fmt.Printf("Error in sendHandleUtxoTransaction-CcInfo: %s\n", err.Error())
			errCount++
			continue
		}
		if ccInfo.UTXOAlreadyHandled {
			fmt.Printf("UTXOAlreadyHandled, we can break\n")
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func toCallArg(msg ethereum.CallMsg) interface{} {
	arg := map[string]interface{}{
		"from": msg.From,
		"to":   msg.To,
	}
	if len(msg.Data) > 0 {
		arg["data"] = hexutil.Bytes(msg.Data)
	}
	if msg.Value != nil {
		arg["value"] = (*hexutil.Big)(msg.Value)
	}
	if msg.Gas != 0 {
		arg["gas"] = hexutil.Uint64(msg.Gas)
	}
	if msg.GasPrice != nil {
		arg["gasPrice"] = (*hexutil.Big)(msg.GasPrice)
	}
	return arg
}

func callContractForDebug(ctx context.Context, client *rpc.Client, msg ethereum.CallMsg) {
	var res map[string]any
	err := client.CallContext(ctx, &res, "sbch_call", toCallArg(msg), "latest")
	fmt.Printf("callContractForDebug %#v\n", res)
	if returnData, ok := res["returnData"].(string); ok {
		fmt.Printf("sbch_call %s %v\n", string(hexutil.MustDecode(returnData)), err)
	}
}

func sendTransaction(ctx context.Context, rpcclient *rpc.Client, client *ethclient.Client, from, to common.Address, callData []byte) (common.Hash, error) {
	nonce, err := client.PendingNonceAt(ctx, from)
	fmt.Printf("sendTransaction nonce %d\n", nonce)
	if err != nil {
		fmt.Printf("error in PendingNonceAt %v\n", err)
		return common.Hash{}, err
	}
	//gasPrice, err := client.SuggestGasPrice(ctx)
	//if err != nil {
	//	return common.Hash{}, err
	//}
	gasPrice := big.NewInt(10_000_000_000)
	callMsg := ethereum.CallMsg{
		From: MyAddress,
		To:   &to,
		Data: callData,
	}
	gasLimit, err := client.EstimateGas(ctx, callMsg)
	if err != nil {
		fmt.Printf("error in EstimateGas %v\n", err)
		callContractForDebug(ctx, rpcclient, callMsg)
		return common.Hash{}, err
	}
	fmt.Printf("gasPrice:%s,gasLimit:%d\n", gasPrice.String(), gasLimit)
	value := big.NewInt(0)
	tx := types.NewTransaction(nonce, CCAddress, value, gasLimit, gasPrice, callData)
	chainID, err := client.NetworkID(ctx)
	fmt.Printf("chain id: %#v\n", chainID)
	if err != nil {
		return common.Hash{}, err
	}
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), MyPrivKey)
	if err != nil {
		return common.Hash{}, err
	}
	fmt.Printf("signed tx %#v\n", signedTx)
	return signedTx.Hash(), client.SendTransaction(ctx, signedTx)
}

func checkTxStatus(ctx context.Context, client *ethclient.Client, txHash common.Hash) error {
	tx, err := client.TransactionReceipt(ctx, txHash)
	if err != nil {
		return err
	}
	if tx.Status != uint64(1) {
		return errors.New("tx failed: " + txHash.String())
	}
	return nil
}

func getSigAndTimestamp(pubkeyHex string) (sig string, ts int64) {
	ts = time.Now().Unix()
	msg := fmt.Sprintf("%s,%d", strings.Trim(pubkeyHex, "\""), ts)
	hash := gethacc.TextHash([]byte(msg))
	signature, err := crypto.Sign(hash, MyPrivKey)
	if err != nil {
		panic(err)
	}
	sig = hexutil.Encode(signature)
	return
}
