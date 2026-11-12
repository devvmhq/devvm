package cli

import (
	"os"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/registry"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
)

// app is one run's services, on its UI: devvm's folders, a test build's
// overrides, the secrets store, the backends' registry, the shared
// tools, the environment of the commands on a folder's VM and the image
// builder of the backend new VMs get. Each is made the first time a
// command asks for it, failing or not, and that one is kept for the rest
// of the run. NewRoot makes one and hands it to every command.
type app struct {
	u *ui.UI
	// folder is the command's --here and --parent, which vmEnv's
	// environment finds the folder's VM by.
	folder folderFlags

	folders   made[paths.Dirs]
	overrides made[Overrides]
	store     made[*secrets.Store]
	reg       made[*backend.Registry]
	tl        made[*tools.Env]
	vm        made[*vm.Env]
	b         made[*build.Env]
}

// newApp returns the services of a run on u, none of them made yet.
func newApp(u *ui.UI) *app { return &app{u: u} }

// made is one of a run's services: the value and error of its first get,
// kept for every get after it. n counts the times it was made, never more
// than once, which the tests check.
type made[T any] struct {
	v   T
	err error
	n   int
}

// get returns the service, making it with mk the first time.
func (m *made[T]) get(mk func() (T, error)) (T, error) {
	if m.n == 0 {
		m.n++
		m.v, m.err = mk()
	}
	return m.v, m.err
}

// dirs is devvm's folders, as the environment gives them.
func (a *app) dirs() (paths.Dirs, error) { return a.folders.get(paths.FromEnv) }

// testOverrides is this build's overrides, read from the environment
// (readOverrides): a release's are empty.
func (a *app) testOverrides() Overrides {
	o, _ := a.overrides.get(func() (Overrides, error) { return readOverrides(os.Getenv), nil })
	return o
}

// host is the host devvm runs on, as this build reads it: a test build's
// with its overrides.
func (a *app) host() platform.Host { return platform.NewHost(a.testOverrides().Host) }

// secrets is devvm's secrets store, warning through the run's UI.
func (a *app) secrets() (*secrets.Store, error) {
	return a.store.get(func() (*secrets.Store, error) {
		d, err := a.dirs()
		if err != nil {
			return nil, err
		}
		return secrets.NewStore(d, a.u), nil
	})
}

// registry is the registry of devvm's backends, with this build's
// overrides, their own requests through the company proxy, its password
// from devvm's secrets.
func (a *app) registry() (*backend.Registry, error) {
	return a.reg.get(func() (*backend.Registry, error) {
		d, err := a.dirs()
		if err != nil {
			return nil, err
		}
		store, err := a.secrets()
		if err != nil {
			return nil, err
		}
		cp := settings.NewCompanyProxy(d, os.Getenv, store.ProxyPassword)
		return registry.New(d, assets.FS, a.u, a.testOverrides().Overrides, cp)
	})
}

// tools is the shared tools in devvm's folders, checking the host's free
// space before a build, with this build's mise, through the company
// proxy, its password from devvm's secrets.
func (a *app) tools() (*tools.Env, error) {
	return a.tl.get(func() (*tools.Env, error) {
		d, err := a.dirs()
		if err != nil {
			return nil, err
		}
		store, err := a.secrets()
		if err != nil {
			return nil, err
		}
		t := tools.New(d, assets.FS, devvmVersion(), a.u, a.host())
		t.Mise = a.testOverrides().HostMise
		t.Proxy.Password = store.ProxyPassword
		return t, nil
	})
}

// vmEnv is the environment the commands on this folder's VM run in, on
// the run's registry, shared tools and secrets, asking in its terminal,
// finding the VM by the command's --here and --parent.
func (a *app) vmEnv() (*vm.Env, error) {
	return a.vm.get(func() (*vm.Env, error) {
		d, err := a.dirs()
		if err != nil {
			return nil, err
		}
		self, err := paths.Self()
		if err != nil {
			return nil, err
		}
		reg, err := a.registry()
		if err != nil {
			return nil, err
		}
		tl, err := a.tools()
		if err != nil {
			return nil, err
		}
		store, err := a.secrets()
		if err != nil {
			return nil, err
		}
		e := vm.NewEnv(d, assets.FS, self, devvmVersion(), reg, tl, store, a.u)
		e.PickFeatures = pickFeatures(a.u)
		e.PickParts = pickParts(a.u)
		e.PickOne = pickOne(a.u)
		e.PickMany = pickMany(a.u)
		e.Folder = a.folder.rule()
		e.HistoryMax = a.testOverrides().HistoryMax
		e.PasteWait = time.Duration(a.testOverrides().PasteWait) * time.Second
		clip := platform.Clipboard{Tools: a.testOverrides().ClipTools}
		e.ReadClip, e.CopyText = clip.ReadClip, clip.CopyText
		return e, nil
	})
}

// builder is what builds images on the backend new VMs get (the VM
// environment's Builder), its base devvm's own until its caller sets
// From.
func (a *app) builder() (*build.Env, error) {
	return a.b.get(func() (*build.Env, error) {
		env, err := a.vmEnv()
		if err != nil {
			return nil, err
		}
		c, err := env.Registry.For(new(state.VM))
		if err != nil {
			return nil, err
		}
		return env.Builder(c.Backend, ""), nil
	})
}

// env runs do in this folder's environment and returns its failure
// (failed), with what's message for an error that isn't one.
func (a *app) env(do func(*vm.Env) error, what func(reason string) msg.M) error {
	env, err := a.vmEnv()
	if err == nil {
		err = do(env)
	}
	return failed(err, what)
}
