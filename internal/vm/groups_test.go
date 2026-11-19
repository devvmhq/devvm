package vm

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

func TestCheckGroupName(t *testing.T) {
	for _, ok := range []string{"web", "team-2", "a"} {
		if err := CheckGroupName(ok); err != nil {
			t.Errorf("CheckGroupName(%q) = %v", ok, msg.NamesOf(err))
		}
	}
	for _, bad := range []string{"", "Web", "a_b", "-a", "a.b", "42", strings.Repeat("a", 64)} {
		if got := msg.NamesOf(CheckGroupName(bad)); !strings.HasPrefix(got, "group_name_invalid ") {
			t.Errorf("CheckGroupName(%q) = %s", bad, got)
		}
	}
}

// A link its group made reads back as one, with the user's beside it.
func TestReadGroupLinks(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := vmState(t, d, "devvm-app-0123456789abc1")
	a := netip.MustParseAddr("127.77.0.1")
	want := []Link{
		{Name: "db", Port: 5432, Target: "devvm-db-0123456789abc2", TargetID: "i1", Addr: a},
		{Name: "db", Port: 8000, Target: "devvm-db-0123456789abc2", TargetID: "i1", Addr: a, Group: true},
	}
	if err := writeLinks(t.Context(), st, want); err != nil {
		t.Fatal(err)
	}
	if v := get(st, KeyLinks); v != "db,5432,devvm-db-0123456789abc2,i1,127.77.0.1 db,8000,devvm-db-0123456789abc2,i1,127.77.0.1,group" {
		t.Errorf("links = %q", v)
	}
	if got, err := ReadLinks(readState(t, d, st.Name())); err != nil || !slices.Equal(got, want) {
		t.Errorf("ReadLinks = %v, %v", got, err)
	}
}

// member makes a VM's state with its published ports and the name it
// joined as.
func member(t *testing.T, d paths.Dirs, name vmname.VM, publish, as string) *state.VM {
	t.Helper()
	return vmState(t, d, name, func(st *state.VM) error {
		return edit(st, linksSection, func(l *linksInVM) error {
			l.Publish, l.GroupAs, l.Group = publish, as, "web"
			return nil
		})
	})
}

// The group's links in a client: a link to each port each other member
// publishes, at one address per member, named as the user's links to it
// are, else by its name in the group; the user's links stay and take
// precedence.
func TestGroupLinks(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	a := netip.MustParseAddr
	app := member(t, d, "devvm-app-0123456789abc1", "", "")
	db := member(t, d, "devvm-db-0123456789abc2", "5432,8000-8002", "")
	api := member(t, d, "devvm-api-0123456789abc3", "80", "web2")
	none := member(t, d, "devvm-cache-0123456789abc6", "", "")
	dbID, apiID := db.ID, api.ID
	others := []*state.VM{api, db, none}

	links, clashes := groupLinks(app, nil, nil, others)
	want := []Link{
		{Name: "db", Port: 5432, Target: db.Name(), TargetID: dbID, Addr: a("127.77.0.2"), Group: true},
		{Name: "db", Port: 8000, Target: db.Name(), TargetID: dbID, Addr: a("127.77.0.2"), Group: true},
		{Name: "db", Port: 8001, Target: db.Name(), TargetID: dbID, Addr: a("127.77.0.2"), Group: true},
		{Name: "db", Port: 8002, Target: db.Name(), TargetID: dbID, Addr: a("127.77.0.2"), Group: true},
		{Name: "web2", Port: 80, Target: api.Name(), TargetID: apiID, Addr: a("127.77.0.1"), Group: true},
	}
	if !slices.Equal(links, want) || clashes != nil {
		t.Errorf("groupLinks = %v, %v; want %v", links, clashes, want)
	}
	if got := shownLinks(links); got != "db:5432,8000-8002, web2:80" {
		t.Errorf("shownLinks = %q", got)
	}

	// The addresses stay as they were, whatever the order.
	again, _ := groupLinks(app, nil, links, []*state.VM{db, api})
	if !slices.Equal(again, want) {
		t.Errorf("again = %v", again)
	}

	// The user's link to db names it and has its port; one called web2 to
	// another VM takes the name from api, whose links go.
	own := []Link{
		{Name: "pg", Port: 5432, Target: db.Name(), TargetID: dbID, Addr: a("127.77.0.2")},
		{Name: "web2", Port: 9000, Target: "devvm-x-0123456789abc8", TargetID: "i9", Addr: a("127.77.0.3")},
	}
	links, clashes = groupLinks(app, own, slices.Concat(want, own), others)
	got := []string{}
	for _, l := range links {
		got = append(got, formatLink(l))
	}
	if strings.Join(got, " ") != "pg,5432,"+db.Name().String()+","+dbID+",127.77.0.2 pg,8000,"+db.Name().String()+","+dbID+",127.77.0.2,group "+
		"pg,8001,"+db.Name().String()+","+dbID+",127.77.0.2,group pg,8002,"+db.Name().String()+","+dbID+",127.77.0.2,group "+
		"web2,9000,devvm-x-0123456789abc8,i9,127.77.0.3" {
		t.Errorf("with the user's links: %v", got)
	}
	if !slices.Equal(clashes, []groupClash{{Name: "web2", Target: api.Name()}}) {
		t.Errorf("clashes = %v", clashes)
	}
	added, removed := groupChanged(want, links)
	if shownLinks(added) != "pg:8000-8002" || shownLinks(removed) != "db:5432,8000-8002, web2:80" {
		t.Errorf("changed: %s; %s", shownLinks(added), shownLinks(removed))
	}

	// A member whose name is the client's own hostname, or localhost,
	// which joins refuse, gets no links there.
	twin := member(t, d, "devvm-app-fedcba98765433", "3000", "")
	local := member(t, d, "devvm-local-0123456789abc7", "3000", "localhost")
	links, clashes = groupLinks(app, nil, nil, []*state.VM{twin, local})
	if links != nil || clashes != nil {
		t.Errorf("own hostname: %v, %v", links, clashes)
	}
}

// A join's names: never localhost, never a member's name that's the
// joiner's own hostname, whatever it joins as, and never a name a member
// has, as its name or its hostname.
func TestCheckJoinName(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	db := member(t, d, "devvm-db-0123456789abc2", "", "")
	api := member(t, d, "devvm-api-0123456789abc3", "", "web2")
	members := []*state.VM{api, db}
	for _, c := range []struct {
		vm       vmname.VM
		as, want string
	}{
		{"devvm-app-0123456789abc1", "", ""},
		{"devvm-app-0123456789abc1", "pg", ""},
		// A second VM in a folder called api: api's name is web2.
		{"devvm-api-fedcba98765431", "api2", ""},
		{"devvm-app-0123456789abc1", "localhost", "group_name_host vm=devvm-app-0123456789abc1"},
		// The joiner's own hostname is a member's folder part, or the name
		// it joined with: no name it joins as helps.
		{"devvm-db-fedcba98765432", "", "group_name_hostname group=web member=" + db.Name().String() + " name=db vm=devvm-db-fedcba98765432"},
		{"devvm-db-fedcba98765432", "db2", "group_name_hostname group=web member=" + db.Name().String() + " name=db vm=devvm-db-fedcba98765432"},
		{"devvm-web2-0123456789abc5", "other", "group_name_hostname group=web member=" + api.Name().String() + " name=web2 vm=devvm-web2-0123456789abc5"},
		// Its name is a member's, or a member's own hostname.
		{"devvm-app-0123456789abc1", "db", "group_name_taken group=web member=" + db.Name().String() + " name=db"},
		{"devvm-app-0123456789abc1", "web2", "group_name_taken group=web member=" + api.Name().String() + " name=web2"},
		{"devvm-app-0123456789abc1", "api", "group_name_taken group=web member=" + api.Name().String() + " name=api"},
	} {
		if got := msg.NamesOf(checkJoinName("web", c.vm, c.as, members)); got != c.want {
			t.Errorf("checkJoinName(%s, %q) = %q; want %q", c.vm, c.as, got, c.want)
		}
	}
}

// The groups lock: another devvm waits for it under a status line, which
// ends once it has the lock, and gets it once the first lets go; one
// whose context ends first doesn't.
func TestLockGroups(t *testing.T) {
	d := paths.Dirs{Data: filepath.Join(t.TempDir(), "data")}
	unlock, err := lockGroups(t.Context(), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockGroups(ctx, d, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("while held: %v", err)
	}
	var showed msg.Recorder
	waiting, ended := make(chan struct{}), make(chan struct{})
	status := func(m msg.M) func() {
		showed.Add(m)
		close(waiting)
		return func() { close(ended) }
	}
	got := make(chan error, 1)
	go func() {
		u, err := lockGroups(t.Context(), d, status)
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("took the held lock: %v", err)
	case <-waiting:
	}
	unlock()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	default:
		t.Error("the status line didn't end")
	}
	if !slices.Equal(showed.Names(), []string{"group_waiting"}) {
		t.Errorf("showed %q", showed.Names())
	}
	if _, err := os.Stat(filepath.Join(d.Data, "groups.lock")); err != nil {
		t.Error(err)
	}
}

// A learning VM can't join a group, whose VMs share one tier, until it's
// locked: then it can.
func TestGroupJoinLearning(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "internet", Learn: true})
	if err := (Groups{e}).Join(t.Context(), "web", ""); msg.NamesOf(err) != "group_join_learning group=web vm="+st.Name().String() {
		t.Errorf("Join while learning = %v", msg.NamesOf(err))
	}
	if err := (Net{e}).Lock(t.Context(), LockOptions{None: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Groups{e}).Join(t.Context(), "web", ""); err != nil {
		t.Errorf("Join once locked = %v", msg.NamesOf(err))
	}
}

// Two VMs joining one group at once, one with network policy and one
// without: the groups lock checks one join after the other, so only one
// gets in, every time.
func TestGroupJoinsAtOnce(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	off := existingVM(t, e, Options{Network: "open"})
	e2 := *e
	wire(&e2)
	e2.Dir = filepath.Join(filepath.Dir(e.Dir), "api")
	if err := os.MkdirAll(e2.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{&e2}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := setNetwork(t.Context(), Net{&e2}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	vms, err := state.VMNames(e.Dirs)
	if err != nil || len(vms) != 2 {
		t.Fatalf("VMs %q, %v", vms, err)
	}
	for range 10 {
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, env := range []*Env{e, &e2} {
			wg.Go(func() { errs[i] = Groups{env}.Join(t.Context(), "web", "") })
		}
		wg.Wait()
		members, err := groupMembers(e.Dirs, "web")
		if err != nil {
			t.Fatal(err)
		}
		if len(members) != 1 || (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("members %d, joins %q, %q", len(members), msg.NamesOf(errs[0]), msg.NamesOf(errs[1]))
		}
		for i, env := range []*Env{e, &e2} {
			want := []string{"group_tier_mismatch group=web tier=allowlist vm=", "group_tier_mismatch group=web tier=open vm="}[i]
			if errs[i] != nil && !strings.HasPrefix(msg.NamesOf(errs[i]), want) {
				t.Errorf("join %d: %s", i, msg.NamesOf(errs[i]))
			}
			if errs[i] == nil {
				if err := (Groups{env}).Leave(t.Context()); err != nil {
					t.Fatal(msg.NamesOf(err))
				}
			}
		}
	}
	if st, _ := state.ReadVM(e.Dirs, off.Name()); get(st, KeyGroup) != "" {
		t.Errorf("still in %q", get(st, KeyGroup))
	}
}

// A link a group made applies only while the two are in that group, so
// one left behind carries nothing.
func TestCheckGroupLink(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	app := member(t, d, "devvm-app-0123456789abc1", "", "")
	db := member(t, d, "devvm-db-0123456789abc2", "5432", "")
	l := Link{Name: "db", Port: 5432, Target: db.Name(), TargetID: db.ID, Group: true}
	check := func(want string) {
		t.Helper()
		if got, _, err := CheckLink(d, app, l); err != nil || got != want {
			t.Errorf("CheckLink = %q, %v; want %q", got, err, want)
		}
	}
	check("")
	for _, g := range []string{"other", ""} {
		if err := set(t, db, KeyGroup, g); err != nil {
			t.Fatal(err)
		}
		check(LinkUngrouped)
	}
	if err := set(t, app, KeyGroup, ""); err != nil {
		t.Fatal(err)
	}
	check(LinkUngrouped)
	// The user's own link doesn't need a group.
	l.Group = false
	check("")
}

// A group's members are the VMs whose state names it.
func TestGroupMembers(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	member(t, d, "devvm-db-0123456789abc2", "", "")
	member(t, d, "devvm-app-0123456789abc1", "", "pg")
	other := member(t, d, "devvm-api-0123456789abc3", "", "")
	if err := set(t, other, KeyGroup, "other"); err != nil {
		t.Fatal(err)
	}
	ms, err := groupMembers(d, "web")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range ms {
		got = append(got, m.Name().String()+"="+memberName(m))
	}
	if strings.Join(got, " ") != "devvm-app-0123456789abc1=pg devvm-db-0123456789abc2=db" {
		t.Errorf("members = %v", got)
	}
	if ms, err := groupMembers(d, ""); ms != nil || err != nil {
		t.Errorf("no group: %v, %v", ms, err)
	}
}
