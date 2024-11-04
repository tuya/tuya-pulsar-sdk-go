package main

import (
	"context"
	"encoding/base64"
	"encoding/json"

	pulsar "github.com/tuya/tuya-pulsar-sdk-go"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tylog"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tyutils"
)

var MqEnvProd string = "event"
var MqEnvTest string ="event-test"

func main() {
	// SetInternalLogLevel(logrus.DebugLevel)
	tylog.SetGlobalLog("sdk", false)
	accessID := "accessID"
	accessKey := "accessKey"
	env := MqEnvProd
	topic := pulsar.TopicForAccessID(accessID，env)

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

	decryptModel := msg.Properties()["em"]
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
	decode, err := tyutils.Decrypt(de, []byte(h.AesSecret), decryptModel)
	tylog.Info("aes decode", tylog.ByteString("decode payload", decode))

	return nil
}
