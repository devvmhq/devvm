package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// configFile is config.toml as it's written: each value checked as it's
// decoded, so a bad one names its line.
type configFile struct {
	Network  opt[networkKind]  `toml:"network"`
	Ports    opt[portsKind]    `toml:"ports"`
	Features list[featureKind] `toml:"features"`
	Agents   list[agentKind]   `toml:"agents"`
	Layers   list[layerKind]   `toml:"layers"`
	Size     struct {
		CPUs   opt[cpusKind]   `toml:"cpus"`
		Memory opt[memoryKind] `toml:"memory"`
		Disk   opt[diskKind]   `toml:"disk"`
	} `toml:"size"`
	Retention struct {
		Logs struct {
			Runs opt[countKind] `toml:"runs"`
		} `toml:"logs"`
		History struct {
			Age   opt[durationKind] `toml:"age"`
			Count opt[countKind]    `toml:"count"`
		} `toml:"history"`
		Tools struct {
			UnusedAge opt[durationKind] `toml:"unused-age"`
		} `toml:"tools"`
		Orphans struct {
			Age opt[durationKind] `toml:"age"`
		} `toml:"orphans"`
		NixCache struct {
			Size opt[nixSizeKind] `toml:"size"`
		} `toml:"nix-cache"`
	} `toml:"retention"`
	// UbuntuMirror is base's first mirror (UbuntuMirror), which no command
	// saves.
	UbuntuMirror opt[ubuntuMirrorKind] `toml:"ubuntu-mirror"`
	// ReleaseCheck is the daily check for a newer release (ReleaseCheck),
	// which no command saves.
	ReleaseCheck opt[switchKind] `toml:"release-check"`
	// PassKeys are the providers whose API keys an agent's VMs get as
	// they are (PassKeys), which no command saves.
	PassKeys passKeys `toml:"pass-keys"`
	// Proxy is the company proxy (HostProxy), which no command saves.
	Proxy struct {
		HTTPS   opt[proxyHTTPSKind] `toml:"https"`
		HTTP    opt[proxyHTTPKind]  `toml:"http"`
		NoProxy opt[noProxyKind]    `toml:"no-proxy"`
	} `toml:"proxy"`
}

// values is what c sets.
func (c *configFile) values() Values {
	r := c.Retention
	v := Values{
		Network: c.Network.ptr(), Ports: c.Ports.ptr(),
		Size:     SizeValues{CPUs: c.Size.CPUs.ptr(), Memory: c.Size.Memory.ptr(), Disk: c.Size.Disk.ptr()},
		Features: c.Features.ptr(), Agents: c.Agents.ptr(), Layers: c.Layers.ptr(),
		Retention: map[string]string{},
	}
	for k, o := range map[string]interface{ ptr() *string }{
		RetainRunLogs: &r.Logs.Runs, RetainHistoryAge: &r.History.Age, RetainHistoryCount: &r.History.Count,
		RetainToolsAge: &r.Tools.UnusedAge, RetainOrphansAge: &r.Orphans.Age, RetainNixCacheSize: &r.NixCache.Size,
	} {
		if p := o.ptr(); p != nil {
			v.Retention[k] = *p
		}
	}
	return v
}

// readConfig reads config.toml at path. A missing file sets nothing; one
// devvm can't read, an unknown key or a value devvm can't use is a
// state.CorruptError naming the file, and the line where TOML has one.
func readConfig(path string) (Values, error) {
	cf, err := decodeConfig(path)
	if err != nil {
		return Values{}, err
	}
	return cf.values(), nil
}

// decodeConfig reads config.toml at path whole, as readConfig does.
func decodeConfig(path string) (*configFile, error) {
	var cf configFile
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &cf, nil
	case err != nil:
		return nil, err
	}
	md, err := toml.Decode(string(data), &cf)
	if err != nil {
		c := &state.CorruptError{Path: path, Reason: err.Error()}
		if pe, ok := errors.AsType[toml.ParseError](err); ok {
			c.Line, c.Reason = pe.Position.Line, pe.Message
		}
		// TOML keeps only a check's text: the value that failed keeps its
		// message, so names mode can show it.
		if m, ok := cf.refused(); ok {
			c.Reason, c.Msg = "", m
		}
		return nil, c
	}
	k := cf.PassKeys.unknown()
	if keys := md.Undecoded(); len(keys) > 0 {
		k = keys[0].String()
	}
	if k != "" {
		return nil, &state.CorruptError{Path: path, Line: keyLine(string(data), k), Reason: fmt.Sprintf("unknown key %q", k)}
	}
	return &cf, nil
}

// A refuser is one of config.toml's values, which keeps the message its
// check refused it with.
type refuser interface{ refusal() (msg.M, bool) }

// refused is the message a value of c's was refused with, if one was.
func (c *configFile) refused() (msg.M, bool) {
	r := c.Retention
	for _, v := range []refuser{
		&c.Network, &c.Ports, &c.Features, &c.Agents, &c.Layers, &c.Size.CPUs, &c.Size.Memory, &c.Size.Disk,
		&r.Logs.Runs, &r.History.Age, &r.History.Count, &r.Tools.UnusedAge, &r.Orphans.Age, &r.NixCache.Size,
		&c.Proxy.HTTPS, &c.Proxy.HTTP, &c.Proxy.NoProxy, &c.UbuntuMirror, c.PassKeys,
	} {
		if m, ok := v.refusal(); ok {
			return m, true
		}
	}
	return msg.M{}, false
}

// keyLine returns the line of data, a TOML file, that sets key or opens
// its table, 0 when it finds none: TOML's decoder gives no line for a key
// it didn't decode. Keys are matched as written, quotes and spaces around
// dots dropped.
func keyLine(data, key string) int {
	table := ""
	for i, line := range strings.Split(data, "\n") {
		l := strings.TrimSpace(line)
		var k string
		switch {
		case strings.HasPrefix(l, "["):
			table = normKey(strings.Trim(strings.SplitN(l, "]", 2)[0], "[ "))
			k = table
		case strings.Contains(l, "=") && !strings.HasPrefix(l, "#"):
			k = normKey(strings.SplitN(l, "=", 2)[0])
			if table != "" {
				k = table + "." + k
			}
		default:
			continue
		}
		if k == key {
			return i + 1
		}
	}
	return 0
}

// normKey is a TOML key as toml.Key.String writes it: its parts joined by
// dots, without quotes or spaces.
func normKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// A kind checks one of config.toml's values as TOML decoded it and returns
// it as Values holds it.
type kind interface{ parse(v any) (string, error) }

// opt is a value of config.toml's of kind K, and whether it's set.
type opt[K kind] struct {
	v   string
	set bool
	bad error // what the check refused it with
}

// UnmarshalTOML checks the value as K's; TOML names the line of an error.
func (o *opt[K]) UnmarshalTOML(v any) error {
	var k K
	s, err := k.parse(v)
	if err != nil {
		o.bad = err
		return err
	}
	o.v, o.set = s, true
	return nil
}

func (o *opt[K]) refusal() (msg.M, bool) { return errors.AsType[msg.M](o.bad) }

func (o *opt[K]) ptr() *string {
	if !o.set {
		return nil
	}
	return Of(o.v)
}

// list is a list of config.toml's, each name of kind K, and whether it's
// set; repeats are refused.
type list[K kind] struct {
	v   []string
	set bool
	bad error // what the check refused a name with
}

func (l *list[K]) refusal() (msg.M, bool) { return errors.AsType[msg.M](l.bad) }

// UnmarshalTOML checks each name as K's.
func (l *list[K]) UnmarshalTOML(v any) error {
	items, ok := v.([]any)
	if !ok {
		return errText("not a list of names, such as [\"a\", \"b\"]")
	}
	names := []string{}
	var k K
	for _, it := range items {
		s, err := k.parse(it)
		if err != nil {
			l.bad = err
			return err
		}
		if slices.Contains(names, s) {
			return errText(strconv.Quote(s) + " is listed twice")
		}
		names = append(names, s)
	}
	l.v, l.set = names, true
	return nil
}

func (l *list[K]) ptr() *[]string {
	if !l.set {
		return nil
	}
	return Of(l.v)
}

// text is a TOML string as a value; number, a whole or decimal number or a
// string, as one.
func text(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errText(fmt.Sprintf("not a string: %v", v))
	}
	return s, nil
}

func number(v any) (string, error) {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10), nil
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), nil
	}
	return text(v)
}

type (
	networkKind  struct{}
	portsKind    struct{}
	cpusKind     struct{}
	memoryKind   struct{}
	diskKind     struct{}
	featureKind  struct{}
	agentKind    struct{}
	layerKind    struct{}
	countKind    struct{}
	durationKind struct{}
	nixSizeKind  struct{}
)

func (networkKind) parse(v any) (string, error) { return checked(text(v))(checkNetwork) }
func (portsKind) parse(v any) (string, error) {
	s, err := text(v)
	if err != nil {
		return "", err
	}
	return ParsePorts(s)
}

func (cpusKind) parse(v any) (string, error) {
	s, err := number(v)
	if err != nil {
		return "", err
	}
	return ParseCPUs("cpus", s)
}

func (memoryKind) parse(v any) (string, error) {
	s, err := number(v)
	if err != nil {
		return "", err
	}
	return ParseGiB("memory", s)
}

func (diskKind) parse(v any) (string, error) {
	s, err := number(v)
	if err != nil {
		return "", err
	}
	return ParseGiB("disk", s)
}

func (featureKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error { return checkFeatures([]string{s}) })
}

func (agentKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error { return checkAgents([]string{s}) })
}

func (layerKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error { return checkLayers([]string{s}) })
}

func (countKind) parse(v any) (string, error) {
	return checked(number(v))(func(s string) error { _, err := count(s); return err })
}

func (durationKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error { _, err := state.ParseDuration(s); return err })
}

func (nixSizeKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error { _, err := state.ParseSize(s); return err })
}

// checked returns s, from text or number, once check takes it.
func checked(s string, err error) func(check func(string) error) (string, error) {
	return func(check func(string) error) (string, error) {
		if err != nil {
			return "", err
		}
		if err := check(s); err != nil {
			return "", err
		}
		return s, nil
	}
}

// checkFeatures refuses a name a base feature can't have, and a repeat.
func checkFeatures(names []string) error {
	for i, n := range names {
		switch {
		case !image.FeatureName(n):
			return errText("not a feature's name: " + strconv.Quote(n))
		case slices.Contains(names[:i], n):
			return errText(strconv.Quote(n) + " is listed twice")
		}
	}
	return nil
}

// checkAgents refuses a name that isn't a coding agent's, and a repeat.
func checkAgents(names []string) error {
	for i, n := range names {
		switch {
		case !slices.Contains(image.Agents, n):
			return errText(fmt.Sprintf("%q isn't a coding agent: %s", n, strings.Join(image.Agents, ", ")))
		case slices.Contains(names[:i], n):
			return errText(strconv.Quote(n) + " is listed twice")
		}
	}
	return nil
}

// checkLayers refuses a name a layer can't have (base and a shared tool's
// among them), and a repeat. Whether the layer exists is new VMs' to
// check: a layer of the user's that's gone is skipped, with a warning.
func checkLayers(names []string) error {
	for i, n := range names {
		switch {
		case !image.LayerName(n):
			return errText("not a layer's name: " + strconv.Quote(n))
		case slices.Contains(names[:i], n):
			return errText(strconv.Quote(n) + " is listed twice")
		}
	}
	return nil
}

// errText is a reason a value is refused, which CorruptError shows.
func errText(s string) error { return errors.New(s) }

// errNotChoice refuses v, which isn't one of choices.
func errNotChoice(v string, choices []string) error {
	return errText(fmt.Sprintf("%q isn't one of %s", v, strings.Join(choices, ", ")))
}
