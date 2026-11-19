package msg

import "context"

// Waiting is the wait for a lock (platform.Waiting) that shows label on a
// status line of status, a Reporter's Status, while devvm waits for
// another devvm that holds the lock, and never when it's free; nil for a
// nil status, which shows nothing.
func Waiting(status func(M) func(), label M) func() func() {
	if status == nil {
		return nil
	}
	return func() func() { return status(label) }
}

// statusKey is the context key of the run's status (WithStatus).
type statusKey struct{}

// WithStatus returns ctx carrying status, the run's Reporter's Status,
// which the run's root sets: a package that takes no Reporter, such as
// internal/state for a VM's lock or the layout's, shows its waits for a
// lock through it (StatusOf).
func WithStatus(ctx context.Context, status func(M) func()) context.Context {
	return context.WithValue(ctx, statusKey{}, status)
}

// StatusOf is ctx's status (WithStatus), nil for none, as in a process
// with no UI, where Waiting shows nothing.
func StatusOf(ctx context.Context) func(M) func() {
	s, _ := ctx.Value(statusKey{}).(func(M) func())
	return s
}
