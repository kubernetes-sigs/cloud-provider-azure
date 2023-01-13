// Package gzip provides a streaming object for taking in io.ReadCloser that is being written to
// and providing an io.ReadCloser that outputs the original content gzip compressed.
package gzip

import (
	"compress/gzip"
	"io"
	"sync"
	"sync/atomic"
)

var compressPool = &sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(nil)
	},
}

// Streamer implements an io.ReadCloser that converts data from a non-compressed stream to a compressed stream.
type Streamer struct {
	userInput   io.ReadCloser
	outputRead  *io.PipeReader
	outputWrite *io.PipeWriter
	size        int64
	err         atomic.Value // holds error
}

// New creates a new streamer object. Use Reset() to initialize it.
func New() *Streamer {
	return &Streamer{}
}

// Reset resets the streamer object to defaults and accepts the io.ReadCloser.
// You can only use Reset after a previous reader has closed.
func (s *Streamer) Reset(reader io.ReadCloser) {
	s.userInput = reader
	s.outputRead, s.outputWrite = io.Pipe()
	s.size = 0
	s.err = atomic.Value{}

	s.run()
}

// InputSize returns the amount of data that the Streamer streamed. This will only be accurate for
// the full stream after Read() has returned io.EOF and not before.
func (s *Streamer) InputSize() int64 {
	return atomic.LoadInt64(&s.size)
}

func Compress(payload io.Reader) io.Reader {
	var closer io.ReadCloser
	var ok bool
	if closer, ok = payload.(io.ReadCloser); !ok {
		closer = io.NopCloser(payload)
	}
	zw := New()
	zw.Reset(closer)

	return zw
}

// run copies the file into a buffer that we stream back via our Read() call.
func (s *Streamer) run() {
	zw := compressPool.Get().(*gzip.Writer)
	zw.Reset(s.outputWrite)

	go func() {
		defer compressPool.Put(zw)
		defer s.outputWrite.Close()
		defer zw.Close()
		defer zw.Flush()

		_, err := io.Copy(zw, s.userInput)
		if err != nil {
			s.err.Store(err)
		}
	}()
}

// Read implements io.Reader.
func (s *Streamer) Read(b []byte) (int, error) {
	amount, err := s.outputRead.Read(b)
	atomic.AddInt64(&s.size, int64(amount))
	return amount, err
}

// Close implements io.Closer.
func (s *Streamer) Close() error {
	return s.outputRead.Close()
}
