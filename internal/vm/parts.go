package vm

import (
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

// PickRow is one row of the tools picker: a toolchain, an agent or a
// layer, what it is or why it's checked, whether it's checked, and
// whether it's shown as it is (a VM's layers, which only a new VM gets).
type PickRow struct {
	Name      string
	Desc      msg.M // the zero message for none
	On, Fixed bool
}

// PickPart is a heading of the tools picker and its rows.
type PickPart struct {
	Heading msg.M
	Rows    []PickRow
}

// Parts are what a new VM gets besides base: its toolchains, its agents
// and its layers, each a part of the picker and a flag of its own.
type Parts struct {
	Toolchains, Agents, Layers []string
}

// tools is the parts' shared tools, in a VM's order.
func (p Parts) tools() []string { return image.SortTools(slices.Concat(p.Toolchains, p.Agents)) }

// image is the image the parts' layers make.
func (p Parts) image() image.Name { return image.NameOf(p.Layers) }

// listOrNone is names as a message shows a list: space-separated, or
// none.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// partsDefault is what a new VM gets of each part when its flag is left
// out, and why: the toolchains the folder's files call for and those its
// devcontainer.json's features map to, the agents devvm setup chose, and
// the layers devvm setup chose, then those the devcontainer.json's
// features map to. why names the file that calls for each toolchain or
// layer; setup and setupLayers are devvm setup's agents and layers.
type partsDefault struct {
	Parts
	why                map[string]string
	setup, setupLayers []string
}

// defaults works out a new VM's default parts for the folder dir with
// its devcontainer.json's plan dc (nil for none), and the agents def
// chooses. A layer a project file alone calls for, such as nix for a
// flake.nix, isn't one: only its reason is kept, for the picker.
func (e creation) defaults(dir string, dc *devcontainer.Plan, def settings.Defaults) partsDefault {
	d := partsDefault{why: map[string]string{}}
	for _, f := range detectFiles(dir) {
		d.why[f.Name] = f.File
		if image.IsTool(f.Name) {
			d.Toolchains = append(d.Toolchains, f.Name)
		}
	}
	if dc != nil {
		for _, p := range dc.Layers {
			if _, ok := d.why[p]; !ok {
				d.why[p] = dc.File
			}
			if image.IsTool(p) {
				d.Toolchains = append(d.Toolchains, p)
			} else {
				d.Layers = append(d.Layers, p)
			}
		}
	}
	d.Toolchains = image.SortTools(d.Toolchains)
	d.Agents = image.AgentsOf(def.Agents.Names)
	d.setup = d.Agents
	return d
}

// addSetupLayers puts devvm setup's layers (def's) under d's, those of
// the user's whose script is gone (not one of b's) left out with a
// warning naming each: a VM is made without it rather than not at all.
func (e creation) addSetupLayers(d *partsDefault, vm vmname.VM, def settings.Defaults, b *build.Env) {
	layers := b.Layers()
	for _, l := range def.Layers.Names {
		if !slices.Contains(layers, l) {
			e.Warn(msg.DefaultLayerGone(b.Dirs.Layers(), l, vm))
			continue
		}
		d.setupLayers = append(d.setupLayers, l)
	}
	d.Layers = slices.Concat(d.setupLayers, d.Layers)
}

// requireSetupLayers refuses a layer of devvm setup's the VM gets that
// b's backend can't run (docker-layer, nix-layer), naming the layer and
// both ways out, before the VM's other needs are checked.
func (e creation) requireSetupLayers(vm vmname.VM, layers []string, d partsDefault, b *build.Env) error {
	for _, l := range layers {
		if !slices.Contains(d.setupLayers, l) {
			continue
		}
		for _, f := range backend.ImageFeatures(image.NameOf([]string{l})) {
			if err := e.Registry.Require(b.Backend, vm, f); err != nil {
				return msg.DefaultLayerLacks(b.Backend.Name(), string(f), l, vm).Because(err)
			}
		}
	}
	return nil
}

// partFlag is a part's flag: its name, the list it was given (nil when
// it wasn't), and the part of a Parts it sets.
type partFlag struct {
	name  string
	given *[]string
	part  *[]string
}

// partFlags are o's --tools, --agents and --layers, each with the part
// of p it sets, in the picker's order.
func partFlags(o Options, p *Parts) []partFlag {
	return []partFlag{
		{"--tools", o.Toolchains, &p.Toolchains}, {"--agents", o.Agents, &p.Agents}, {"--layers", o.Layers, &p.Layers},
	}
}

// partsFor works out what a new VM gets of each part: the flags', each
// the exact list for its part, and the defaults for those left out (def's
// agents and layers), checked (checkParts). It returns the defaults too,
// which its summary and the picker say why of.
func (e creation) partsFor(o Options, vm vmname.VM, dir string, dc *devcontainer.Plan, b *build.Env, def settings.Defaults) (partsDefault, Parts, error) {
	d := e.defaults(dir, dc, def)
	if o.Layers == nil {
		e.addSetupLayers(&d, vm, def, b)
	}
	p := d.Parts
	for _, f := range partFlags(o, &p) {
		if f.given != nil {
			*f.part = *f.given
		}
	}
	p, err := e.checkParts(vm, p, d, b)
	return d, p, err
}

// checkParts returns p with its layers in its image's order, once they're
// checked against b's and its backend's (requireSetupLayers).
func (e creation) checkParts(vm vmname.VM, p Parts, d partsDefault, b *build.Env) (Parts, error) {
	img := p.image()
	p.Layers = img.Layers()
	if err := (build.Names{Env: b}).Check(img); err != nil {
		return Parts{}, err
	}
	if err := e.requireSetupLayers(vm, p.Layers, d, b); err != nil {
		return Parts{}, err
	}
	return p, nil
}

// allGiven reports whether o's flags give every part, so the picker has
// nothing to choose.
func allGiven(o Options) bool {
	return o.Toolchains != nil && o.Agents != nil && o.Layers != nil
}

// pickNew shows the picker for a new VM, what it gets (p) checked: each
// part's defaults saying why, and a part a flag gave greyed out as it
// was given. It returns the parts chosen.
func (e creation) pickNew(vm vmname.VM, o Options, d partsDefault, p Parts, b *build.Env) Parts {
	why := func(name string) msg.M {
		if f, ok := d.why[name]; ok {
			return msg.PickCalledFor(f)
		}
		return msg.M{}
	}
	var toolchains, agents, layers []PickRow
	for _, t := range image.Toolchains {
		toolchains = append(toolchains, PickRow{Name: t, Desc: why(t), On: slices.Contains(p.Toolchains, t)})
	}
	for _, a := range image.Agents {
		r := PickRow{Name: a, On: slices.Contains(p.Agents, a)}
		if slices.Contains(d.setup, a) {
			r.Desc = msg.PickFromSetup()
		}
		agents = append(agents, r)
	}
	for _, l := range b.Layers()[1:] {
		desc := why(l)
		switch {
		case desc.ID() != "":
		case slices.Contains(d.setupLayers, l):
			desc = msg.PickFromSetup()
		default:
			desc = layerDesc(l)
		}
		layers = append(layers, PickRow{Name: l, Desc: desc, On: slices.Contains(p.Layers, l)})
	}
	parts := []PickPart{{msg.PickToolchains(), toolchains}, {msg.PickAgents(), agents}, {msg.PickLayers(), layers}}
	for i, f := range partFlags(o, &p) {
		if f.given == nil {
			continue
		}
		for j := range parts[i].Rows {
			r := &parts[i].Rows[j]
			r.Fixed = true
			if r.On {
				r.Desc = msg.PickFromFlag(f.name)
			}
		}
	}
	got := e.PickParts(msg.PickNewHelp(vm), parts)
	return Parts{Toolchains: checked(got[0]), Agents: checked(got[1]), Layers: inOrder(p.Layers, checked(got[2]))}
}

// inOrder is the layers chosen, those of was first in its order, bottom
// first, then the others as the picker lists them: a picker shows layers
// sorted, but an image's name joins them bottom first.
func inOrder(was, chosen []string) []string {
	var out []string
	for _, l := range was {
		if slices.Contains(chosen, l) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	for _, l := range chosen {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// layerDesc is what a layer is, as the picker says it.
func layerDesc(layer string) msg.M {
	switch layer {
	case image.DesktopLayer:
		return msg.PickLayerDesktop()
	case image.DockerLayer:
		return msg.PickLayerDocker()
	case image.NixLayer:
		return msg.PickLayerNix()
	}
	return msg.PickLayerYours()
}

// checked is the names of a part's checked rows, in order.
func checked(p PickPart) []string {
	var out []string
	for _, r := range p.Rows {
		if r.On {
			out = append(out, r.Name)
		}
	}
	return out
}

// AgentsChoice chooses the coding agents new VMs get by default, which
// the picker checks and a VM made without one gets: from a flag (Set), or
// asked for (Ask). Existing VMs keep their agents.
type AgentsChoice struct {
	Dirs paths.Dirs
	// Pick shows the question and returns the rows as chosen; Reporter
	// says what's saved.
	Pick func(msg.M, []PickPart) []PickPart
	msg.Reporter
}

// Set saves agents as new VMs' agents: none for none. A name that isn't an
// agent's is refused, and so is a choice config.toml makes
// (set_in_config).
func (c AgentsChoice) Set(agents []string) error {
	if err := CheckAgents(agents); err != nil {
		return err
	}
	sorted := append([]string{}, image.SortTools(agents)...)
	if _, err := settings.Save(c.Dirs, settings.Values{Agents: &sorted}); err != nil {
		return err
	}
	c.say(sorted)
	return nil
}

// Ask asks which agents new VMs get, starting from the ones in effect,
// and saves the answer; when config.toml chooses them, it says so and
// asks nothing.
func (c AgentsChoice) Ask() error {
	def, err := settings.Read(c.Dirs)
	if err != nil {
		return err
	}
	if def.Agents.Source == settings.FromConfig {
		c.Info(msg.SetupInConfig(c.Dirs.ConfigFile(), settings.KeyAgents))
		return nil
	}
	rows := make([]PickRow, len(image.Agents))
	for i, a := range image.Agents {
		rows[i] = PickRow{Name: a, On: slices.Contains(def.Agents.Names, a)}
	}
	got := c.Pick(msg.AgentsHelp(), []PickPart{{Rows: rows}})
	return c.Set(checked(got[0]))
}

func (c AgentsChoice) say(agents []string) {
	if len(agents) == 0 {
		c.Info(msg.DefaultAgentsNone())
		return
	}
	c.Info(msg.DefaultAgents(strings.Join(agents, " ")))
}

// LayersChoice chooses the layers new VMs get by default, which the
// picker checks and a VM made without --layers gets: from a flag (Set),
// or asked for (Ask). Existing VMs keep their layers.
type LayersChoice struct {
	Dirs paths.Dirs
	// Layers are the layers there are, the built-in ones and the user's
	// (build.Env's Layers, without base).
	Layers []string
	// Pick shows the question and returns the rows as chosen; Reporter
	// says what's saved.
	Pick func(msg.M, []PickPart) []PickPart
	msg.Reporter
}

// Set saves layers as new VMs' layers, bottom first as given: none for
// none. A name that isn't a layer's is refused, and so is a choice
// config.toml makes (set_in_config).
func (c LayersChoice) Set(layers []string) error {
	if err := CheckPart("--layers", layers, c.Layers); err != nil {
		return err
	}
	bottomUp := append([]string{}, image.NameOf(layers).Layers()...)
	if _, err := settings.Save(c.Dirs, settings.Values{Layers: &bottomUp}); err != nil {
		return err
	}
	if len(bottomUp) == 0 {
		c.Info(msg.DefaultLayersNone())
	} else {
		c.Info(msg.DefaultLayers(strings.Join(bottomUp, " ")))
	}
	return nil
}

// Ask asks which layers new VMs get, starting from the ones in effect,
// and saves the answer, the layers kept in their order; when config.toml
// chooses them, it says so and asks nothing.
func (c LayersChoice) Ask() error {
	def, err := settings.Read(c.Dirs)
	if err != nil {
		return err
	}
	if def.Layers.Source == settings.FromConfig {
		c.Info(msg.SetupInConfig(c.Dirs.ConfigFile(), settings.KeyLayers))
		return nil
	}
	rows := make([]PickRow, len(c.Layers))
	for i, l := range c.Layers {
		rows[i] = PickRow{Name: l, Desc: layerDesc(l), On: slices.Contains(def.Layers.Names, l)}
	}
	got := c.Pick(msg.LayersHelp(), []PickPart{{Rows: rows}})
	return c.Set(inOrder(def.Layers.Names, checked(got[0])))
}

// CheckAgents refuses a name that isn't an agent's, as --agents does.
func CheckAgents(names []string) error {
	return CheckPart("--agents", names, image.Agents)
}

// CheckPart refuses a name of a part's flag (--tools, --agents, --layers)
// that isn't one of names, suggesting the closest (msg.Closest).
func CheckPart(flag string, given, names []string) error {
	for _, g := range given {
		if slices.Contains(names, g) {
			continue
		}
		if like := msg.Closest(g, names); like != "" {
			return msg.UnknownPartLike(flag, g, like)
		}
		return msg.UnknownPart(flag, g, strings.Join(names, " "))
	}
	return nil
}

// CheckLayers refuses a name given as a layer (devvm layer add's, and
// devvm layer rm's once it has the VM) that
// isn't one of layers, the built-in layers and the user's: a shared
// tool's (tool_not_layer), else naming the closest (msg.Closest), else
// them all.
func CheckLayers(given, layers []string) error {
	for _, g := range given {
		switch {
		case slices.Contains(layers, g):
		case image.IsTool(g):
			return msg.ToolNotLayer(g)
		case msg.Closest(g, layers) != "":
			return msg.UnknownLayerLike(g, msg.Closest(g, layers))
		default:
			return msg.UnknownLayer(g, strings.Join(layers, " "))
		}
	}
	return nil
}

// CheckLayerNames refuses a name given as a layer to take out (devvm
// layer rm's) that no image can have, as CheckLayers does: base, a
// shared tool's, or one that isn't a layer's name. Any other name may be
// one of the VM's layers, even one of the user's whose script is gone,
// which RemoveLayers tells once it has the VM.
func CheckLayerNames(given, layers []string) error {
	return CheckLayers(slices.DeleteFunc(slices.Clone(given), image.LayerName), layers)
}

// firstRunAgents asks which agents new VMs get on devvm's first run in a
// terminal, until they're chosen (def), unless --agents chose this VM's.
func (e creation) firstRunAgents(o Options, def settings.Defaults) error {
	if !e.asksAgents(o, def) {
		return nil
	}
	return AgentsChoice{Dirs: e.Dirs, Pick: e.PickParts, Reporter: e.Reporter}.Ask()
}

// asksAgents reports whether firstRunAgents asks: in a terminal with the
// picker, until they're chosen (def), without --agents.
func (e creation) asksAgents(o Options, def settings.Defaults) bool {
	return o.Agents == nil && e.CanPick() && e.PickParts != nil && !def.Agents.Chosen
}
