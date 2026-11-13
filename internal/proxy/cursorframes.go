package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
)

// A Connect or gRPC stream from a host of Cursor's is a run of messages, each in an envelope: a
// flags byte, a 4-byte big-endian length and the message, which the flags' lowest bit says is
// compressed with the reply's message encoding (Connect-Content-Encoding, or gRPC's
// Grpc-Encoding). Connect's last message, its end of stream (flag 0x02), is JSON with the
// stream's error and trailers. Cursor's agent streams its replies this way, and its messages may
// come compressed, gzip being the one encoding Connect's and gRPC's clients all take, so the
// proxy reads each message whole, as it would reach the VM, decompressed, before passing it on.
const (
	frameCompressed = 0x01
	frameEnd        = 0x02
	// frameMax is the most a message holds, compressed or as it decompresses: one message at a
	// time, so this bounds what the proxy keeps of a stream.
	frameMax = 16 << 20
)

// streamKind is what a reply's Content-Type says its body is: Connect's streaming or gRPC's
// enveloped messages, or neither (""), and whether those messages are JSON. gRPC-Web's text
// variant, a whole body in base64 that no scanner reads, is "unreadable".
func streamKind(contentType string) (kind string, isJSON bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	switch {
	case err != nil:
		return "", false
	case strings.HasPrefix(mt, "application/grpc-web-text"):
		return "unreadable", false
	case strings.HasPrefix(mt, "application/connect+"):
		return "connect", mt == "application/connect+json"
	case mt == "application/grpc" || strings.HasPrefix(mt, "application/grpc+"):
		return "grpc", mt == "application/grpc+json"
	}
	return "", false
}

// frameScanner is a stream's body that passes message by message, each read whole (at most
// frameMax), decompressed when its flag says so, and checked for a JWT's start (jwtStart) before
// any of it passes, as it came; a message holding one, one the proxy can't read (a flag it doesn't
// know, a compressed one without gzip, one over frameMax, a cut-off envelope) and trailers holding
// one (tokenIn) end the stream there, so ReverseProxy passes nothing more.
type frameScanner struct {
	r       io.ReadCloser
	grpc    bool         // gRPC's envelopes, whose flags have no end of stream
	gzip    bool         // the reply's messages that are compressed are gzip
	json    bool         // every message is JSON, checked as it decodes
	trailer *http.Header // the reply's trailers, filled in once the body ends
	out     []byte       // what of the message read last hasn't passed yet
	err     error
}

func (f *frameScanner) refuse(why string) error {
	f.err = fmt.Errorf("%w: %s in a stream from a host of Cursor's", errCaptureRefused, why)
	return f.err
}

// Read passes what's left of the message read last, and when that's gone reads the next one
// (next).
func (f *frameScanner) Read(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if len(f.out) == 0 {
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// next reads the next message and checks it, ending the stream at its end or at a refusal.
func (f *frameScanner) next() error {
	var head [5]byte
	if _, err := io.ReadFull(f.r, head[:]); err != nil {
		switch {
		case errors.Is(err, io.EOF) && f.trailer != nil && tokenIn(*f.trailer):
			return f.refuse("a token in a trailer")
		case errors.Is(err, io.EOF):
			f.err = io.EOF
			return io.EOF
		case errors.Is(err, io.ErrUnexpectedEOF):
			return f.refuse("an envelope cut off")
		}
		f.err = err
		return err
	}
	flags, size := head[0], binary.BigEndian.Uint32(head[1:])
	known := frameCompressed
	if !f.grpc {
		known |= frameEnd
	}
	switch {
	case flags&^byte(known) != 0:
		return f.refuse(fmt.Sprintf("a message flagged %#x", flags))
	case size > frameMax:
		return f.refuse("a message over the proxy's limit")
	}
	// The message as it passes, its envelope first, grown as its bytes come rather than as its
	// length says.
	var buf bytes.Buffer
	buf.Write(head[:])
	if n, err := io.CopyN(&buf, f.r, int64(size)); err != nil || n != int64(size) {
		return f.refuse("a message cut off")
	}
	msg := buf.Bytes()[len(head):]
	plain := msg
	if flags&frameCompressed != 0 {
		if !f.gzip {
			return f.refuse("a compressed message without gzip")
		}
		zr, err := gzip.NewReader(bytes.NewReader(msg))
		if err != nil {
			return f.refuse("a compressed message that isn't gzip")
		}
		plain, err = io.ReadAll(io.LimitReader(zr, frameMax+1))
		if err != nil || len(plain) > frameMax {
			return f.refuse("a compressed message the proxy can't read whole")
		}
	}
	if jwtStart.Match(plain) {
		return f.refuse("a token")
	}
	// Connect's end of stream is JSON, whose escapes (eyJ…) would hide a token from the bytes
	// alone: its strings are checked as the VM's client reads them, and one that isn't JSON ends it.
	// So is every message of a stream in JSON (application/connect+json, or gRPC's +json).
	if (!f.grpc && flags&frameEnd != 0) || f.json {
		var v any
		if json.Unmarshal(plain, &v) != nil {
			return f.refuse("a JSON message that isn't JSON")
		}
		if jsonHoldsToken(v) {
			return f.refuse("a token")
		}
	}
	f.out = buf.Bytes()
	return nil
}

// jsonHoldsToken reports whether any string in v, a decoded JSON value, keys included, holds a
// JWT's start.
func jsonHoldsToken(v any) bool {
	switch x := v.(type) {
	case string:
		return jwtStart.MatchString(x)
	case []any:
		return slices.ContainsFunc(x, jsonHoldsToken)
	case map[string]any:
		for k, e := range x {
			if jwtStart.MatchString(k) || jsonHoldsToken(e) {
				return true
			}
		}
	}
	return false
}

func (f *frameScanner) Close() error { return f.r.Close() }
