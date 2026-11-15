// Package registry registers every backend devvm has. It's apart from
// package backend, which the backends themselves import.
package registry

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/lima"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
)

// Overrides are a test build's stand-ins for the backends, each the value
// of the environment variable it names, as internal/cli reads them (only
// in a test build: a release has none). "" leaves the backends' own.
type Overrides struct {
	// Host stands in for this machine's size (platform.Overrides).
	Host platform.Overrides
	// Unreviewed (DEVVM_UNREVIEWED) names the unreviewed emulations let
	// through, with a warning each time: for their end-to-end checks.
	Unreviewed string
	// KVM (DEVVM_KVM) stands in for Linux's /dev/kvm.
	KVM string
	// UbuntuURL (DEVVM_UBUNTU_URL) is where Ubuntu's dated releases are
	// listed, in place of ubuntu.DefaultURL.
	UbuntuURL string
	// UbuntuRelease (DEVVM_UBUNTU_RELEASE) is the dated release of
	// Ubuntu's cloud image that base is pinned to, in place of the newest:
	// e2e's, so its runs keep their images.
	UbuntuRelease string
	// Pins (DEVVM_PINS) stands in for Lima's probe of whether this host
	// pins paths in QEMU's namespace: on, or else the error it gives.
	Pins string
	// SSHPorts (DEVVM_SSH_PORTS) is the ports VMs' ssh comes from, in
	// place of ports.SSHPorts (ports.Allocator's SSHList), comma-separated.
	SSHPorts string
	// PortRange (DEVVM_PORT_RANGE) is the range new host ports come from,
	// LO-HI, in place of platform.ChoosablePorts (ports.Allocator's
	// Range): each CLI test's sandbox's own, as sandboxes that run at once
	// would otherwise choose the same ones.
	PortRange string
}

// Range is o's PortRange: the zero range, which leaves
// platform.ChoosablePorts, for none or one that isn't LO-HI of ports.
func (o Overrides) Range() platform.PortRange {
	lo, hi, ok := strings.Cut(o.PortRange, "-")
	l, err1 := strconv.Atoi(lo)
	h, err2 := strconv.Atoi(hi)
	if !ok || err1 != nil || err2 != nil || !backend.ValidPort(l) || !backend.ValidPort(h) || l > h {
		return platform.PortRange{}
	}
	return platform.PortRange{Lo: l, Hi: h}
}

// SSHList is o's SSHPorts as numbers, each entry that isn't a port
// skipped: nil for none, which leaves ports.SSHPorts.
func (o Overrides) SSHList() []int {
	var out []int
	for e := range strings.SplitSeq(o.SSHPorts, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(e)); err == nil && backend.ValidPort(n) {
			out = append(out, n)
		}
	}
	return out
}

// New returns the registry of devvm's backends, on devvm's folders, its
// own files (assets) and r: lima, the default (backend.Default), with a
// test build's overrides o in place of its own, their own requests on
// this machine through the company proxy cp. The registry's Host is o's
// (platform.NewHost).
func New(d paths.Dirs, assets fs.FS, r msg.Reporter, o Overrides, cp *settings.CompanyProxy) (*backend.Registry, error) {
	host := platform.NewHost(o.Host)
	l := lima.New(d, assets, r, host, o.UbuntuURL, cp)
	if o.KVM != "" {
		l.KVM = o.KVM
	}
	l.UbuntuRelease = o.UbuntuRelease
	l.Ports.SSHList, l.Ports.Range = o.SSHList(), o.Range()
	switch o.Pins {
	case "":
	case "on":
		l.Pins = func() error { return nil }
	default:
		l.Pins = func() error { return errors.New(o.Pins) }
	}
	return build(r, host, o.Unreviewed, l)
}

// build returns the registry of bs, on host, warning through rep, with
// the unreviewed emulations unreviewed lets through (DEVVM_UNREVIEWED's
// form); a backend whose name is bad or taken fails it.
func build(rep msg.Reporter, host platform.Host, unreviewed string, bs ...backend.Backend) (*backend.Registry, error) {
	r, err := backend.NewRegistry(bs...)
	if err != nil {
		return nil, err
	}
	r.Host = host
	r.Unreviewed = backend.ParseUnreviewed(unreviewed)
	r.Reporter = rep
	return r, nil
}
