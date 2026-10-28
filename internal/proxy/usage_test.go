package proxy

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestUsageTapSplitAcrossReads(t *testing.T) {
	body := `data: {"response":{"usageMetadata":{"promptTokenCount": 10}}}` + "\n\n" +
		`data: {"response":{"usageMetadata":{"promptTokenCount": 12345,"cachedContentTokenCount": 9876}}}` + "\n\n"
	tap := &usageTap{r: iotest.OneByteReader(strings.NewReader(body)), cached: -1}
	out, err := io.ReadAll(tap)
	if err != nil || string(out) != body {
		t.Fatalf("tap altered the stream: err=%v", err)
	}
	if tap.prompt != 12345 || tap.cached != 9876 {
		t.Fatalf("got prompt=%d cached=%d, want 12345/9876", tap.prompt, tap.cached)
	}
}

func TestUsageTapCachedNotReported(t *testing.T) {
	tap := usageTap{cached: -1}
	tap.scan([]byte(`{"usageMetadata":{"promptTokenCount": 42}}`))
	if tap.prompt != 42 || tap.cached != -1 {
		t.Fatalf("got prompt=%d cached=%d, want 42/-1", tap.prompt, tap.cached)
	}
}
