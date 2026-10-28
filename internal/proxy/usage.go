package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"regexp"
	"strconv"
)

var usageRe = regexp.MustCompile(`"(promptTokenCount|cachedContentTokenCount)"\s*:\s*(\d+)`)

// usageTap records the last promptTokenCount and cachedContentTokenCount seen in a response body as it is read,
// so prompt-cache hits can be measured per account without buffering streams.
type usageTap struct {
	r      io.Reader
	carry  []byte // tail of the previous read, so a field split across reads is still matched
	prompt int64
	cached int64 // -1 until upstream reports cachedContentTokenCount, which it omits rather than sending 0
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.scan(p[:n])
	}
	return n, err
}

func (t *usageTap) scan(b []byte) {
	buf := b
	if len(t.carry) > 0 {
		buf = append(t.carry, b...)
	}
	for _, m := range usageRe.FindAllSubmatch(buf, -1) {
		v, _ := strconv.ParseInt(string(m[2]), 10, 64)
		if string(m[1]) == "promptTokenCount" {
			t.prompt = v
		} else {
			t.cached = v
		}
	}
	t.carry = append([]byte(nil), buf[max(0, len(buf)-64):]...)
}

// sessionTag returns a short, stable, non-reversible label for a session key in logs.
func sessionTag(sessionKey string) string {
	if sessionKey == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:4])
}
