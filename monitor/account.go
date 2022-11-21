package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	goecies "github.com/ecies/go"
	"github.com/ethereum/go-ethereum"
	gethacc "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
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
func encryptPrivKey() {
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

	fmt.Printf("The Encrypted Pubkey: %s", hex.EncodeToString(out))
}

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
	errCount := 0
	callData := ccabi.PackPauseFunc()
	for errCount < RetryThreshold {
		txHash, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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

func sendResumeTransaction(ctx context.Context, client *ethclient.Client) error {
	errCount := 0
	callData := ccabi.PackResumeFunc()
	for errCount < RetryThreshold {
		txHash, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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

func sendStartRescanTransaction(ctx context.Context, client *ethclient.Client, sbchClient *sbchrpcclient.Client,
	lastRescanHeight, rescanHeight int64) error {
	errCount := 0
	callData := ccabi.PackStartRescanFunc(big.NewInt(rescanHeight))
	for errCount < RetryThreshold {
		_, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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
		if int64(ccInfo.LastRescannedHeight) != lastRescanHeight {
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func sendHandleUtxoTransaction(ctx context.Context, client *ethclient.Client, sbchClient *sbchrpcclient.Client) error {
	errCount := 0
	callData := ccabi.PackHandleUTXOsFunc()
	for errCount < RetryThreshold {
		_, err := sendTransaction(ctx, client, MyAddress, CCAddress, callData)
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
		if ccInfo.UTXOAlreadyHandled {
			break
		}
	}
	if errCount >= RetryThreshold {
		return errors.New("sendPauseTransaction reaches retry threshold")
	}
	return nil
}

func sendPauseOperator() error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hash := gethacc.TextHash([]byte(ts))
	sig, err := crypto.Sign(hash, MyPrivKey)
	if err != nil {
		panic(err)
	}
	return SendSuspendToOperator(hex.EncodeToString(sig), ts)
}

func sendTransaction(ctx context.Context, client *ethclient.Client, from, to common.Address, callData []byte) (common.Hash, error) {
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, err
	}
	//gasPrice, err := client.SuggestGasPrice(ctx)
	//if err != nil {
	//	return common.Hash{}, err
	//}
	gasPrice := big.NewInt(10_000_000_000)
	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		To:   &to,
		Data: callData,
	})
	if err != nil {
		return common.Hash{}, err
	}
	fmt.Printf("gasPrice:%s,gasLimit:%d\n", gasPrice.String(), gasLimit)
	value := big.NewInt(0)
	tx := types.NewTransaction(nonce, CCAddress, value, gasLimit, gasPrice, callData)
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), MyPrivKey)
	if err != nil {
		return common.Hash{}, err
	}
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

func getSigAndTimestamp() (sig string, ts int64) {
	ts = time.Now().Unix()
	hash := gethacc.TextHash([]byte(fmt.Sprintf("%d", ts)))
	signature, err := crypto.Sign(hash, MyPrivKey)
	if err != nil {
		panic(err)
	}
	sig = hexutil.Encode(signature)
	return
}

