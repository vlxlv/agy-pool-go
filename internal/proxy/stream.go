package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// SendStream streams upstream response chunks with Transfer-Encoding: chunked.
// Once headers are written downstream, the stream is committed and cannot be replayed.
// If an error occurs after commitment, SendStream returns ErrCommittedStream or ErrClientDisconnected.
func SendStream(w http.ResponseWriter, status int, headers http.Header, response io.Reader) error {
	committer := NewResponseCommitter(w)

	filtered := FilterResponseHeaders(headers)
	for k, vv := range filtered {
		for _, v := range vv {
			committer.Header().Add(k, v)
		}
	}
	committer.Header().Set("Transfer-Encoding", "chunked")
	committer.Header().Set("Connection", "close")
	committer.WriteHeader(status)
	committer.MarkStream()
	committer.Flush()

	var expectedLen *int64
	if clStr := headers.Get("Content-Length"); clStr != "" {
		if cl, err := strconv.ParseInt(clStr, 10, 64); err == nil {
			expectedLen = &cl
		}
	}

	buf := make([]byte, 4096)
	var sent int64
	for {
		n, err := response.Read(buf)
		if n > 0 {
			sent += int64(n)
			if _, wErr := committer.Write(buf[:n]); wErr != nil {
				// Downstream client disconnected
				return ErrClientDisconnected
			}
			committer.Flush()
		}
		if err != nil {
			if err != io.EOF {
				logMessage("STREAM TRUNCATED", err.Error())
				return fmt.Errorf("%w: %v", ErrCommittedStream, err)
			}
			break
		}
	}

	if expectedLen != nil && sent != *expectedLen {
		logMessage("STREAM TRUNCATED", fmt.Sprintf("expected %d bytes, sent %d", *expectedLen, sent))
		return fmt.Errorf("%w: expected %d bytes, sent %d", ErrCommittedStream, *expectedLen, sent)
	}
	return nil
}
