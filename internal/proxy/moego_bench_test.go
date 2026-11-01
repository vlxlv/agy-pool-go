package proxy

import (
	"crypto/rand"
	stdBase64 "encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	moegoBase64 "github.com/MoeForge/moego/base64"
)

// Generate realistic multi-turn conversation body (~50KB)
func makeRealisticMultiTurnBody() []byte {
	var turns []string
	for i := 0; i < 30; i++ {
		turns = append(turns, fmt.Sprintf(`{"role":"user","parts":[{"text":"User prompt turn %d with code and detailed question about algorithms and system design."}]}`, i))
		turns = append(turns, fmt.Sprintf(`{"role":"model","parts":[{"text":"Model response turn %d with extensive explanation, code snippets, and analysis."}]}`, i))
	}
	contentStr := strings.Join(turns, ",")
	return []byte(fmt.Sprintf(`{"model":"gemini-3.8-flash-high","request":{"sessionId":"-3750763034362895579","labels":{"cascade_id":"cascade-xyz-12345"},"contents":[%s]}}`, contentStr))
}

func BenchmarkRequestModel(b *testing.B) {
	body := makeRealisticMultiTurnBody()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := requestModel(body)
		if m == "" {
			b.Fatal("empty model")
		}
	}
}

func BenchmarkExtractSessionKey(b *testing.B) {
	req, _ := http.NewRequest("POST", "/v1internal:streamGenerateContent", nil)
	body := makeRealisticMultiTurnBody()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		k := ExtractSessionKey(req, body)
		if k == "" {
			b.Fatal("empty key")
		}
	}
}

func BenchmarkBase64_1MiB_Std(b *testing.B) {
	raw := make([]byte, 1024*1024)
	_, _ = rand.Read(raw)
	encoded := stdBase64.StdEncoding.EncodeToString(raw)

	b.Run("Encode", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = stdBase64.StdEncoding.EncodeToString(raw)
		}
	})

	b.Run("Decode", func(b *testing.B) {
		b.SetBytes(int64(len(encoded)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = stdBase64.StdEncoding.DecodeString(encoded)
		}
	})
}

func BenchmarkBase64_1MiB_Moego(b *testing.B) {
	raw := make([]byte, 1024*1024)
	_, _ = rand.Read(raw)
	encoded := moegoBase64.StdEncoding.EncodeToString(raw)

	b.Run("Encode", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = moegoBase64.StdEncoding.EncodeToString(raw)
		}
	})

	b.Run("Decode", func(b *testing.B) {
		b.SetBytes(int64(len(encoded)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = moegoBase64.StdEncoding.DecodeString(encoded)
		}
	})
}
