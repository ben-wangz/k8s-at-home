package download

import "context"

type cleanupGuardKey struct{}

func WithCleanupGuard(ctx context.Context, guard func(context.Context) error) context.Context {
	return context.WithValue(ctx, cleanupGuardKey{}, guard)
}

func validateCleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard, ok := ctx.Value(cleanupGuardKey{}).(func(context.Context) error); ok {
		return guard(ctx)
	}
	return nil
}
