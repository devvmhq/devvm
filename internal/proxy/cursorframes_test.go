package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// frame is one enveloped message as Connect and gRPC send them: flags, a big-endian length and
// the message, gzipped first when its flags say so.
func frame(t testing.TB, flags byte, msg string) []byte {
	t.Helper()
	data := []byte(msg)
	if flags&frameCompressed != 0 {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		if _, err := zw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		data = b.Bytes()
	}
	out := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(data))) //nolint:gosec // G115: a test's small message
	return append(out, data...)
}

// stream is a reply from a Cursor host with body and headers, as the forwarder has it.
func stream(body []byte, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	req, _ := http.NewRequest(http.MethodPost, "https://agentn.global.api5.cursor.sh/agent.v1.AgentService/Run", nil) //nolint:noctx // a test's request, never sent
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: req, Trailer: http.Header{}}
}

// read passes a stream's reply, body and headers, through cursorReply and reads its body a byte
// at a time, the hardest split.
func read(t *testing.T, body []byte, header ...string) ([]byte, error) {
	t.Helper()
	resp := stream(body, header...)
	defer func() { _ = resp.Body.Close() }()
	s := &server{captures: newCaptures(nil, nil)}
	if err := s.cursorReply(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(iotest.OneByteReader(resp.Body))
}

// Cursor's agent's Run, a Connect stream whose messages come gzipped (Connect-Content-Encoding)
// with an end of stream, passes as it came; so does gRPC's. A message holding a JWT, in any of
// them and compressed or not, ends the stream before it, the messages before it passed whole; so
// do the trailers of gRPC holding one, and a stream the proxy can't read: a flag it doesn't know,
// a message over frameMax, a compressed message without gzip or that isn't gzip, an envelope cut
// off, and a message encoding but gzip.
func TestCursorStreams(t *testing.T) {
	const connect, gzipEnc = "application/connect+proto", "gzip"
	clean := bytes.Join([][]byte{
		frame(t, frameCompressed, "\x0a\x05hello"), frame(t, 0, "\x12\x02ok"), frame(t, frameEnd, `{"metadata":{"x":["1"]}}`),
	}, nil)
	got, err := read(t, clean, "Content-Type", connect, "Connect-Content-Encoding", gzipEnc)
	if err != nil || !bytes.Equal(got, clean) {
		t.Fatalf("a Connect stream: %q, %v", got, err)
	}
	grpcBody := clean[:len(clean)-len(frame(t, frameEnd, `{"metadata":{"x":["1"]}}`))]
	if got, err := read(t, grpcBody, "Content-Type", "application/grpc", "Grpc-Encoding", gzipEnc); err != nil || len(got) == 0 {
		t.Errorf("a gRPC stream: %q, %v", got, err)
	}
	first := frame(t, frameCompressed, "\x0a\x05hello")
	for name, tc := range map[string]struct {
		body   []byte
		header []string
	}{
		"a token, compressed": {append(append([]byte{}, first...), frame(t, frameCompressed, cursorJWT)...), nil},
		// Text that deflate codes, so the token's bytes aren't in the message as it came.
		"a token, deflated":             {append(append([]byte{}, first...), frame(t, frameCompressed, strings.Repeat("hello world ", 50)+cursorJWT)...), nil},
		"a token, plain":                {append(append([]byte{}, first...), frame(t, 0, cursorJWT)...), nil},
		"a token at the end":            {append(append([]byte{}, first...), frame(t, frameEnd, `{"metadata":{"t":["`+cursorJWT[2:54]+`"]}}`)...), nil},
		"a token at the end, escaped":   {append(append([]byte{}, first...), frame(t, frameEnd|frameCompressed, `{"error":{"message":"eyJ`+cursorJWT[5:54]+`"}}`)...), nil},
		"an end that isn't JSON":        {append(append([]byte{}, first...), frame(t, frameEnd, `{"metadata"`)...), nil},
		"a flag it doesn't know":        {append(append([]byte{}, first...), frame(t, 0x80, "x")...), nil},
		"over frameMax":                 {append(append([]byte{}, first...), 0, 0xff, 0xff, 0xff, 0xff), nil},
		"an envelope cut off":           {append(append([]byte{}, first...), 0, 0, 0), nil},
		"a message cut off":             {append(append([]byte{}, first...), 0, 0, 0, 0, 9, 'x'), nil},
		"not gzip":                      {append(append([]byte{}, first...), 1, 0, 0, 0, 3, 'x', 'y', 'z'), nil},
		"compressed, encoding identity": {first, []string{"Content-Type", connect, "Connect-Content-Encoding", "identity"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.header
			if h == nil {
				h = []string{"Content-Type", connect, "Connect-Content-Encoding", gzipEnc}
			}
			got, err := read(t, tc.body, h...)
			if !errors.Is(err, errCaptureRefused) {
				t.Fatalf("read %q, %v", got, err)
			}
			if strings.Contains(string(got), "real-signature") || strings.Contains(string(got), "eyJzdWIi") {
				t.Errorf("the token passed: %q", got)
			}
			if name != "compressed, encoding identity" && !bytes.HasPrefix(got, first) {
				t.Errorf("the message before it didn't pass whole: %q", got)
			}
		})
	}
	// gRPC's trailers, filled in once its body ends.
	resp := stream(frame(t, 0, "\x0a\x02hi"), "Content-Type", "application/grpc")
	defer func() { _ = resp.Body.Close() }()
	s := &server{captures: newCaptures(nil, nil)}
	if err := s.cursorReply(resp); err != nil {
		t.Fatal(err)
	}
	resp.Trailer.Set("Grpc-Status-Details-Bin", cursorJWT[2:54])
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, errCaptureRefused) {
		t.Errorf("a token in gRPC's trailers: %v", err)
	}
	// A stream in JSON has each message checked as it decodes, escapes undone.
	jsonStream := append(frame(t, 0, `{"a":"eyJ`+cursorJWT[5:54]+`"}`), frame(t, frameEnd, `{}`)...)
	if got, err := read(t, jsonStream, "Content-Type", "application/connect+json"); !errors.Is(err, errCaptureRefused) || len(got) != 0 {
		t.Errorf("a JSON stream with an escaped token: %q, %v", got, err)
	}
	// A message encoding the proxy can't read, on a stream or not, and gRPC-Web's base64 text,
	// refused before any of it.
	for _, h := range [][]string{
		{"Content-Type", connect, "Connect-Content-Encoding", "br"},
		{"Content-Type", "application/grpc", "Grpc-Encoding", "snappy"},
		{"Content-Type", connect, "Grpc-Encoding", gzipEnc},
		{"Content-Type", "application/proto", "Connect-Content-Encoding", gzipEnc},
		{"Content-Type", "application/grpc-web-text+proto"},
	} {
		if _, err := read(t, clean, h...); !errors.Is(err, errCaptureRefused) {
			t.Errorf("%v: %v", h, err)
		}
	}
}

// FuzzCursorFrames: a reply from a Cursor host, of any type, encodings, body and trailers, read
// in reads of any size, passes only what came, from its start, or is refused before any of it. A
// stream passes whole messages alone, none over frameMax, compressed or as it decompresses, and
// none holding a JWT's start once decompressed and, in JSON, with its escapes undone; any other
// body passes no JWT's start of up to jwtWindow bytes; and a trailer holding one never lets the
// body end cleanly.
func FuzzCursorFrames(f *testing.F) {
	const connect, grpc = "application/connect+proto", "application/grpc"
	hello, end := frame(f, frameCompressed, "\x0a\x05hello"), frame(f, frameEnd, `{"metadata":{"x":["1"]}}`)
	clean := bytes.Join([][]byte{hello, frame(f, 0, "\x12\x02ok"), end}, nil)
	// A message that decompresses to one byte past frameMax: a gzip bomb, small as it comes.
	bomb := frame(f, frameCompressed, strings.Repeat("\x00", frameMax+1))
	for _, s := range []struct {
		ctype, cenc, genc, trailer string
		body                       []byte
	}{
		{connect, "gzip", "", "", clean},
		{grpc, "", "gzip", "", clean[:len(clean)-len(end)]},
		{grpc, "", "", cursorJWT, frame(f, 0, "\x0a\x02hi")},
		{connect, "gzip", "", "", append(bytes.Clone(hello), frame(f, frameCompressed, cursorJWT)...)},
		{"application/connect+json", "", "", "", append(frame(f, 0, `{"a":"eyJ`+cursorJWT[5:54]+`"}`), frame(f, frameEnd, `{}`)...)},
		{connect, "gzip", "", "", bomb},
		{connect, "gzip", "", "", frame(f, frameCompressed, strings.Repeat("hello world ", 50)+cursorJWT)},
		{connect, "identity", "", "", append(bytes.Clone(hello), 0, 0xff, 0xff, 0xff, 0xff)},
		{"application/proto", "", "", "", []byte(strings.Repeat("x", 100) + cursorJWT)},
		{"application/json", "", "", "", []byte(`{"token":"` + cursorJWT[2:54] + `"}`)},
	} {
		f.Add(s.ctype, s.cenc, s.genc, s.trailer, s.body, uint16(1))
		f.Add(s.ctype, s.cenc, s.genc, s.trailer, s.body, uint16(4096))
	}
	f.Fuzz(func(t *testing.T, ctype, cenc, genc, trailer string, body []byte, chunk uint16) {
		resp := stream(body)
		for k, v := range map[string]string{"Content-Type": ctype, "Connect-Content-Encoding": cenc, "Grpc-Encoding": genc} {
			if v != "" {
				resp.Header.Set(k, v)
			}
		}
		s := &server{captures: newCaptures(nil, nil)}
		if s.cursorReply(resp) != nil {
			return // refused, before any of it
		}
		// Trailers come once the body ends.
		resp.Trailer.Set("X-Trailer", trailer)
		got, err := readIn(resp.Body, int(chunk)%(64<<10)+1)
		_ = resp.Body.Close()
		switch {
		case !bytes.HasPrefix(body, got):
			t.Fatalf("passed %q, which isn't the start of %q", got, body)
		case err == nil && jwtStart.MatchString(trailer):
			t.Fatalf("a trailer holding a JWT's start, %q, and the body ended cleanly", trailer)
		}
		kind, isJSON := streamKind(ctype)
		if kind == "" {
			for _, m := range jwtStart.FindAllIndex(got, -1) {
				if m[1]-m[0] <= jwtWindow {
					t.Fatalf("a JWT's start passed: %q", got[m[0]:m[1]])
				}
			}
			return
		}
		checkFrames(t, got, kind == "grpc", isJSON)
	})
}

// readIn reads r whole in reads of size n, returning what came before any error but io.EOF.
func readIn(r io.Reader, n int) ([]byte, error) {
	var out []byte
	buf := make([]byte, n)
	for {
		k, err := r.Read(buf)
		out = append(out, buf[:k]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// checkFrames reads the messages a stream passed, b, apart again, as a client would: each one
// whole, of a flag the stream's protocol has, at most frameMax as it came and as it decompresses,
// and without a JWT's start, also, for one in JSON (the stream's every message, or Connect's end),
// once its escapes are undone.
func checkFrames(t *testing.T, b []byte, grpc, isJSON bool) {
	t.Helper()
	known := byte(frameCompressed)
	if !grpc {
		known |= frameEnd
	}
	for len(b) > 0 {
		if len(b) < 5 {
			t.Fatalf("an envelope cut off passed: %q", b)
		}
		flags, size := b[0], binary.BigEndian.Uint32(b[1:5])
		if flags&^known != 0 || size > frameMax || len(b)-5 < int(size) {
			t.Fatalf("a message flagged %#x of %d bytes passed, with %d left", flags, size, len(b)-5)
		}
		plain := b[5 : 5+size]
		b = b[5+size:]
		if flags&frameCompressed != 0 {
			zr, err := gzip.NewReader(bytes.NewReader(plain))
			if err != nil {
				t.Fatalf("a compressed message that isn't gzip passed: %v", err)
			}
			if plain, err = io.ReadAll(io.LimitReader(zr, frameMax+1)); err != nil || len(plain) > frameMax {
				t.Fatalf("a compressed message passed that decompresses to %d bytes: %v", len(plain), err)
			}
		}
		if jwtStart.Match(plain) {
			t.Fatalf("a message holding a JWT's start passed: %q", plain)
		}
		if !isJSON && (grpc || flags&frameEnd == 0) {
			continue
		}
		var v any
		if err := json.Unmarshal(plain, &v); err != nil {
			t.Fatalf("a JSON message that isn't JSON passed: %q", plain)
		}
		var out bytes.Buffer
		e := json.NewEncoder(&out)
		e.SetEscapeHTML(false)
		if err := e.Encode(v); err != nil || jwtStart.Match(out.Bytes()) {
			t.Fatalf("a JSON message holding a JWT's start passed: %q (%v)", out.Bytes(), err)
		}
	}
}
