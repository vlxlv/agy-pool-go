package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// SendStream streams upstream response chunks with Transfer-Encoding: chunked.
// Once headers are written downstream, the stream is committed and cannot be replayed.
func SendStream(w http.ResponseWriter, status int, headers http.Header, response io.Reader) {
	filtered := FilterResponseHeaders(headers)
	for k, vv := range filtered {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)

	flusher, hasFlusher := w.(http.Flusher)
	if hasFlusher {
		flusher.Flush()
	}

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
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				// Downstream client disconnected
				return
			}
			if hasFlusher {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				logMessage("STREAM TRUNCATED", err.Error())
			}
			break
		}
	}

	if expectedLen != nil && sent != *expectedLen {
		logMessage("STREAM TRUNCATED", fmt.Sprintf("expected %d bytes, sent %d", *expectedLen, sent))
	}
}
