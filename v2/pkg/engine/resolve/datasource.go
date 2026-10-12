package resolve

import (
	"context"
	"net/http"

	"github.com/cespare/xxhash/v2"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/datasource/httpclient"
)

type DataSource interface {
	Load(ctx context.Context, headers http.Header, input []byte) (data []byte, err error)
	LoadWithFiles(ctx context.Context, headers http.Header, input []byte, files []*httpclient.FileUpload) (data []byte, err error)
}

type SubscriptionDataSource interface {
	// Start is called when a new subscription is created. It establishes the connection to the data source.
	// The updater is used to send updates to the client. Deduplication of the request must be done before calling this method.
	Start(ctx *Context, headers http.Header, input []byte, updater SubscriptionUpdater) error
	// HashTriggerInput writes identity-relevant fields of the subscription into xxh.
	// The resolver appends the subgraph headers hash afterward to produce the final trigger ID.
	HashTriggerInput(input []byte, xxh *xxhash.Digest) error
}

// HookableSubscriptionDataSource is a hookable interface for subscription data sources.
// It is used to call a function when a subscription is started.
// This is useful for data sources that need to do some work when a subscription is started,
// e.g. to establish a connection to the data source or to emit updates to the client.
// The function is called with the context and the input of the subscription.
// The function is called before the subscription is started and can be used to emit updates to the client.
type HookableSubscriptionDataSource interface {
	// SubscriptionOnStart is called when a new subscription is created
	// If an error is returned, the error is propagated to the client.
	SubscriptionOnStart(ctx StartupHookContext, input []byte) (err error)
}

// BackfillSubscriptionDataSource is implemented by subscription data sources that can
// replay events a subscriber missed, e.g. while it was reconnecting.
//
// For these sources the resolver guarantees a gap-free, ordered hand-over from replayed
// to live events:
//  1. Source.Start has returned before SubscriptionBackfill runs, so the source must be
//     subscribed to live events by the time Start returns. Anything published after the
//     backfill read is then guaranteed to arrive as a live event.
//  2. Live events for the subscriber are held from the moment it is registered until
//     SubscriptionBackfill returns. Events emitted through ctx.Updater are written
//     immediately, in order.
//  3. The held events are then written, except those for which the returned skip
//     function reports true (typically because the backfill already sent them).
//
// SubscriptionOnStart, if implemented, runs after Source.Start and before
// SubscriptionBackfill. Returning an error from either closes the subscription.
type BackfillSubscriptionDataSource interface {
	SubscriptionBackfill(ctx StartupHookContext, input []byte) (skip func(data []byte) bool, err error)
}

// HookablePubsubDatasource is an extension of HookableSubscriptionDataSource for pubsub datasources.
// They contain additional hooks which make sense for pubsub based datasources but not for normal
// subscription based datasources.
type HookablePubsubDatasource interface {
	HookableSubscriptionDataSource
	// SubscriptionOnCreate is called right before the trigger gets generated.
	// It lets a source rewrite the subscription event configuration.
	SubscriptionOnCreate(ctx context.Context, input []byte) (newInput []byte, err error)
}
