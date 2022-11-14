package monitor

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

var (
	OperatorUrl       *string
	OperatorTlsConfig *tls.Config
)

func SendSuspendToOperator(sig, ts string) error {
	_, err := HttpGet(OperatorTlsConfig, fmt.Sprintf("https://"+*OperatorUrl+"/suspend?sig=%s&ts=%s", sig, ts))
	return err
}

func HttpGet(tlsConfig *tls.Config, url string) ([]byte, error) {
	client := http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println(resp.Status)
		return nil, errors.New("response status not ok: " + resp.Status)
	}
	return ioutil.ReadAll(resp.Body)
}
