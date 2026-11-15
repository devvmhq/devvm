package vm

import (
	"errors"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

// portsChoiceList is the ports picker's row that asks for a list.
const portsChoiceList = "list"

// choosePorts shows the ports a new VM vm may forward, every port, none
// or a list, for one to be chosen, starting from shown, the summary's;
// a list is asked for (askPorts). Where the picker can't show, it's
// shown. Nothing is saved.
func (e creation) choosePorts(vm vmname.VM, shown string) string {
	if e.PickOne == nil {
		return shown
	}
	rows := portsRows(shown)
	switch rows[e.PickOne(msg.NewVMPortsHelp(vm), rows)].Name {
	case settings.PortsAll:
		return settings.PortsAll
	case settings.PortsNone:
		return settings.PortsNone
	}
	return e.askPorts(shown)
}

// portsRows are the ports picker's rows: every port, none, then a list,
// the one shown is on.
func portsRows(shown string) []PickRow {
	rows := []PickRow{
		{Name: settings.PortsAll, Desc: msg.NewVMPortsAll()},
		{Name: settings.PortsNone, Desc: msg.NewVMPortsNone()},
		{Name: portsChoiceList, Desc: msg.NewVMPortsList()},
	}
	switch shown {
	case settings.PortsAll:
		rows[0].On = true
	case settings.PortsNone:
		rows[1].On = true
	default:
		rows[2].On = true
	}
	return rows
}

// askPorts asks for the ports to forward, Enter keeping shown, checked
// as --ports is (settings.ParsePorts): a list that isn't one is said,
// and asked for again. Where it can't ask, it's shown.
func (e creation) askPorts(shown string) string {
	q := msg.NewVMPortsPrompt(shown).AnsweredBy("--ports")
	for {
		v, err := e.Answer(q)
		if err != nil || v == "" {
			return shown
		}
		list, err := settings.ParsePorts(v)
		if err == nil {
			return list
		}
		if m, ok := errors.AsType[msg.M](err); ok {
			e.Warn(m)
		}
	}
}
