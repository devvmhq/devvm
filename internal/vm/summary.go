package vm

import (
	"slices"
	"strconv"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

// summary is what a new VM gets, as devvm shows it before making it: its
// parts, each with why it gets it, its size, tier and ports, and the
// devcontainer.json it's made as.
type summary struct {
	vm vmname.VM
	o  Options
	d  partsDefault
	p  Parts
	// agentsFrom and layersFrom are what chose setup's agents and layers:
	// devvm setup, or config.toml.
	agentsFrom, layersFrom string
	size                   Size // CPUs, memory and disk, each set (shownSize)
	tier                   netpolicy.Tier
	learn                  bool // it learns the hosts it reaches, on tier internet
	ports                  string
	dc                     string // the devcontainer.json's path, "" for none
}

// chosenBy is what chose a default list from source: config.toml, or
// devvm setup, which saves it.
func chosenBy(source settings.Source) string {
	if source == settings.FromConfig {
		return string(settings.FromConfig)
	}
	return "devvm setup"
}

// shownSize is s with what it leaves unset as the VM gets it: base's
// CPUs and memory fitted to host (SizeChoice.Fitted), and base's disk.
func shownSize(s Size, host Host) Size {
	return s.Or(SizeChoice{Host: host}.Fitted()).Or(Size{Disk: strconv.Itoa(backend.BaseDiskGiB)})
}

// lines are the summary's messages: its heading first, then a line for
// each toolchain, agent and layer, or none of one, then its size, tier,
// ports and devcontainer.json.
func (s summary) lines() []msg.M {
	out := []msg.M{msg.NewVMSummary(s.vm)}
	out = append(out, s.toolchains()...)
	out = append(out, s.agents()...)
	out = append(out, s.layers()...)
	network := msg.NewVMNetwork(string(s.tier))
	if s.learn {
		network = msg.NewVMNetworkLearn()
	}
	out = append(out, msg.NewVMSize(s.size.CPUs, s.size.Disk, s.size.Memory), network, msg.NewVMPorts(s.ports))
	if s.dc != "" {
		out = append(out, msg.NewVMDevcontainer(s.dc))
	}
	return out
}

// toolchains are a line for each toolchain: from --tools, or for the file
// that calls for it.
func (s summary) toolchains() []msg.M {
	if len(s.p.Toolchains) == 0 {
		return []msg.M{msg.NewVMToolchainsNone()}
	}
	var out []msg.M
	for _, t := range s.p.Toolchains {
		if s.o.Toolchains != nil {
			out = append(out, msg.NewVMToolchainFrom("--tools", t))
		} else {
			out = append(out, msg.NewVMToolchainFor(s.d.why[t], t))
		}
	}
	return out
}

// agents are a line for each agent: from --agents, or from what chose
// setup's.
func (s summary) agents() []msg.M {
	if len(s.p.Agents) == 0 {
		return []msg.M{msg.NewVMAgentsNone()}
	}
	from := s.agentsFrom
	if s.o.Agents != nil {
		from = "--agents"
	}
	out := make([]msg.M, 0, len(s.p.Agents))
	for _, a := range s.p.Agents {
		out = append(out, msg.NewVMAgentFrom(a, from))
	}
	return out
}

// layers are a line for each layer, bottom first: from --layers, from
// what chose setup's, or for the devcontainer.json whose features call
// for it.
func (s summary) layers() []msg.M {
	if len(s.p.Layers) == 0 {
		return []msg.M{msg.NewVMLayersNone()}
	}
	out := make([]msg.M, 0, len(s.p.Layers))
	for _, l := range s.p.Layers {
		switch {
		case s.o.Layers != nil:
			out = append(out, msg.NewVMLayerFrom("--layers", l))
		case slices.Contains(s.d.setupLayers, l):
			out = append(out, msg.NewVMLayerFrom(s.layersFrom, l))
		default:
			out = append(out, msg.NewVMLayerFor(s.d.why[l], l))
		}
	}
	return out
}

// leftToChoose reports whether customizing a new VM has anything to ask
// once o's flags are taken as given: a part of the picker's, its tier
// when tierOpen (neither --network nor a saved tier, and a backend that
// may confine it), its ports, or its CPUs, memory or disk.
func leftToChoose(o Options, tierOpen bool) bool {
	return !allGiven(o) || tierOpen || o.Ports == "" || o.Size.CPUs == "" || o.Size.Memory == "" || o.Size.Disk == ""
}
