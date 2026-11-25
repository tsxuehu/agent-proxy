package gateway

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tsxuehu/agent-proxy/shared/codex"
)

var errEncoding = errors.New("unsupported request encoding")

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw := http.MaxBytesReader(w, r.Body, codex.MaxBody)
	reader := io.Reader(raw)
	switch strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		body, err := gzip.NewReader(raw)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		reader = body
	case "zstd":
		body, err := zstd.NewReader(raw, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(codex.MaxBody*2))
		if err != nil {
			return nil, err
		}
		defer body.Close()
		reader = body
	default:
		return nil, errEncoding
	}
	data, err := io.ReadAll(io.LimitReader(reader, codex.MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read request body: %w", err)
	}
	if len(data) > codex.MaxBody {
		return nil, &http.MaxBytesError{Limit: codex.MaxBody}
	}
	return data, nil
}
