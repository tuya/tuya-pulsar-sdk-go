package pulsar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tylog"
)

func TestMain(m *testing.M) {
	// tylog 包级 logger 默认 nil，未初始化时 Debug/Warn/Error 会 panic。
	// 用临时目录避免污染工作目录。
	dir, err := os.MkdirTemp("", "tuya-pulsar-sdk-test-")
	if err != nil {
		panic(err)
	}
	tylog.SetGlobalLog("client-test", false, tylog.WithDirOption(filepath.Join(dir, "logs")))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeMsgID and fakeMsg embed the interfaces; unused methods panic on a nil receiver.
// Tests only exercise ID/Payload, the two methods consumerV2 actually calls.
type fakeMsgID struct{ pulsar.MessageID }

func (fakeMsgID) LedgerID() int64     { return 1 }
func (fakeMsgID) EntryID() int64      { return 1 }
func (fakeMsgID) BatchIdx() int32     { return 0 }
func (fakeMsgID) PartitionIdx() int32 { return 0 }
func (fakeMsgID) String() string      { return "1:1:0:0" }

type fakeMsg struct{ pulsar.Message }

func (fakeMsg) ID() pulsar.MessageID { return fakeMsgID{} }
func (fakeMsg) Payload() []byte      { return []byte("test") }

type fakeConsumer struct {
	pulsar.Consumer
	receiveFn func(ctx context.Context) (pulsar.Message, error)
	ackCount  atomic.Int32
	nackCount atomic.Int32
	ackErr    error
	nackCh    chan struct{}
}

func (f *fakeConsumer) Receive(ctx context.Context) (pulsar.Message, error) {
	return f.receiveFn(ctx)
}

func (f *fakeConsumer) Ack(_ pulsar.Message) error {
	f.ackCount.Add(1)
	return f.ackErr
}

func (f *fakeConsumer) Nack(_ pulsar.Message) {
	f.nackCount.Add(1)
	if f.nackCh != nil {
		select {
		case f.nackCh <- struct{}{}:
		default:
		}
	}
}

func (f *fakeConsumer) Close() {}

type stubHandler struct {
	err error
}

// fakeCloser embeds pulsar.Client and only implements Close to verify ownership-release logic.
type fakeCloser struct {
	pulsar.Client
	closed bool
}

func (f *fakeCloser) Close() { f.closed = true }

func (s stubHandler) HandlePayload(_ context.Context, _ pulsar.Message, _ []byte) error {
	return s.err
}

// blockingReceive blocks until ctx is cancelled, simulating Receive when the broker has no new messages.
func blockingReceive(ctx context.Context) (pulsar.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// fakeResultErr implements pulsarResulter to simulate pulsar.Error result codes in tests.
type fakeResultErr struct{ result pulsar.Result }

func (e fakeResultErr) Result() pulsar.Result { return e.result }
func (e fakeResultErr) Error() string         { return "fake pulsar error" }

// onceThenBlock returns fakeMsg on the first call and blocks until ctx is cancelled after that;
// use it to deliver exactly one message to the pipeline.
func onceThenBlock() func(ctx context.Context) (pulsar.Message, error) {
	var sent atomic.Bool
	return func(ctx context.Context) (pulsar.Message, error) {
		if sent.CompareAndSwap(false, true) {
			return fakeMsg{}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func TestReceiveAndHandle_CtxCancelExitsQuickly(t *testing.T) {
	fc := &fakeConsumer{receiveFn: blockingReceive}
	c := consumerV2{consumer: fc, workerNum: 2, ackWorkerNum: 1, nackOnHandleError: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.ReceiveAndHandle(ctx, stubHandler{})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReceiveAndHandle did not return within 5s after ctx cancel")
	}
}

func TestReceiveAndHandle_HandlerErrorTriggersNack(t *testing.T) {
	nackCh := make(chan struct{}, 1)
	fc := &fakeConsumer{
		receiveFn: onceThenBlock(),
		nackCh:    nackCh,
	}
	c := consumerV2{consumer: fc, workerNum: 1, ackWorkerNum: 1, nackOnHandleError: true}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.ReceiveAndHandle(ctx, stubHandler{err: errors.New("boom")})
	}()

	select {
	case <-nackCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected Nack within 2s on handler error")
	}
	if got := fc.ackCount.Load(); got != 0 {
		t.Errorf("expected 0 Ack calls, got %d", got)
	}

	cancel()
	wg.Wait()
}

func TestReceiveAndHandle_LegacyAckOnHandleError(t *testing.T) {
	fc := &fakeConsumer{receiveFn: onceThenBlock()}
	// nackOnHandleError=false 保留旧 SDK 把 err 也 Ack 掉的 buggy 语义
	c := consumerV2{consumer: fc, workerNum: 1, ackWorkerNum: 1, nackOnHandleError: false}

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.ReceiveAndHandle(ctx, stubHandler{err: errors.New("boom")})
	}()

	// 等 Ack 真的被调用
	deadline := time.Now().Add(2 * time.Second)
	for fc.ackCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fc.ackCount.Load(); got == 0 {
		t.Fatal("expected at least 1 Ack call under legacy nackOnHandleError=false")
	}
	if got := fc.nackCount.Load(); got != 0 {
		t.Errorf("expected 0 Nack calls under legacy mode, got %d", got)
	}

	cancel()
	wg.Wait()
}

func TestAckOne_FailureFallsBackToNack(t *testing.T) {
	// isolate ackOne; avoids the 1.7s cumulative sleep from the real ackBackoffSteps;
	// covers the full-backoff-then-Nack semantic — enough to validate all branches.
	defer swapBackoff(&ackBackoffSteps,
		[]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond})()

	fc := &fakeConsumer{ackErr: errors.New("ack broken")}
	c := consumerV2{consumer: fc}
	c.ackOne(context.Background(), ackTask{msg: fakeMsg{}})

	if got := fc.ackCount.Load(); got != int32(len(ackBackoffSteps)) {
		t.Errorf("expected %d Ack attempts, got %d", len(ackBackoffSteps), got)
	}
	if got := fc.nackCount.Load(); got != 1 {
		t.Errorf("expected 1 Nack fallback, got %d", got)
	}
}

func TestAckOne_NackTaskSkipsAckRetry(t *testing.T) {
	fc := &fakeConsumer{ackErr: errors.New("would fail if called")}
	c := consumerV2{consumer: fc}
	c.ackOne(context.Background(), ackTask{msg: fakeMsg{}, nack: true})

	if got := fc.ackCount.Load(); got != 0 {
		t.Errorf("nack task should not call Ack, got %d Ack calls", got)
	}
	if got := fc.nackCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 Nack, got %d", got)
	}
}

// TestAckOne_CtxCancelInterruptsBackoff verifies that a ctx cancellation during backoff
// causes ackOne to Nack and exit immediately, not block until the full backoff sequence completes
// (a 1h backoff makes the difference obvious — ctx-aware exits in <1s, blocking would hang).
func TestAckOne_CtxCancelInterruptsBackoff(t *testing.T) {
	defer swapBackoff(&ackBackoffSteps,
		[]time.Duration{time.Hour, time.Hour, time.Hour})()

	fc := &fakeConsumer{ackErr: errors.New("ack broken")}
	c := consumerV2{consumer: fc}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately to simulate shutdown

	done := make(chan struct{})
	go func() {
		c.ackOne(ctx, ackTask{msg: fakeMsg{}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ackOne 未在退避期间响应 ctx 取消")
	}
	if got := fc.ackCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 Ack attempt before ctx cancel, got %d", got)
	}
	if got := fc.nackCount.Load(); got != 1 {
		t.Errorf("expected 1 Nack fallback after ctx cancel, got %d", got)
	}
}

// swapBackoff temporarily replaces the package-level backoff slice and returns a restore func.
func swapBackoff(target *[]time.Duration, replacement []time.Duration) func() {
	old := *target
	*target = replacement
	return func() { *target = old }
}

// TestAcquireClient_CachesWhenNoAuth verifies that the shared client is cached and reused when no auth is set.
// The old value-receiver implementation silently discarded the c.cli assignment, causing a new client
// to be created on every call (connection leak).
func TestAcquireClient_CachesWhenNoAuth(t *testing.T) {
	c := &clientImpl{clientCfg: ClientConfig{PulsarAddr: PulsarAddrCN}}
	a, owned, err := c.acquireClient(ConsumerConfig{Topic: "persistent://t/out/event"})
	if err != nil {
		t.Fatalf("acquireClient 第一次失败: %v", err)
	}
	if owned {
		t.Error("shared client should have owned=false")
	}
	b, _, err := c.acquireClient(ConsumerConfig{Topic: "persistent://t/out/event"})
	if err != nil {
		t.Fatalf("acquireClient 第二次失败: %v", err)
	}
	if a != b {
		t.Error("no-auth calls should reuse the same cached client")
	}
	a.Close()
}

// TestAcquireClient_AuthGetsDedicatedClient verifies that an auth consumer gets a dedicated client
// with owned=true and that the shared cache is left untouched (writing to it would leak the cached client).
func TestAcquireClient_AuthGetsDedicatedClient(t *testing.T) {
	c := &clientImpl{clientCfg: ClientConfig{PulsarAddr: PulsarAddrCN}}
	auth := NewAuthProvider("id", "key")
	a, owned, err := c.acquireClient(ConsumerConfig{Topic: "persistent://t/out/event", Auth: auth})
	if err != nil {
		t.Fatalf("acquireClient 第一次失败: %v", err)
	}
	if !owned {
		t.Error("dedicated client should have owned=true")
	}
	if c.cli != nil {
		t.Error("auth consumer must not populate the shared cache")
	}
	b, _, err := c.acquireClient(ConsumerConfig{Topic: "persistent://t/out/event", Auth: auth})
	if err != nil {
		t.Fatalf("acquireClient 第二次失败: %v", err)
	}
	if a == b {
		t.Error("auth consumers should each get a separate dedicated client")
	}
	a.Close()
	b.Close()
}

// TestConsumerClose_ClosesOwnedClient verifies Close releases a dedicated client and does not touch the shared client.
func TestConsumerClose_ClosesOwnedClient(t *testing.T) {
	// dedicated client: ownsClient=true → Close must also close the client
	owned := &fakeCloser{}
	co := consumerV2{consumer: &fakeConsumer{receiveFn: blockingReceive}, client: owned, ownsClient: true}
	co.Close()
	if !owned.closed {
		t.Error("ownsClient=true: Close should close the dedicated client")
	}

	// shared client: ownsClient=false → Close must NOT close the client
	shared := &fakeCloser{}
	cs := consumerV2{consumer: &fakeConsumer{receiveFn: blockingReceive}, client: shared, ownsClient: false}
	cs.Close()
	if shared.closed {
		t.Error("ownsClient=false: Close must not close the shared client")
	}
}

func TestGetTenant(t *testing.T) {
	cases := []struct {
		topic string
		want  string
	}{
		{"persistent://abc/out/event", "abc"},
		{"abc/out/event", "abc"},
		{"persistent://abc", "abc"}, // malformed: no "/" — return whole segment, not panic
		{"", ""},
	}
	for _, tc := range cases {
		if got := getTenant(tc.topic); got != tc.want {
			t.Errorf("getTenant(%q) = %q, want %q", tc.topic, got, tc.want)
		}
	}
}

func TestSubscriptionName(t *testing.T) {
	cases := []struct {
		name string
		cfg  ConsumerConfig
		want string
	}{
		{
			name: "explicit subscription",
			cfg:  ConsumerConfig{Topic: "persistent://abc/out/event", Subscription: "abc-a"},
			want: "abc-a",
		},
		{
			name: "derived subscription",
			cfg:  ConsumerConfig{Topic: "persistent://abc/out/event"},
			want: "abc-sub",
		},
		{
			name: "blank subscription falls back",
			cfg:  ConsumerConfig{Topic: "persistent://abc/out/event", Subscription: "  "},
			want: "abc-sub",
		},
	}
	for _, tc := range cases {
		if got := subscriptionName(tc.cfg); got != tc.want {
			t.Errorf("%s: subscriptionName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestConsumerClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ConsumerClosed", fakeResultErr{pulsar.ConsumerClosed}, true},
		{"AlreadyClosedError", fakeResultErr{pulsar.AlreadyClosedError}, true},
		{"wrapped ConsumerClosed", fmt.Errorf("recv: %w", fakeResultErr{pulsar.ConsumerClosed}), true},
		{"TimeoutError", fakeResultErr{pulsar.TimeoutError}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := consumerClosed(tc.err); got != tc.want {
			t.Errorf("%s: consumerClosed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestReceiveLoop_ExitsOnConsumerClosed verifies that receiveLoop exits immediately when the consumer
// is closed (without a ctx cancellation), rather than looping forever with backoff.
func TestReceiveLoop_ExitsOnConsumerClosed(t *testing.T) {
	// 用极长退避放大效果：若没识别 closed，会卡在退避里超时
	defer swapBackoff(&receiveBackoffSteps, []time.Duration{time.Hour})()

	fc := &fakeConsumer{
		receiveFn: func(context.Context) (pulsar.Message, error) {
			return nil, fakeResultErr{pulsar.ConsumerClosed}
		},
	}
	c := consumerV2{consumer: fc, workerNum: 1, ackWorkerNum: 1, nackOnHandleError: true}

	done := make(chan struct{})
	go func() {
		// ctx is never cancelled — exit must be triggered solely by ConsumerClosed
		c.ReceiveAndHandle(context.Background(), stubHandler{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReceiveAndHandle did not exit after consumer closed (suspected infinite backoff)")
	}
}

// TestReceiveLoop_NackOnCtxCancelBeforeDispatch verifies that a message already fetched from the broker
// but not yet dispatched to a handler is explicitly Nack'd on ctx cancellation instead of being silently dropped.
func TestReceiveLoop_NackOnCtxCancelBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := &fakeConsumer{
		receiveFn: func(context.Context) (pulsar.Message, error) {
			cancel() // cancel ctx at the same time the message is received
			return fakeMsg{}, nil
		},
	}
	c := consumerV2{consumer: fc, workerNum: 1, ackWorkerNum: 1, nackOnHandleError: true}

	// unbuffered channel with no reader: the send never completes, so select always takes ctx.Done()
	recvCh := make(chan pulsar.Message)
	c.receiveLoop(ctx, recvCh)

	if got := fc.nackCount.Load(); got != 1 {
		t.Errorf("expected 1 Nack before drop, got %d", got)
	}
}
