package storage

import "context"

type CallAttribution struct {
	Operation string
	KeyClass  string
	Reason    string
}

type callAttributionKey struct{}

func WithCallAttribution(ctx context.Context, attribution CallAttribution) context.Context {
	return context.WithValue(ctx, callAttributionKey{}, attribution)
}

func CallAttributionFromContext(ctx context.Context) CallAttribution {
	attribution, _ := ctx.Value(callAttributionKey{}).(CallAttribution)
	return attribution
}
