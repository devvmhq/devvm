package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

// pairOf is a unix socketpair as the broker makes it, as two frame connections: the broker's
// end and the child's.
func pairOf(t *testing.T) (broker, child *frameConn) {
	t.Helper()
	fds, err := socketpair()
	must(t, err)
	conn := func(fd int, name string) *frameConn {
		f := os.NewFile(uintptr(fd), name)
		defer f.Close()
		c, err := net.FileConn(f)
		must(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return newFrameConn(c)
	}
	return conn(fds[0], "broker"), conn(fds[1], "child")
}

// rawFrame writes a frame of body, its length first, on f's connection, as a child that a VM took
// over could.
func rawFrame(t *testing.T, f *frameConn, n uint32, body []byte) {
	t.Helper()
	_, err := f.c.Write(append(binary.BigEndian.AppendUint32(nil, n), body...))
	must(t, err)
}

// Every frame crosses as it was sent, bytes that aren't UTF-8 included, and a link's connection
// crosses as the frame's one descriptor.
func TestFramesRoundTrip(t *testing.T) {
	t.Parallel()
	b, c := pairOf(t)
	odd := []byte{0xff, 0, 'x', 0xc3}
	start := toChild{Type: frameStart, Start: childStart{
		vm: "devvm-x", password: redact.NewSecret("pw"), files: []watchedFile{{watchPolicy, "/p"}, {watchBindings, "/b"}},
		ca: odd, caKey: redact.NewSecret("key"),
	}.wire()}
	must(t, b.send(start, nil))
	var got toChild
	if fd, err := c.recv(&got); err != nil || fd != nil || !got.fields() {
		t.Fatalf("start: %v, %v, fields %v", err, fd, got.fields())
	}
	st, err := startOf(got.Start)
	if err != nil || st.vm != "devvm-x" || st.password.Reveal() != "pw" || !bytes.Equal(st.ca, odd) || st.caKey.Reveal() != "key" || len(st.files) != 2 {
		t.Errorf("start = %+v, %v", st, err)
	}
	// A request and its reply.
	must(t, c.send(toBroker{Type: frameToken, ID: 7, Token: &wireTokenReq{Host: "h", Body: odd, Header: []wireHeader{{"X", odd}}}}, nil))
	var req toBroker
	if _, err := b.recv(&req); err != nil || !req.fields() || req.ID != 7 || !bytes.Equal(req.Token.Body, odd) || !bytes.Equal(req.Token.Header[0].Value, odd) {
		t.Errorf("token request: %+v, %v", req, err)
	}
	// A descriptor crosses with its frame.
	dir := shortAgentDir(t)
	file := filepath.Join(dir, "f")
	must(t, os.WriteFile(file, []byte("through the descriptor"), 0o600))
	f, err := os.Open(file) //nolint:gosec // the test's own file
	must(t, err)
	defer f.Close()
	must(t, b.send(toChild{Type: frameLink, ID: 8, Link: &wireLink{FD: true}}, f))
	must(t, b.send(toChild{Type: frameValue, ID: 9, Value: &wireValue{Value: "v", At: time.Unix(1, 0)}}, nil))
	fd, err := c.recv(&got)
	if err != nil || fd == nil || got.ID != 8 {
		t.Fatalf("the link's reply: %v, %v, %+v", fd, err, got)
	}
	defer fd.Close()
	if text, _ := io.ReadAll(fd); string(text) != "through the descriptor" {
		t.Errorf("the descriptor reads %q", text)
	}
	if fd, err := c.recv(&got); err != nil || fd != nil || got.ID != 9 {
		t.Errorf("the frame after: %v, %v, %+v", fd, err, got)
	}
	// The other end closing: io.EOF at a frame's start.
	_ = b.close()
	if _, err := c.recv(&got); !errors.Is(err, io.EOF) {
		t.Errorf("after the close: %v", err)
	}
}

// A frame that breaks the rules is a ruleError, which names the rule alone, never the frame's
// bytes: over 2 MiB (refused before it's read), not JSON, a field its type doesn't have, more
// after the object, and two descriptors at once, whose control data is cut short.
func TestFramesBreakingTheRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		n    uint32
		body string
	}{
		{"over 2 MiB", wireMax + 1, ""},
		{"not JSON", 9, "secret-xx"},
		{"an unknown field", 33, `{"type":"value","secret-xx":true}`},
		{"more after it", 25, `{"type":"value"}secret-xx`},
	} {
		b, c := pairOf(t)
		rawFrame(t, c, tc.n, []byte(tc.body))
		var f toBroker
		_, err := b.recv(&f)
		if !isRuleError(err) || strings.Contains(err.Error(), "secret-xx") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// Two descriptors on one frame.
	b, c := pairOf(t)
	r, w, err := os.Pipe()
	must(t, err)
	defer r.Close()
	defer w.Close()
	body, _ := json.Marshal(toBroker{Type: frameValue, Value: &wireValueKey{}})
	u := c.c.(*net.UnixConn)
	_, _, err = u.WriteMsgUnix(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...), syscall.UnixRights(int(r.Fd()), int(w.Fd())), nil) //nolint:gosec // a small frame
	must(t, err)
	var f toBroker
	if fd, err := b.recv(&f); !isRuleError(err) || fd != nil {
		t.Errorf("two descriptors: %v, %v", fd, err)
	}
	// A frame that ends early is the connection's error, never a frame.
	b, c = pairOf(t)
	rawFrame(t, c, 100, []byte(`{"type"`))
	_ = c.close()
	if _, err := b.recv(&f); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut short: %v", err)
	}
}

// fields: a frame has the fields of its type alone; a reply may have its error instead, but a
// start never.
func TestFrameFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		f    toBroker
		want bool
	}{
		{toBroker{Type: frameValue, Value: &wireValueKey{}}, true},
		{toBroker{Type: frameValue}, false},
		{toBroker{Type: frameValue, Value: &wireValueKey{}, File: &wireFileReq{}}, false},
		{toBroker{Type: frameFile, Value: &wireValueKey{}}, false},
		{toBroker{Type: "other"}, false},
		{toBroker{Type: frameReady, Ready: &wireReady{}}, true},
	} {
		if got := tc.f.fields(); got != tc.want {
			t.Errorf("%+v: fields %v", tc.f, got)
		}
	}
	for _, tc := range []struct {
		f    toChild
		want bool
	}{
		{toChild{Type: frameValue, Error: "no"}, true},
		{toChild{Type: frameValue, Error: "no", Value: &wireValue{}}, false},
		{toChild{Type: frameStart, Error: "no"}, false},
		{toChild{Type: frameLink, Link: &wireLink{}}, true},
		{toChild{Type: frameLink, Start: &wireStart{}}, false},
	} {
		if got := tc.f.fields(); got != tc.want {
			t.Errorf("%+v: fields %v", tc.f, got)
		}
	}
}

// The protocol, from the child's side: whatever a child that a VM took over sends, the broker's
// read of it never panics, refuses a frame past 2 MiB without reading it, takes a frame only of a
// type the version knows with its fields alone, and answers a request only with what its own
// config names: a value of its own bindings, looked up with its own binding, a file of its own
// list, a link of its own list, and a token exchange at its own endpoints, for its own VM.
func FuzzBrokerFrames(f *testing.F) {
	for _, s := range []string{
		`{"type":"value","id":1,"value":{"kind":"secret","name":"gh"}}`,
		`{"type":"value","id":2,"value":{"kind":"login","agent":"codex","name":"codex"}}`,
		`{"type":"file","id":3,"file":{"n":0}}`, `{"type":"file","id":3,"file":{"n":-1}}`, `{"type":"file","id":3,"file":{"n":9}}`,
		`{"type":"link","id":4,"link":{"host":"db","port":5432}}`,
		`{"type":"token","id":5,"token":{"host":"auth.test","path":"/oauth/token","method":"POST","body":"Z3Q9eA=="}}`,
		`{"type":"token","id":5,"token":{"host":"auth.test","path":"/../oauth//token","method":"POST"}}`,
		`{"type":"start","id":0,"start":{}}`, `{"type":"ready","id":0,"ready":{}}`, `{}`, `[]`, `{"type":"value","id":1}`,
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	own := Binding{Name: "gh", Kind: KindSecret, Hosts: []string{"api.github.com"}, Header: "Authorization", Placeholder: "PH"}
	bindings := filepath.Join(dir, "bindings.json")
	if err := os.WriteFile(bindings, BindingList{Bindings: []Binding{own}}.Encode(), 0o600); err != nil {
		f.Fatal(err)
	}
	policy := filepath.Join(dir, "policy")
	if err := os.WriteFile(policy, []byte("example.com\n"), 0o600); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var looked []Binding
		var exchanged []Exchange
		b, err := newBroker(t.Context(), Config{
			VM: "devvm-own-0123456789abcd", Password: redact.NewSecret("pw"), Secrets: bindings, Policies: []string{policy},
			Captures: []Capture{{Agent: "codex", Host: "auth.test", Path: "/oauth/token"}},
		},
			IO{
				Secret: func(_ context.Context, got Binding) (redact.Secret, error) {
					looked = append(looked, got)
					return redact.NewSecret("v"), nil
				},
				Capture: capturerFunc(func(x Exchange) ([]byte, []byte, error) {
					exchanged = append(exchanged, x)
					return nil, []byte(`{}`), nil
				}),
			}, quiet())
		if err != nil {
			t.Fatal(err)
		}
		br, ch := pairOf(t)
		// Written as the broker reads it, so a frame larger than the socket's buffer is read too.
		wrote := make(chan struct{})
		go func() {
			defer close(wrote)
			_, _ = ch.c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)) //nolint:gosec // a fuzzed frame, at most the fuzzer's input
		}()
		defer func() { _ = ch.close(); <-wrote }()
		var fr toBroker
		if _, err := br.recv(&fr); err != nil {
			if !isRuleError(err) {
				t.Fatalf("not a rule broken: %v", err)
			}
			return
		}
		if _, ok := requestCaps()[fr.Type]; !ok || !fr.fields() {
			return // serveChild ends the pair
		}
		b.answer(t.Context(), br, fr)
		var reply toChild
		fd, err := ch.recv(&reply)
		closeFile(fd)
		if err != nil || reply.ID != fr.ID || reply.Type != fr.Type || !reply.fields() {
			t.Fatalf("the reply: %+v, %v", reply, err)
		}
		for _, l := range looked {
			if !reflect.DeepEqual(l, own) {
				t.Fatalf("looked up %+v, not the broker's own binding", l)
			}
		}
		for _, x := range exchanged {
			if x.VM != "devvm-own-0123456789abcd" || len(x.Agents) != 1 || x.Agents[0] != "codex" {
				t.Fatalf("exchanged %+v", x)
			}
		}
		if fr.Type == frameLink && reply.Error == "" {
			t.Fatalf("a link the broker's config has none of: %+v", reply)
		}
		if fr.Type == frameFile && reply.Error == "" && reply.File != nil && !reply.File.Missing &&
			string(reply.File.Data) != "example.com\n" && !bytes.Contains(reply.File.Data, []byte(`"gh"`)) {
			t.Fatalf("a file outside the broker's list: %q", reply.File.Data)
		}
	})
}

// framesOf is frames, each its length and its bytes, as one stream.
func framesOf(frames ...string) []byte {
	var out []byte
	for _, f := range frames {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) //nolint:gosec // a seed's frame, short
		out = append(out, f...)
	}
	return out
}

// Streams of frames, whatever their bytes, through both readers: the broker's serveChild, which
// answers each request with a frame of its ID and type and a code of the protocol's alone, and
// ends at the first frame that breaks the rules or at the stream's end; and the child's
// readReplies, which takes no reply to a request it never sent, nor a second start, and ends the
// same way. Neither panics or hangs.
func FuzzFrameStreams(f *testing.F) {
	f.Add(framesOf(`{"type":"value","id":1,"value":{"kind":"secret","name":"x"}}`, `{"type":"file","id":2,"file":{"n":0}}`), true)
	f.Add(framesOf(`{"type":"link","id":3,"link":{"host":"db","port":1}}`, `{"type":"token","id":4,"token":{"host":"h","path":"/","method":"POST"}}`, `{"type":"ready","id":5,"ready":{}}`), true)
	f.Add(framesOf(`{"type":"value","id":1,"value":{"value":"v","at":"2026-01-01T00:00:00Z"}}`), false)
	f.Add(framesOf(`{"type":"start","id":0,"start":{}}`, `{"type":"file","id":9,"error":"failed"}`), false)
	f.Add([]byte{0, 0, 0, 2, '{', '}', 0xff}, true)
	codes := map[string]bool{"": true, codeNotFound: true, codeRefused: true, codeBusy: true, codeTooLong: true, codeFailed: true}
	f.Fuzz(func(t *testing.T, stream []byte, toBrokerSide bool) {
		a, b := pairOf(t)
		wrote := make(chan struct{})
		go func() {
			defer close(wrote)
			_, _ = b.c.Write(stream)
			_ = b.c.(*net.UnixConn).CloseWrite()
		}()
		defer func() { _ = b.close(); <-wrote }()
		if !toBrokerSide {
			client := newBrokerClient(a)
			if err := client.read(); err != nil && !isRuleError(err) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("the child's read: %v", err)
			}
			return
		}
		replies := make(chan error, 1)
		go func() {
			for {
				var r toChild
				fd, err := b.recv(&r)
				closeFile(fd)
				if err != nil {
					replies <- nil
					return
				}
				if !r.fields() || !codes[r.Error] {
					replies <- fmt.Errorf("a reply %+v", r)
					return
				}
			}
		}()
		br, err := newBroker(t.Context(), Config{VM: "devvm-x", Password: redact.NewSecret("pw")}, IO{}, quiet())
		if err != nil {
			t.Fatal(err)
		}
		if err := br.serveChild(t.Context(), a); err != nil && !isRuleError(err) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("serveChild: %v", err)
		}
		time.Sleep(time.Millisecond) // the answers under way
		_ = a.close()
		if err := <-replies; err != nil {
			t.Fatal(err)
		}
	})
}
