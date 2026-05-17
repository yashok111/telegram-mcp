package ipc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// MaxFrameSize caps a single payload at 64 MiB. Photos go via file paths,
// not over the wire; this only needs to fit JSON envelopes.
const MaxFrameSize = 64 * 1024 * 1024

// ErrFrameTooLarge is returned when a Content-Length header exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("frame exceeds MaxFrameSize")

// FrameReader reads Content-Length framed payloads from an io.Reader.
// Not safe for concurrent use.
type FrameReader struct {
	br *bufio.Reader
}

func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// ReadFrame parses headers until CRLF CRLF, then reads Content-Length bytes.
// Returns io.EOF cleanly when the stream ends between frames.
func (fr *FrameReader) ReadFrame() ([]byte, error) {
	length := -1

	for {
		line, err := fr.br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("read header: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header: %q", line)
		}

		if strings.EqualFold(strings.TrimSpace(key), "content-length") {
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return nil, fmt.Errorf("parse content-length: %w", err)
			}

			if n < 0 {
				return nil, fmt.Errorf("negative content-length: %d", n)
			}

			if n > MaxFrameSize {
				return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameSize)
			}

			length = n
		}
	}

	if length < 0 {
		return nil, errors.New("missing content-length header")
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(fr.br, buf); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	return buf, nil
}

// FrameWriter writes Content-Length framed payloads. Safe for concurrent use.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

func (fw *FrameWriter) WriteFrame(payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(payload), MaxFrameSize)
	}

	fw.mu.Lock()
	defer fw.mu.Unlock()

	hdr := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(fw.w, hdr); err != nil {
		return err
	}

	if _, err := fw.w.Write(payload); err != nil {
		return err
	}

	return nil
}
