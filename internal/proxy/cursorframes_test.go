package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// frame is one enveloped message as Connect and gRPC send them: flags, a big-endian length and
// the message, gzipped first when its flags say so.
func frame(t *testing.T, flags byte, msg string) []byte {
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
		"a token, compressed":           {append(append([]byte{}, first...), frame(t, frameCompressed, cursorJWT)...), nil},
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
