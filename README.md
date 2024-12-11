# pulsar-client-go

[English](README.md) | [中文版](README_cn.md)

## Introduction

Tuya pulsar client SDK for Golang

## Preparation

1. AccessID: Provided by Tuya platform.
2. AccessKey: provided by Tuya platform.
3. Pulsar address: Select the pulsar address according to different business areas. You can find out the address from documents.

## Example

```
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"

	pulsar "github.com/tuya/tuya-pulsar-sdk-go"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tylog"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tyutils"
)

func main() {
	// SetInternalLogLevel(logrus.DebugLevel)
	tylog.SetGlobalLog("sdk", false)
	accessID := "accessID"
	accessKey := "accessKey"
	topic := pulsar.TopicForAccessID(accessID)

	// create client
	cfg := pulsar.ClientConfig{
		PulsarAddr: pulsar.PulsarAddrCN,
	}
	c := pulsar.NewClient(cfg)

	// create consumer
	csmCfg := pulsar.ConsumerConfig{
		Topic: topic,
		Auth:  pulsar.NewAuthProvider(accessID, accessKey),
	}
	csm, _ := c.NewConsumer(csmCfg)

	// handle message
	csm.ReceiveAndHandle(context.Background(), &helloHandler{AesSecret: accessKey[8:24]})
}

type helloHandler struct {
	AesSecret string
}

func (h *helloHandler) HandlePayload(ctx context.Context, msg pulsar.Message, payload []byte) error {
	tylog.Info("payload preview", tylog.String("payload", string(payload)))

	// let's decode the payload with AES
	m := map[string]interface{}{}
	err := json.Unmarshal(payload, &m)
	if err != nil {
		tylog.Error("json unmarshal failed", tylog.ErrorField(err))
		return nil
	}
	bs := m["data"].(string)
	de, err := base64.StdEncoding.DecodeString(string(bs))
	if err != nil {
		tylog.Error("base64 decode failed", tylog.ErrorField(err))
		return nil
	}
	decode := tyutils.EcbDecrypt(de, []byte(h.AesSecret))
	tylog.Info("aes decode", tylog.ByteString("decode payload", decode))

	return nil
}



```

## Precautions

1. Make sure that the accessID and accessKey are correct.
2. Make sure that the Pulsar address is correct, For example `pulsar+ssl://mqe.tuyaus.com:7285`.
3. Make sure that the SDK code version you use is the latest.

## About debug

Through the following code, you can see all communications with the pulsar service in the terminal.

```
func main(){
	pulsar.SetInternalLogLevel(logrus.DebugLevel)
	// other code
}
```

Through the following code, you can see the log information of `tuya_pulsar_go_sdk`.
At the same time, the log will be saved in the `logs/sdk.log` file.
```
func main(){
	tylog.SetGlobalLog("sdk", false)
}
```

In a formal environment, you may not want the SDK logs to be output to the terminal. It is recommended that you use the following code to output the log to a file.
```
func main(){
	tylog.SetGlobalLog("sdk", true)
}
```
## Support

You can get support from Tuya with the following methods:

- Tuya Smart Help Center: [https://support.tuya.com/en/help](https://support.tuya.com/en/help)
- Technical Support Council: [https://iot.tuya.com/council](https://iot.tuya.com/council)

