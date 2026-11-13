package buildrunner

import (
	"bytes"
	"io"
	"slices"
	"sync"
)

// redactingWriter保留最长秘密的尾部，避免跨Write边界直接泄露已知值。
// 这仅尽力处理已知明文：应用主动编码、变形或打印未知秘密仍无法保证自动保密。
type redactingWriter struct {
	mu      sync.Mutex
	out     io.Writer
	secrets [][]byte
	pending []byte
	hold    int
}

func newRedactingWriter(out io.Writer, secrets [][]byte) *redactingWriter {
	r := &redactingWriter{out: out}
	for _, s := range secrets {
		if len(s) > 0 {
			r.secrets = append(r.secrets, slices.Clone(s))
			if len(s) > r.hold {
				r.hold = len(s)
			}
		}
	}
	slices.SortFunc(r.secrets, func(a, b []byte) int { return len(b) - len(a) })
	return r
}
func (r *redactingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, p...)
	return len(p), r.emit(false)
}
func (r *redactingWriter) Close() error { r.mu.Lock(); defer r.mu.Unlock(); return r.emit(true) }
func (r *redactingWriter) emit(final bool) error {
	cutoff := len(r.pending) - r.hold + 1
	if final {
		cutoff = len(r.pending)
	}
	if cutoff < 0 {
		cutoff = 0
	}
	position := 0
	output := make([]byte, 0, cutoff)
	for position < cutoff {
		matched := false
		for _, secret := range r.secrets {
			if bytes.HasPrefix(r.pending[position:], secret) {
				output = append(output, []byte("[REDACTED]")...)
				position += len(secret)
				matched = true
				break
			}
		}
		if !matched {
			output = append(output, r.pending[position])
			position++
		}
	}
	r.pending = slices.Clone(r.pending[position:])
	if len(output) > 0 {
		n, err := r.out.Write(output)
		if err != nil {
			return err
		}
		if n != len(output) {
			return io.ErrShortWrite
		}
	}
	return nil
}
