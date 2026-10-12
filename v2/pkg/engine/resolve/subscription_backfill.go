package resolve

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// DefaultMaxSubscriptionBackfillBuffer is the default for ResolverOptions.MaxSubscriptionBackfillBuffer.
const DefaultMaxSubscriptionBackfillBuffer = 10_000

// ErrSubscriptionBackfillOverflow is returned to a subscription when more live events arrived
// during its backfill than the resolver can hold. The client has to resync its state.
var ErrSubscriptionBackfillOverflow = errors.New("RESYNC_REQUIRED: too many events arrived while replaying missed events")

// backfillBuffer holds live events for a subscription while missed events are replayed.
// After the backfill it keeps dropping late live copies of replayed events, which can still
// arrive through UpdateSubscription (e.g. from slow or abandoned per-subscription hooks).
// A nil *backfillBuffer never holds anything.
type backfillBuffer struct {
	mu         sync.Mutex
	active     bool
	limit      int
	held       [][]byte
	overflowed bool
	// skip reports whether a live event was already replayed. Set when the backfill ends.
	skip func(data []byte) bool
}

func newBackfillBuffer(limit int) *backfillBuffer {
	return &backfillBuffer{active: true, limit: limit}
}

// hold reports whether a live event must not be delivered now. While the backfill runs it
// stores a copy of data. Once the limit is reached further events are dropped and the buffer
// is marked as overflowed; it then drops every event until the subscription is removed, so
// nothing after the gap reaches the client. After the backfill it drops replayed events.
func (b *backfillBuffer) hold(data []byte) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.overflowed {
		return true
	}
	if !b.active {
		return b.skip != nil && b.skip(data)
	}
	if len(b.held) >= b.limit {
		b.overflowed = true
		b.held = nil
		return true
	}
	b.held = append(b.held, slices.Clone(data))
	return true
}

// release stops holding events and returns the held ones. skip is used from now on to drop
// live copies of replayed events. After an overflow the buffer keeps dropping everything.
func (b *backfillBuffer) release(skip func(data []byte) bool) (held [][]byte, overflowed bool) {
	if b == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	held, overflowed = b.held, b.overflowed
	b.held = nil
	if overflowed {
		return nil, true
	}
	b.active = false
	b.skip = skip
	return held, false
}

func startupHookContext(add *addSubscription, updater *subscriptionUpdater) StartupHookContext {
	return StartupHookContext{
		Context: add.ctx.Context(),
		Updater: func(data []byte) {
			updater.updateSubscriptionDirect(add.id, data)
		},
	}
}

// backfillOnExistingTrigger backfills a subscription that joined a trigger created by an
// earlier subscription. The trigger's source may still be starting, so wait for it first:
// reading missed events before the source is subscribed would leave a gap.
func (r *Resolver) backfillOnExistingTrigger(add *addSubscription, sub *subscriptionState, trig *trigger, source BackfillSubscriptionDataSource) {
	select {
	case <-trig.started:
	case <-add.ctx.Context().Done():
		return
	case <-trig.updater.ctx.Done():
		// The trigger was torn down. If its start failed, the error was already sent to all
		// of its subscriptions, including this one.
		return
	}
	r.backfillSubscription(add, sub, trig.updater, source)
}

// backfillSubscription runs the startup hooks and the backfill of a subscription whose trigger
// source has started, then flushes the live events held meanwhile. On error the subscription
// receives the error and is removed.
func (r *Resolver) backfillSubscription(add *addSubscription, sub *subscriptionState, updater *subscriptionUpdater, source BackfillSubscriptionDataSource) {
	if r.options.Debug {
		fmt.Printf("resolver:trigger:subscription:backfill:%d\n", add.id.SubscriptionID)
	}
	err := r.executeStartupHooks(add, updater)
	var skip func(data []byte) bool
	if err == nil {
		skip, err = source.SubscriptionBackfill(startupHookContext(add, updater), add.input)
	}
	if err == nil {
		err = updater.finishBackfill(add.id, sub, skip)
	}
	if err != nil {
		if r.options.Debug {
			fmt.Printf("resolver:trigger:subscription:backfill:failed:%d\n", add.id.SubscriptionID)
		}
		sub.writeError(r.errorFormatter, add.ctx, err, add.resolve.Response)
		_ = r.UnsubscribeSubscription(add.id)
	}
}
