package pulsar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tylog"
)

const (
	PulsarAddrCN  = "pulsar+ssl://mqe.tuyacn.com:7285"
	PulsarAddrEU  = "pulsar+ssl://mqe.tuyaeu.com:7285"
	PulsarAddrUS  = "pulsar+ssl://mqe.tuyaus.com:7285"
	PulsarAddrIND = "pulsar+ssl://mqe.tuyain.com:7285"
	PulsarAddrUE  = "pulsar+ssl://mqe-ueaz.tuyaus.com:7285"
	PulsarAddrWE  = "pulsar+ssl://mqe-weaz.tuyaeu.com:7285"
	PulsarAddrSG  = "pulsar+ssl://mqe-sg.iotbing.com:7285"

	defaultWorkerNum    = 32
	defaultAckWorkerNum = 4
)

var ackBackoffSteps = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}

// receiveBackoffSteps paces retries after a Receive error (broker blip / network hiccup)
// to prevent a tight CPU-spinning loop on bare continue.
var receiveBackoffSteps = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}

type Message = pulsar.Message

type Client interface {
	NewConsumer(config ConsumerConfig) (Consumer, error)
}

type ProducerMessage struct {
	Payload []byte
	Key     string
}

type Consumer interface {
	ReceiveAndHandle(ctx context.Context, handler PayloadHandlerV2)
	Close() error
}

type PayloadHandlerV2 interface {
	HandlePayload(ctx context.Context, msg Message, payload []byte) error
}

type clientImpl struct {
	mu        sync.Mutex
	cli       pulsar.Client
	clientCfg ClientConfig
}

type ClientConfig struct {
	PulsarAddr string
	// Deprecated: unused. Set authentication on ConsumerConfig.Auth instead.
	Auth interface{}
}

type ConsumerConfig struct {
	Topic string
	// Subscription overrides the default subscription name derived from the topic tenant ({tenant}-sub).
	Subscription string
	Auth         interface{}

	// WorkerNum controls the number of handler goroutines. 0 defaults to 32.
	// The old SDK hard-coded 10, capping single-instance throughput at 10/handle_latency.
	WorkerNum int

	// ReceiverQueueSize is forwarded to pulsar-client-go. 0 uses its default (1000/partition).
	ReceiverQueueSize int

	// AckWorkerNum controls the number of dedicated ack goroutines. 0 defaults to 4.
	// Handlers pass messages to the ack pool immediately and pull the next one without blocking.
	AckWorkerNum int

	// NackOnHandleError controls what happens when HandlePayload returns an error.
	//   nil (default) → Nack so the broker redelivers; avoids silent message loss.
	//   ptr to true   → Nack (same as default).
	//   ptr to false  → Ack and discard, preserving the old (buggy) SDK behavior.
	// *bool forces callers who want the old behavior to declare it explicitly,
	// preventing silent zero-value footguns. The old behavior is a bug, not a safe default.
	NackOnHandleError *bool
}

func NewClient(cfg ClientConfig) Client {
	return newClientV2(cfg)
}

func newClientV2(cfg ClientConfig) Client {
	return &clientImpl{clientCfg: cfg}
}

func (c *clientImpl) NewConsumer(config ConsumerConfig) (Consumer, error) {
	cli, owned, err := c.acquireClient(config)
	if err != nil {
		return nil, err
	}
	opts := pulsar.ConsumerOptions{
		Topic:            config.Topic,
		SubscriptionName: subscriptionName(config),
		Type:             pulsar.Failover,
	}
	if config.ReceiverQueueSize > 0 {
		opts.ReceiverQueueSize = config.ReceiverQueueSize
	}
	consumer, err := cli.Subscribe(opts)
	if err != nil {
		tylog.Error("create consumer failed", tylog.ErrorField(err), tylog.Any("config", config))
		// close dedicated client immediately if Subscribe fails to avoid a leak.
		if owned {
			cli.Close()
		}
		return nil, err
	}
	workerNum := config.WorkerNum
	if workerNum <= 0 {
		workerNum = defaultWorkerNum
	}
	ackWorkerNum := config.AckWorkerNum
	if ackWorkerNum <= 0 {
		ackWorkerNum = defaultAckWorkerNum
	}
	nackOnErr := true
	if config.NackOnHandleError != nil {
		nackOnErr = *config.NackOnHandleError
	}
	tylog.Info("create consumer success",
		tylog.Any("config", config),
		tylog.Any("workerNum", workerNum),
		tylog.Any("ackWorkerNum", ackWorkerNum),
		tylog.Any("nackOnHandleError", nackOnErr),
	)
	return consumerV2{
		consumer:          consumer,
		client:            cli,
		ownsClient:        owned,
		workerNum:         workerNum,
		ackWorkerNum:      ackWorkerNum,
		nackOnHandleError: nackOnErr,
	}, nil
}

// acquireClient returns the pulsar.Client to use for Subscribe and whether the caller owns it.
//   - config.Auth != nil: builds a dedicated client for this consumer (auth is a client-level option);
//     does NOT write to the shared cache to avoid overwriting and leaking the cached client.
//     owned=true — the consumer must close it.
//   - config.Auth == nil: returns the lazily-initialized shared client (process-level singleton).
//     owned=false — the consumer must NOT close it.
//
// Pointer receiver + lock: the old value-receiver implementation wrote `c.cli = client` to a copy,
// so the cache was never populated — every call created a new client (connection leak).
func (c *clientImpl) acquireClient(config ConsumerConfig) (cli pulsar.Client, owned bool, err error) {
	if config.Auth != nil {
		cli, err = c.newPulsarClient(config.Auth)
		return cli, true, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == nil {
		client, err := c.newPulsarClient(config.Auth)
		if err != nil {
			return nil, false, err
		}
		c.cli = client
	}
	return c.cli, false, nil
}

func (c *clientImpl) newPulsarClient(auth interface{}) (pulsar.Client, error) {
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		TLSAllowInsecureConnection: true,
		URL:                        c.clientCfg.PulsarAddr,
		Authentication:             auth,
	})
	if err != nil {
		tylog.Error("create pulsar client failed", tylog.ErrorField(err), tylog.Any("config", c.clientCfg))
		return nil, err
	}
	tylog.Info("create pulsar client success", tylog.Any("config", c.clientCfg))
	return client, nil
}

type consumerV2 struct {
	consumer pulsar.Consumer
	// client is the pulsar.Client used to Subscribe.
	// ownsClient=true means this is a dedicated client (created for an auth consumer);
	// it must be closed in Close(). false means it is the shared singleton and must NOT be closed.
	client            pulsar.Client
	ownsClient        bool
	workerNum         int
	ackWorkerNum      int
	nackOnHandleError bool
}

// ackTask is an element of ackCh. nack=true requests broker redelivery; false runs the Ack retry path.
type ackTask struct {
	msg  pulsar.Message
	nack bool
}

func (c consumerV2) ReceiveAndHandle(ctx context.Context, handler PayloadHandlerV2) {
	recvCh := make(chan pulsar.Message, c.workerNum*2)
	ackCh := make(chan ackTask, c.workerNum*2)

	var handlerWg sync.WaitGroup
	for i := 0; i < c.workerNum; i++ {
		handlerWg.Add(1)
		go func() {
			defer handlerWg.Done()
			for msg := range recvCh {
				c.handleOne(ctx, handler, msg, ackCh)
			}
		}()
	}

	var ackWg sync.WaitGroup
	for i := 0; i < c.ackWorkerNum; i++ {
		ackWg.Add(1)
		go func() {
			defer ackWg.Done()
			for task := range ackCh {
				c.ackOne(ctx, task)
			}
		}()
	}

	receiveDone := make(chan struct{})
	go func() {
		defer close(receiveDone)
		c.receiveLoop(ctx, recvCh)
	}()

	// Graceful shutdown order: receiveLoop exits → close recvCh → handlers drain → close ackCh → ack workers drain.
	<-receiveDone
	close(recvCh)
	handlerWg.Wait()
	close(ackCh)
	ackWg.Wait()
}

func (c consumerV2) receiveLoop(ctx context.Context, recvCh chan<- pulsar.Message) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := c.consumer.Receive(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			// consumer has been closed: Receive keeps returning ConsumerClosed.
			// Retrying with backoff would spin forever without making progress.
			if consumerClosed(err) {
				tylog.Info("consumer closed, receive loop exit")
				return
			}
			backoff := receiveBackoffSteps[min(attempt, len(receiveBackoffSteps)-1)]
			attempt++
			tylog.Warn("consumer receive failed", tylog.ErrorField(err), tylog.Any("backoff", backoff))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		attempt = 0
		select {
		case recvCh <- msg:
		case <-ctx.Done():
			// Message already fetched from the broker but not yet dispatched to a handler.
			// Explicit Nack triggers immediate redelivery instead of waiting for ack-timeout.
			c.consumer.Nack(msg)
			return
		}
	}
}

// pulsarResulter abstracts the error-code accessor on pulsar.Error, decoupling the check from the concrete type for testability.
type pulsarResulter interface{ Result() pulsar.Result }

// consumerClosed reports whether err indicates the consumer has been closed (ConsumerClosed or AlreadyClosedError).
func consumerClosed(err error) bool {
	var r pulsarResulter
	if !errors.As(err, &r) {
		return false
	}
	res := r.Result()
	return res == pulsar.ConsumerClosed || res == pulsar.AlreadyClosedError
}

func (c consumerV2) handleOne(ctx context.Context, handler PayloadHandlerV2, msg pulsar.Message, ackCh chan<- ackTask) {
	start := time.Now()
	id := MsgId(msg.ID())
	tylog.Debug("consume receive", tylog.Any("messageId", id))
	err := handler.HandlePayload(ctx, msg, msg.Payload())
	if err != nil {
		tylog.Warn("consumer HandlePayload failed", tylog.ErrorField(err), tylog.Any("consumer", c.consumer), tylog.Any("msg", msg))
	}
	tylog.Debug("consume handle finish", tylog.Any("messageId", id), tylog.Any("cost", time.Since(start)))

	// ackCh is always open here (close order guarantees handlers finish before ackCh is closed),
	// so send unconditionally — do not gate on ctx to ensure in-flight messages are ack'd on shutdown.
	ackCh <- ackTask{msg: msg, nack: err != nil && c.nackOnHandleError}
}

// ackOne processes one ack task: nack=true → Nack immediately for redelivery;
// otherwise retry Ack with backoff and fall back to Nack when all attempts are exhausted.
// ctx-aware backoff: on shutdown, Nack and exit immediately rather than sleeping.
// Invariant: every code path except a successful Ack ends with a Nack — no silent message drops.
func (c consumerV2) ackOne(ctx context.Context, task ackTask) {
	id := MsgId(task.msg.ID())
	if task.nack {
		tylog.Debug("nack on handle error", tylog.Any("messageId", id))
		c.consumer.Nack(task.msg)
		return
	}
	ackStart := time.Now()
	for i, backoff := range ackBackoffSteps {
		err := c.consumer.Ack(task.msg)
		if err == nil {
			tylog.Debug("consume ack finish", tylog.Any("messageId", id), tylog.Any("cost", time.Since(ackStart)))
			return
		}
		tylog.Warn("ack failed", tylog.ErrorField(err), tylog.Any("messageId", id), tylog.Any("attempt", i+1))
		// Skip backoff sleep on the last attempt — fall through to the Nack below.
		if i == len(ackBackoffSteps)-1 {
			break
		}
		select {
		case <-ctx.Done():
			tylog.Debug("ack backoff interrupted by ctx, nack to redeliver", tylog.Any("messageId", id))
			c.consumer.Nack(task.msg)
			return
		case <-time.After(backoff):
		}
	}
	tylog.Warn("ack giving up, nack to redeliver", tylog.Any("messageId", id))
	c.consumer.Nack(task.msg)
}

func MsgId(id pulsar.MessageID) string {
	return fmt.Sprintf("%d:%d:%d:%d", id.LedgerID(), id.EntryID(), id.PartitionIdx(), id.BatchIdx())
}

func (c consumerV2) Close() error {
	c.consumer.Close()
	// Dedicated clients (created for auth consumers) must be closed here;
	// otherwise their TCP connections and background goroutines leak.
	// The shared singleton must NOT be closed.
	if c.ownsClient && c.client != nil {
		c.client.Close()
	}
	return nil
}

func TopicForAccessID(accessID string) string {
	topic := fmt.Sprintf("persistent://%s/out/event", accessID)
	return topic
}

func subscriptionName(config ConsumerConfig) string {
	if strings.TrimSpace(config.Subscription) != "" {
		return config.Subscription
	}
	return getTenant(config.Topic) + "-sub"
}

func getTenant(topic string) string {
	topic = strings.TrimPrefix(topic, "persistent://")
	end := strings.Index(topic, "/")
	if end < 0 {
		// Malformed topic (no "/"): return the whole string to avoid an out-of-bounds panic.
		return topic
	}
	return topic[:end]
}
