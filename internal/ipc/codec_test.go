package ipc

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrameRoundtrip(t *testing.T) {
	var buf bytes.Buffer

	fw := NewFrameWriter(&buf)
	require.NoError(t, fw.WriteFrame([]byte(`{"hello":"world"}`)))
	require.NoError(t, fw.WriteFrame([]byte(`{"n":2}`)))

	fr := NewFrameReader(&buf)
	a, err := fr.ReadFrame()
	require.NoError(t, err)
	assert.JSONEq(t, `{"hello":"world"}`, string(a))

	b, err := fr.ReadFrame()
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":2}`, string(b))
}

func TestFrameEOFAfterLastFrame(t *testing.T) {
	var buf bytes.Buffer

	fw := NewFrameWriter(&buf)
	require.NoError(t, fw.WriteFrame([]byte("x")))

	fr := NewFrameReader(&buf)
	_, err := fr.ReadFrame()
	require.NoError(t, err)
	_, err = fr.ReadFrame()
	assert.ErrorIs(t, err, io.EOF)
}

func TestFrameMalformedHeader(t *testing.T) {
	fr := NewFrameReader(strings.NewReader("not-a-header\r\n\r\nbody"))
	_, err := fr.ReadFrame()
	assert.Error(t, err)
}

func TestFrameOversizedRejected(t *testing.T) {
	hdr := "Content-Length: 999999999\r\n\r\n"
	fr := NewFrameReader(strings.NewReader(hdr))
	_, err := fr.ReadFrame()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFrameTooLarge)
}

func TestFrameNegativeLength(t *testing.T) {
	fr := NewFrameReader(strings.NewReader("Content-Length: -1\r\n\r\n"))
	_, err := fr.ReadFrame()
	assert.Error(t, err)
}

func TestFrameMultipleHeaders(t *testing.T) {
	payload := []byte(`{"k":"v"}`)
	hdr := "Content-Length: 9\r\nContent-Type: application/vnd.jsonrpc;charset=utf-8\r\n\r\n"
	fr := NewFrameReader(strings.NewReader(hdr + string(payload)))
	got, err := fr.ReadFrame()
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}
