package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"

	goecies "github.com/ecies/go"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/smartbch/smartbch/crosschain"
)

var (
	PrivKey   *ecdsa.PrivateKey
	MyAddress common.Address

	CCAddress = common.HexToAddress("0x4592d8f8d7b001e72cb26a73e4fa1806a51ac79d")
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
		PrivKey, err = crypto.ToECDSA(bz)
	}
	if err != nil {
		fmt.Print("Cannot decode hex string\n")
		panic(err)
	}
	MyAddress = crypto.PubkeyToAddress(PrivKey.PublicKey)
}

func PackStartRescanFunc(mainFinalizedBlockHeight *big.Int) []byte {
	return crosschain.ABI.MustPack("startRescan", mainFinalizedBlockHeight)
}

func sendPauseTransaction(ctx context.Context, client *ethclient.Client) error {
	callData := crosschain.PackPauseFunc()
	return sendTransaction(ctx, client, callData)
}

func sendRescanTransaction(ctx context.Context, client *ethclient.Client, height int64) error {
	callData := crosschain.PackStartRescanFunc(big.NewInt(height))
	return sendTransaction(ctx, client, callData)
}

func sendTransaction(ctx context.Context, client *ethclient.Client, callData []byte) error {
	nonce, err := client.PendingNonceAt(ctx, MyAddress)
	if err != nil {
		return err
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}

	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		To:   &CCAddress,
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

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), PrivKey)
	if err != nil {
		return err
	}

	return client.SendTransaction(ctx, signedTx)
}
