package proxy

// The broker's protocol: what devvm _proxy, the broker, and devvm _proxy-child, the child that
// reads the VM's network, say to each other over the one socketpair between them (broker.go,
// child.go). Each message is a frame: a 4-byte big-endian length, then a JSON object of at most
// wireMax, its type, its id and its fields, decoded refusing a field its type doesn't have. Bytes
// (a file, a body, a header's value) cross as []byte, which JSON holds in base64, so no byte that
// isn't UTF-8 changes on the way. A frame may carry one descriptor (SCM_RIGHTS), on its first
// bytes: a link's connection, from the broker alone. Every read is a recvmsg with room for one
// descriptor, asking for no more than what's left of its frame, so a descriptor always arrives with
// the frame it belongs to, and a read whose control data was cut short breaks the rules.
//
// The broker sends start first; the child answers ready, or failed. After that only the child
// asks (value, file, link, token), and the broker answers each with a frame of the same id, in any
// order. Nothing logs a frame: an error names only its type and the rule it broke. A secret's value
// crosses as a string in a wire struct, which only the encoder fills (Reveal) and the decoder makes
// a redact.Secret at once (where secret values live, in design.md).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/vmname"
)

// protocolVersion is the protocol's version: a start or a ready of another fails the start.
const protocolVersion = 1

// wireMax bounds a frame's JSON: a watched file of 1 MiB in base64, with room to spare.
const wireMax = 2 << 20

// The frames' types.
const (
	frameStart  = "start"
	frameReady  = "ready"
	frameFailed = "failed"
	frameValue  = "value"
	frameFile   = "file"
	frameLink   = "link"
	frameToken  = "token"
)

// errFrameTooLong is send's error for a frame past wireMax, which it never sends.
var errFrameTooLong = errors.New("a frame past 2 MiB")

// A ruleError is a frame that broke the protocol's rules: the pair ends, and the error names the
// rule alone, never the frame's bytes.
type ruleError struct{ rule string }

func (e ruleError) Error() string { return "the broker's protocol: " + e.rule }

// broke is the ruleError of rule.
func broke(rule string) error { return ruleError{rule} }

// isRuleError reports whether err is a frame that broke the rules.
func isRuleError(err error) bool {
	var r ruleError
	return errors.As(err, &r)
}

// frameConn is one end of the socketpair: frames written whole under one lock, each with at most
// one descriptor on its first bytes, and read one at a time. Over a connection that isn't a unix
// socket (a test's in memory) no descriptor crosses.
type frameConn struct {
	c   net.Conn
	wmu sync.Mutex
	// oob is the room each read has for one descriptor's control message.
	oob []byte
}

func newFrameConn(c net.Conn) *frameConn {
	return &frameConn{c: c, oob: make([]byte, syscall.CmsgSpace(4))}
}

// send writes v as one frame, with fd, when it isn't nil, on its first bytes. A frame past
// wireMax isn't sent.
func (f *frameConn) send(v any, fd *os.File) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > wireMax {
		return fmt.Errorf("%w: %d bytes", errFrameTooLong, len(b))
	}
	buf := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b))) //nolint:gosec // at most wireMax
	buf = append(buf, b...)
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if fd != nil {
		u, ok := f.c.(*net.UnixConn)
		if !ok {
			return errors.New("a descriptor needs a unix socket")
		}
		n, _, err := u.WriteMsgUnix(buf, syscall.UnixRights(int(fd.Fd())), nil)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	_, err = f.c.Write(buf)
	return err
}

// recv reads one frame, into v, refusing a field v's type doesn't have, and the descriptor that
// came with it, nil for none: its caller closes it, or refuses it. An error that is a ruleError
// broke the rules; any other is the connection's (io.EOF once the other end has closed it).
func (f *frameConn) recv(v any) (*os.File, error) {
	var hdr [4]byte
	var fds []int
	defer func() { closeFDs(fds) }()
	if err := f.read(hdr[:], &fds, true); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > wireMax {
		return nil, broke("a frame over 2 MiB")
	}
	body := make([]byte, n)
	if err := f.read(body, &fds, false); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(fds) > 1 {
		return nil, broke("a frame with more than one descriptor")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil || d.More() {
		return nil, broke("a frame that isn't one of version 1's")
	}
	if len(fds) == 0 {
		return nil, nil
	}
	fd := os.NewFile(uintptr(fds[0]), "descriptor")
	fds = nil
	return fd, nil
}

// read fills b, the rest of a frame, asking no read for more than what's left, and keeps the
// descriptors that came with it in fds. first: b is a frame's start, where the other end closing
// before a byte is io.EOF.
func (f *frameConn) read(b []byte, fds *[]int, first bool) error {
	u, unix := f.c.(*net.UnixConn)
	got := 0
	for got < len(b) {
		var n, oobn, flags int
		var err error
		if unix {
			n, oobn, flags, _, err = u.ReadMsgUnix(b[got:], f.oob)
		} else {
			n, err = f.c.Read(b[got:])
		}
		if flags&syscall.MSG_CTRUNC != 0 {
			return broke("control data cut short")
		}
		if oobn > 0 {
			rights, perr := unixRights(f.oob[:oobn])
			*fds = append(*fds, rights...)
			if perr != nil {
				return broke("control data that isn't descriptors")
			}
		}
		got += n
		if err != nil {
			if errors.Is(err, io.EOF) && (got > 0 || !first) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 && oobn == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// unixRights is the descriptors in oob, a read's control messages.
func unixRights(oob []byte) ([]int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for i := range msgs {
		got, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			return fds, err
		}
		for _, fd := range got {
			syscall.CloseOnExec(fd)
		}
		fds = append(fds, got...)
	}
	return fds, nil
}

// closeFDs closes descriptors a frame brought that nothing took.
func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

// close closes the connection: the other end reads its end.
func (f *frameConn) close() error { return f.c.Close() }

// --- what the broker sends: start, then replies ---

// toChild is a frame the broker sends: start, or the reply to the child's request of the same ID
// and type, with its fields, or Error, which refuses it.
type toChild struct {
	Type  string          `json:"type"`
	ID    uint64          `json:"id"`
	Start *wireStart      `json:"start,omitempty"`
	Value *wireValue      `json:"value,omitempty"`
	File  *wireFile       `json:"file,omitempty"`
	Link  *wireLink       `json:"link,omitempty"`
	Token *wireTokenReply `json:"token,omitempty"`
	Error string          `json:"error,omitempty"`
}

// toBroker is a frame the child sends: ready or failed, then requests, each with an ID of its own.
type toBroker struct {
	Type   string        `json:"type"`
	ID     uint64        `json:"id"`
	Ready  *wireReady    `json:"ready,omitempty"`
	Failed *wireFailed   `json:"failed,omitempty"`
	Value  *wireValueKey `json:"value,omitempty"`
	File   *wireFileReq  `json:"file,omitempty"`
	Link   *wireLinkReq  `json:"link,omitempty"`
	Token  *wireTokenReq `json:"token,omitempty"`
}

// fields reports whether f has the fields of its type alone: the ones a frame of it must have,
// and none of another's.
func (f toBroker) fields() bool {
	set := map[string]bool{
		frameReady: f.Ready != nil, frameFailed: f.Failed != nil, frameValue: f.Value != nil,
		frameFile: f.File != nil, frameLink: f.Link != nil, frameToken: f.Token != nil,
	}
	return onlyField(set, f.Type)
}

// fields reports whether f has the fields of its type alone, or, for a reply, its Error and none.
func (f toChild) fields() bool {
	set := map[string]bool{
		frameStart: f.Start != nil, frameValue: f.Value != nil, frameFile: f.File != nil,
		frameLink: f.Link != nil, frameToken: f.Token != nil,
	}
	if f.Error != "" {
		return f.Type != frameStart && onlyField(set, "")
	}
	return onlyField(set, f.Type)
}

// onlyField reports whether set marks typ alone ("" for none), and typ is one of its keys.
func onlyField(set map[string]bool, typ string) bool {
	if _, ok := set[typ]; !ok && typ != "" {
		return false
	}
	for k, v := range set {
		if v != (k == typ) {
			return false
		}
	}
	return true
}

// wireStart is everything the child runs with: its VM's config as the broker read it, without the
// paths the child never opens, the CAs and the company proxy's password the broker read, and the
// watched files the child looks at (stat) and asks the broker for by their number. Password,
// CAKey, CANextKey and UpstreamPassword are secrets, filled by childStart.wire alone.
type wireStart struct {
	Version int    `json:"version"`
	Exe     string `json:"exe"` // the broker's executable's SHA-256, in hex
	// Spawned is how the broker started the child (Confinement.String): under a sandbox, or
	// unconfined and why.
	Spawned string    `json:"spawned"`
	VM      vmname.VM `json:"vm"`
	// Password is the VM's password: secret.
	Password        string      `json:"password"`
	BuiltinPolicies []string    `json:"builtin_policies,omitempty"`
	Files           []wireWatch `json:"files,omitempty"`
	Open            bool        `json:"open,omitempty"`
	Log             string      `json:"log,omitempty"`
	ProxyLog        string      `json:"proxy_log,omitempty"`
	CA              []byte      `json:"ca,omitempty"`
	// CAKey and CANextKey are the CAs' keys (PEM): secret.
	CAKey       string    `json:"ca_key,omitempty"`
	CANext      []byte    `json:"ca_next,omitempty"`
	CANextKey   string    `json:"ca_next_key,omitempty"`
	Captures    []Capture `json:"captures,omitempty"`
	UpstreamCAs string    `json:"upstream_cas,omitempty"`
	Upstream    *Upstream `json:"upstream,omitempty"`
	// UpstreamPassword is the value of Upstream.Secret, the company proxy's password: secret.
	UpstreamPassword   string `json:"upstream_password,omitempty"`
	UpstreamUnresolved bool   `json:"upstream_unresolved,omitempty"`
	NixCache           string `json:"nix_cache,omitempty"`
	NixCacheMax        int64  `json:"nix_cache_max,omitempty"`
	LinkSockets        string `json:"link_sockets,omitempty"`
}

// wireWatch is one of the files the child looks at and asks for (watchedFile): its kind and path.
type wireWatch struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// wireReady is the child's answer to start: the protocol's version, its own executable's SHA-256,
// the address it serves and how it's confined (Confinement.String).
type wireReady struct {
	Version  int    `json:"version"`
	Exe      string `json:"exe"`
	Addr     string `json:"addr"`
	Confined string `json:"confined"`
}

// wireFailed is the child's answer to a start it can't run with: why.
type wireFailed struct {
	Why string `json:"why"`
}

// wireValueKey asks for a binding's value by its key (valueKey).
type wireValueKey struct {
	Kind  string `json:"kind"`
	Agent string `json:"agent,omitempty"`
	Name  string `json:"name"`
}

// wireValue is a value and when the broker looked it up. Value is secret.
type wireValue struct {
	Value string    `json:"value"`
	At    time.Time `json:"at"`
}

// wireFileReq asks for one of start's Files, by its number.
type wireFileReq struct {
	N int `json:"n"`
}

// wireFile is a watched file the broker read: its bytes and its stamp, or Missing.
type wireFile struct {
	Data    []byte    `json:"data,omitempty"`
	MTime   time.Time `json:"mtime"`
	Size    int64     `json:"size"`
	Ino     uint64    `json:"ino"`
	Missing bool      `json:"missing,omitempty"`
}

// wireLinkReq asks for a connection to the link a request's host and port name.
type wireLinkReq struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// wireLink is a link's reply: FD, the frame carries the connection.
type wireLink struct {
	FD bool `json:"fd"`
}

// wireHeader is one of a header's values.
type wireHeader struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

// wireTokenReq is a captured token request: its host, its endpoint's clean path, its method and
// query, its headers and its body.
type wireTokenReq struct {
	Host   string       `json:"host"`
	Path   string       `json:"path"`
	Method string       `json:"method"`
	Query  string       `json:"query,omitempty"`
	Header []wireHeader `json:"header,omitempty"`
	Body   []byte       `json:"body,omitempty"`
}

// wireTokenReply is what the VM gets for a captured token request: its status, its few headers
// and its body, the agent whose login it saved ("" for none), and whether it went to the endpoint
// (Sent), rather than IO.Capture answering it.
type wireTokenReply struct {
	Status int          `json:"status"`
	Header []wireHeader `json:"header,omitempty"`
	Body   []byte       `json:"body,omitempty"`
	Agent  string       `json:"agent,omitempty"`
	Sent   bool         `json:"sent,omitempty"`
}
