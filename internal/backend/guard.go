package backend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/vmname"
)

// guarded is a registered backend, as the registry hands it out: CreateVM,
// Start, BuildImage and BuildTools first check that the backend has what
// their spec needs (Require's refusal, without its warning: the caller's
// own Require gave that) and that the spec is well formed (its Validate),
// so a caller that forgot Require never reaches a backend with a feature
// it lacks. Callers still call Require first, for the refusal at the
// right moment, before anything is built or made. It has every role's
// methods, its own in place of the backend's for the ones it guards, so a
// guarded backend handed on as Images or Machines is still guarded.
type guarded struct {
	Backend
	r *Registry
}

var (
	_ Host         = (*guarded)(nil)
	_ Images       = (*guarded)(nil)
	_ Machines     = (*guarded)(nil)
	_ Housekeeping = (*guarded)(nil)
)

// guard returns b behind the registry's checks, and only the registry's:
// one another registry guards is guarded again by this one alone.
func (r *Registry) guard(b Backend) Backend {
	if g, ok := b.(*guarded); ok {
		b = g.Backend
	}
	return &guarded{Backend: b, r: r}
}

// As returns b as T, a role some backends implement besides Backend's own
// (Forgetter), or the backend's own type in tests: the registry's
// backends are behind its guard, whose type has only Backend's methods,
// so a type assertion on one finds none of the others.
func As[T any](b Backend) (T, bool) {
	if g, ok := b.(*guarded); ok {
		b = g.Backend
	}
	t, ok := b.(T)
	return t, ok
}

func (g *guarded) BuildImage(ctx context.Context, spec ImageSpec) (Built, error) {
	if err := spec.Validate(); err != nil {
		return Built{}, err
	}
	if err := g.r.refusal(g.Backend, spec.Image, nil, spec.Features()); err != nil {
		return Built{}, err
	}
	return g.Backend.BuildImage(ctx, spec)
}

func (g *guarded) BuildTools(ctx context.Context, spec ToolsSpec) error {
	if err := spec.Validate(g.ToolFormat()); err != nil {
		return err
	}
	if err := g.r.refusal(g.Backend, spec.Name, nil, spec.Features()); err != nil {
		return err
	}
	return g.Backend.BuildTools(ctx, spec)
}

func (g *guarded) CreateVM(ctx context.Context, img string, spec VMSpec) error {
	if err := g.r.refusal(g.Backend, spec.Name.String(), nil, spec.Features()); err != nil {
		return err
	}
	return g.Backend.CreateVM(ctx, img, spec)
}

func (g *guarded) Start(ctx context.Context, vm vmname.VM, w Want, c Confinement) error {
	if err := g.r.refusal(g.Backend, vm.String(), nil, slices.Concat(w.Features(), c.Features())); err != nil {
		return err
	}
	return g.Backend.Start(ctx, vm, w, c)
}

// Validate returns an error for a spec no backend builds: a layer from a
// container image (only base starts from one), a network that isn't a
// build's (NetInternet or NetNone), or a key that isn't one (image.IsKey).
func (s ImageSpec) Validate() error {
	if s.From != nil && s.Parent != "" {
		return errors.New("only base starts from a container image")
	}
	if err := buildNet(s.Net); err != nil {
		return err
	}
	for _, k := range []string{s.Key, s.Parent} {
		if k != "" && !image.IsKey(k) {
			return fmt.Errorf("invalid image key %q", k)
		}
	}
	if s.Key == "" {
		return errors.New("an image without a key")
	}
	return nil
}

// Validate returns an error for a tools build no backend runs: a
// network that isn't a build's, images in another format than format
// (the backend's ToolFormat), a base key that isn't one, or folders that
// aren't absolute.
func (s ToolsSpec) Validate(format string) error {
	if err := buildNet(s.Net); err != nil {
		return err
	}
	if s.Format != format {
		return fmt.Errorf("tool images in format %q, not the backend's %q", s.Format, format)
	}
	if !image.IsKey(s.Base) {
		return fmt.Errorf("invalid image key %q", s.Base)
	}
	if !filepath.IsAbs(s.Out) || !filepath.IsAbs(s.OutAt) {
		return fmt.Errorf("the tools build's folders must be absolute: %q at %q", s.Out, s.OutAt)
	}
	return nil
}

// buildNet returns an error unless n is a build's network.
func buildNet(n Net) error {
	if n != NetInternet && n != NetNone {
		return fmt.Errorf("builds are confined, not started with network %d", n)
	}
	return nil
}
