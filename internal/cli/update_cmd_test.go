package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestUpdateCmd(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	binName := "agy-pool-" + runtime.GOOS + "-" + runtime.GOARCH
	newBin := []byte("new binary")
	sum := sha256.Sum256(newBin)
	sums := hex.EncodeToString(sum[:]) + "  " + binName + "\n"
	tag := "v9.9.9"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"tag_name":%q,"assets":[{"name":%q,"url":"%s/bin"},{"name":"SHA256SUMS","url":"%s/sums"}]}]`,
			tag, binName, srv.URL, srv.URL)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(newBin) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })

	oldAPI, oldTarget := releasesAPI, updateTargetPath
	defer func() { releasesAPI, updateTargetPath = oldAPI, oldTarget }()
	releasesAPI = srv.URL + "/releases"
	target := filepath.Join(t.TempDir(), "agy-pool")
	updateTargetPath = func() string { return target }

	run := func(args ...string) (int, string) {
		if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := Main(append([]string{"update"}, args...), nil, &stdout, &stderr)
		got, _ := os.ReadFile(target)
		return code, stdout.String() + stderr.String() + "|" + string(got)
	}

	if code, out := run("--check"); code != 0 || !strings.Contains(out, "9.9.9") || !strings.HasSuffix(out, "|old binary") {
		t.Fatalf("--check: code=%d out=%q", code, out)
	}
	if code, out := run(); code != 0 || !strings.HasSuffix(out, "|new binary") {
		t.Fatalf("update: code=%d out=%q", code, out)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}

	sums = "0000  " + binName + "\n"
	if code, out := run(); code != 1 || !strings.HasSuffix(out, "|old binary") {
		t.Fatalf("bad checksum must not replace: code=%d out=%q", code, out)
	}

	tag = "v" + config.Version
	if code, out := run(); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("up to date: code=%d out=%q", code, out)
	}
}
