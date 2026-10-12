package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backfillStream is a BackfillSubscriptionDataSource whose live events are pushed by the test.
type backfillStream struct {
	mu        sync.Mutex
	updater   SubscriptionUpdater
	started   chan struct{}
	release   chan struct{} // Start blocks until closed, if set
	startedAt atomic.Int64
	backfill  func(ctx StartupHookContext, input []byte) (func([]byte) bool, error)
}

func newBackfillStream(backfill func(ctx StartupHookContext, input []byte) (func([]byte) bool, error)) *backfillStream {
	return &backfillStream{started: make(chan struct{}), backfill: backfill}
}

func (b *backfillStream) HashTriggerInput(input []byte, xxh *xxhash.Digest) error {
	_, err := xxh.Write(input)
	return err
}

func (b *backfillStream) Start(ctx *Context, _ http.Header, _ []byte, updater SubscriptionUpdater) error {
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	b.updater = updater
	b.mu.Unlock()
	b.startedAt.Store(time.Now().UnixNano())
	close(b.started)
	go func() {
		<-ctx.ctx.Done()
		updater.Done()
	}()
	return nil
}

func (b *backfillStream) SubscriptionBackfill(ctx StartupHookContext, input []byte) (func([]byte) bool, error) {
	return b.backfill(ctx, input)
}

// live publishes a live event to every subscription of the trigger.
func (b *backfillStream) live(counter int) {
	b.mu.Lock()
	updater := b.updater
	b.mu.Unlock()
	updater.Update(counterEvent(counter))
}

func (b *backfillStream) complete() {
	b.mu.Lock()
	updater := b.updater
	b.mu.Unlock()
	updater.Complete()
	updater.Done()
}

func counterEvent(counter int) []byte {
	return fmt.Appendf(nil, `{"data":{"counter":%d}}`, counter)
}

// skipUpTo returns a skip function dropping held events with a counter <= last.
func skipUpTo(last int) func([]byte) bool {
	return func(data []byte) bool {
		var counter int
		_, err := fmt.Sscanf(string(data), `{"data":{"counter":%d}}`, &counter)
		return err == nil && counter <= last
	}
}

func backfillTestPlan(source SubscriptionDataSource) *GraphQLSubscription {
	fetches := Sequence()
	fetches.Trigger = &FetchTreeNode{
		Kind: FetchTreeNodeKindTrigger,
		Item: &FetchItem{
			Fetch: &SingleFetch{
				Info: &FetchInfo{DataSourceID: "0", DataSourceName: "counter"},
			},
			ResponsePath: "counter",
		},
	}
	return &GraphQLSubscription{
		Trigger: GraphQLSubscriptionTrigger{
			Source: source,
			InputTemplate: InputTemplate{
				Segments: []TemplateSegment{{SegmentType: StaticSegmentType, Data: []byte(`{"channel":"counter"}`)}},
			},
			PostProcessing: PostProcessingConfiguration{
				SelectResponseDataPath:   []string{"data"},
				SelectResponseErrorsPath: []string{"errors"},
			},
		},
		Response: &GraphQLResponse{
			Data: &Object{
				Fields: []*Field{{
					Name:  []byte("counter"),
					Value: &Integer{Path: []string{"counter"}},
					Info: &FieldInfo{
						Name:                "counter",
						ExactParentTypeName: "Subscription",
						Source:              TypeFieldSource{IDs: []string{"0"}, Names: []string{"counter"}},
					},
				}},
			},
			Fetches: fetches,
		},
	}
}

func newRecorder() *SubscriptionRecorder {
	return &SubscriptionRecorder{buf: &bytes.Buffer{}, messages: []string{}}
}

func counterMessages(counters ...int) []string {
	out := make([]string, 0, len(counters))
	for _, c := range counters {
		out = append(out, string(counterEvent(c)))
	}
	return out
}

func TestResolver_SubscriptionBackfill(t *testing.T) {
	const timeout = 10 * time.Second

	t.Run("new trigger starts the source before backfill and flushes held live events without duplicates", func(t *testing.T) {
		inBackfill := make(chan struct{})
		liveSent := make(chan struct{})
		var stream *backfillStream
		stream = newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
			select {
			case <-stream.started:
			default:
				return nil, errors.New("backfill ran before Source.Start")
			}
			ctx.Updater(counterEvent(1))
			close(inBackfill)
			<-liveSent
			ctx.Updater(counterEvent(2))
			return skipUpTo(2), nil
		})

		resolver := newResolver(t.Context())
		recorder := newRecorder()
		id := SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), backfillTestPlan(stream), recorder, id))

		<-inBackfill
		// Event 2 was published after the backfill read started, so it is both
		// replayed and received live. Event 3 is only received live.
		stream.live(2)
		stream.live(3)
		close(liveSent)

		recorder.AwaitMessages(t, 3, timeout)
		stream.live(4)
		recorder.AwaitMessages(t, 4, timeout)
		assert.Equal(t, counterMessages(1, 2, 3, 4), recorder.Messages())

		stream.complete()
		recorder.AwaitComplete(t, timeout)
	})

	t.Run("existing trigger keeps delivering live events to other subscribers during backfill", func(t *testing.T) {
		inBackfill := make(chan struct{})
		liveSent := make(chan struct{})
		var calls atomic.Int32
		stream := newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
			if calls.Add(1) == 1 {
				// The first subscriber has nothing to replay.
				return nil, nil
			}
			ctx.Updater(counterEvent(1))
			close(inBackfill)
			<-liveSent
			return skipUpTo(1), nil
		})

		resolver := newResolver(t.Context())
		plan := backfillTestPlan(stream)

		first := newRecorder()
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), plan, first, SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}))
		<-stream.started
		require.Eventually(t, func() bool { return calls.Load() == 1 }, timeout, time.Millisecond)

		second := newRecorder()
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), plan, second, SubscriptionIdentifier{ConnectionID: 2, SubscriptionID: 1}))

		<-inBackfill
		stream.live(1)
		stream.live(2)
		// The first subscriber is not backfilling and gets live events right away.
		first.AwaitMessages(t, 2, timeout)
		assert.Equal(t, counterMessages(1), second.Messages())
		close(liveSent)

		second.AwaitMessages(t, 2, timeout)
		assert.Equal(t, counterMessages(1, 2), first.Messages())
		assert.Equal(t, counterMessages(1, 2), second.Messages())

		stream.complete()
		first.AwaitComplete(t, timeout)
		second.AwaitComplete(t, timeout)
	})

	t.Run("existing trigger waits for the source to start before backfill", func(t *testing.T) {
		var backfillAt atomic.Int64
		stream := newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
			backfillAt.CompareAndSwap(0, time.Now().UnixNano())
			return nil, nil
		})
		stream.release = make(chan struct{})

		resolver := newResolver(t.Context())
		plan := backfillTestPlan(stream)
		first := newRecorder()
		second := newRecorder()
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), plan, first, SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}))
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), plan, second, SubscriptionIdentifier{ConnectionID: 2, SubscriptionID: 1}))

		time.Sleep(50 * time.Millisecond)
		assert.Zero(t, backfillAt.Load(), "backfill must not run before Source.Start returns")
		close(stream.release)
		<-stream.started

		require.Eventually(t, func() bool { return backfillAt.Load() != 0 }, timeout, time.Millisecond)
		assert.GreaterOrEqual(t, backfillAt.Load(), stream.startedAt.Load())

		stream.complete()
		first.AwaitComplete(t, timeout)
		second.AwaitComplete(t, timeout)
	})

	t.Run("overflowing the hold buffer closes the subscription with a resync error", func(t *testing.T) {
		inBackfill := make(chan struct{})
		liveSent := make(chan struct{})
		stream := newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
			close(inBackfill)
			<-liveSent
			return nil, nil
		})

		resolver := newResolver(t.Context())
		resolver.options.MaxSubscriptionBackfillBuffer = 2
		recorder := newRecorder()
		id := SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), backfillTestPlan(stream), recorder, id))

		<-inBackfill
		stream.live(1)
		stream.live(2)
		stream.live(3)
		close(liveSent)

		recorder.AwaitMessages(t, 1, timeout)
		assert.Equal(t, []string{fmt.Sprintf(`{"errors":[{"message":"%s"}],"data":null}`, ErrSubscriptionBackfillOverflow)}, recorder.Messages())
	})

	t.Run("backfill error closes the subscription", func(t *testing.T) {
		stream := newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
			return nil, errors.New("cursor expired")
		})

		resolver := newResolver(t.Context())
		recorder := newRecorder()
		id := SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}
		require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), backfillTestPlan(stream), recorder, id))

		recorder.AwaitMessages(t, 1, timeout)
		assert.Equal(t, []string{`{"errors":[{"message":"cursor expired"}],"data":null}`}, recorder.Messages())
	})
}

func TestBackfillBuffer(t *testing.T) {
	t.Run("nil buffer never holds", func(t *testing.T) {
		var b *backfillBuffer
		assert.False(t, b.hold([]byte("x")))
		held, overflowed := b.release(nil)
		assert.Nil(t, held)
		assert.False(t, overflowed)
	})

	t.Run("holds copies until released", func(t *testing.T) {
		b := newBackfillBuffer(10)
		data := []byte("a")
		assert.True(t, b.hold(data))
		data[0] = 'b'
		held, overflowed := b.release(nil)
		assert.Equal(t, [][]byte{[]byte("a")}, held)
		assert.False(t, overflowed)
		assert.False(t, b.hold([]byte("c")))
	})

	t.Run("drops replayed events after release", func(t *testing.T) {
		b := newBackfillBuffer(10)
		held, _ := b.release(func(data []byte) bool { return string(data) == "replayed" })
		assert.Empty(t, held)
		assert.True(t, b.hold([]byte("replayed")))
		assert.False(t, b.hold([]byte("new")))
	})

	t.Run("drops everything after the limit", func(t *testing.T) {
		b := newBackfillBuffer(1)
		assert.True(t, b.hold([]byte("a")))
		assert.True(t, b.hold([]byte("b")))
		assert.True(t, b.hold([]byte("c")))
		held, overflowed := b.release(nil)
		assert.Nil(t, held)
		assert.True(t, overflowed)
		// Nothing after the gap may reach the client before the subscription is removed.
		assert.True(t, b.hold([]byte("d")))
	})
}

// eventIDRecorder records the event ID set before each flushed message.
type eventIDRecorder struct {
	*SubscriptionRecorder
	mu      sync.Mutex
	pending string
	ids     []string
}

var _ SubscriptionEventIDWriter = (*eventIDRecorder)(nil)

func (r *eventIDRecorder) SetEventID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = id
}

func (r *eventIDRecorder) Flush() error {
	r.mu.Lock()
	r.ids = append(r.ids, r.pending)
	r.mu.Unlock()
	return r.SubscriptionRecorder.Flush()
}

func (r *eventIDRecorder) eventIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

func TestResolver_SubscriptionEventID(t *testing.T) {
	const timeout = 10 * time.Second

	stream := newBackfillStream(func(ctx StartupHookContext, _ []byte) (func([]byte) bool, error) {
		ctx.Updater([]byte(`{"data":{"counter":1},"eventId":"1-0"}`))
		return nil, nil
	})

	resolver := newResolver(t.Context())
	recorder := &eventIDRecorder{SubscriptionRecorder: newRecorder()}
	id := SubscriptionIdentifier{ConnectionID: 1, SubscriptionID: 1}
	require.NoError(t, resolver.AsyncResolveGraphQLSubscription(NewContext(context.Background()), backfillTestPlan(stream), recorder, id))

	recorder.AwaitMessages(t, 1, timeout)
	stream.live(2)
	recorder.AwaitMessages(t, 2, timeout)
	stream.updater.Update([]byte(`{"eventId":"3-0","data":{"counter":3}}`))
	recorder.AwaitMessages(t, 3, timeout)

	// The event ID is not part of the response, and events without one reset it.
	assert.Equal(t, counterMessages(1, 2, 3), recorder.Messages())
	assert.Equal(t, []string{"1-0", "", "3-0"}, recorder.eventIDs())

	stream.complete()
	recorder.AwaitComplete(t, timeout)
}
