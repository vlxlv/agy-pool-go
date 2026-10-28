package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
)

// releasesAPI lists releases newest-first; every published tag is a prerelease, so /releases/latest would 404.
var releasesAPI = "https://api.github.com/repos/vlxlv/agy-pool-go/releases?per_page=1"

// updateTargetPath resolves the binary to replace; tests point it at a temp file.
var updateTargetPath = daemon.GetEntrypointPath

// dpkgFileList lists the files owned by the agy-pool Debian package; tests point it at a temp file.
var dpkgFileList = "/var/lib/dpkg/info/agy-pool.list"

// UpdateCmd replaces the running agy-pool binary with the newest GitHub release and restarts a running gateway.
func UpdateCmd(stdout, stderr io.Writer, args ...string) int {
	checkOnly := false
	for _, arg := range args {
		switch arg {
		case "--check":
			checkOnly = true
		case "-h", "--help":
			fmt.Fprintf(stdout, "usage: agy-pool update [--check]\n")
			return 0
		default:
			fmt.Fprintf(stderr, "agy-pool update: error: unrecognized argument: %s\n", arg)
			return 2
		}
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, fmt.Sprintf(format, a...), clrReset)
		return 1
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	token := githubToken()
	body, err := download(client, releasesAPI, token)
	if err != nil {
		return fail("Cannot query releases: %v", err)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"url"` // API URL; works for private repos, unlike browser_download_url
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &releases); err != nil || len(releases) == 0 {
		return fail("Cannot parse releases response: %v", err)
	}
	rel := releases[0]
	latest := strings.TrimPrefix(rel.TagName, "v")
	// shortcut: any differing tag counts as newer, so a local build ahead of the newest release would be downgraded; add semver ordering if that happens.
	if latest == config.Version {
		fmt.Fprintf(stdout, "%s✓ agy-pool %s is up to date%s\n", clrGreen, config.Version, clrReset)
		return 0
	}
	fmt.Fprintf(stdout, "Update available: %s → %s\n", config.Version, latest)
	if checkOnly {
		return 0
	}
	exe := updateTargetPath()
	if list, err := os.ReadFile(dpkgFileList); err == nil && slices.Contains(strings.Split(string(list), "\n"), exe) {
		// Overwriting a dpkg-owned file would leave the package database describing a binary that is gone.
		return fail("%s is installed by the agy-pool Debian package; install agy-pool_%s_%s.deb from release %s with `sudo apt install ./<file>.deb`",
			exe, latest, runtime.GOARCH, rel.TagName)
	}

	binName := "agy-pool-" + runtime.GOOS + "-" + runtime.GOARCH
	assets := map[string]string{}
	for _, a := range rel.Assets {
		assets[a.Name] = a.URL
	}
	if assets[binName] == "" || assets["SHA256SUMS"] == "" {
		return fail("Release %s has no %s or SHA256SUMS asset", rel.TagName, binName)
	}
	sums, err := download(client, assets["SHA256SUMS"], token)
	if err != nil {
		return fail("Cannot download SHA256SUMS: %v", err)
	}
	want := checksumFor(sums, binName)
	if want == "" {
		return fail("SHA256SUMS has no entry for %s", binName)
	}
	bin, err := download(client, assets[binName], token)
	if err != nil {
		return fail("Cannot download %s: %v", binName, err)
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != want {
		return fail("Checksum mismatch for %s; binary not replaced", binName)
	}

	wasRunning := daemon.IsDaemonRunning("", 0)
	if err := replaceFile(exe, bin); err != nil {
		return fail("Cannot replace %s: %v", exe, err)
	}
	fmt.Fprintf(stdout, "%s✓ Updated %s to %s%s\n", clrGreen, exe, latest, clrReset)

	if wasRunning {
		// Exec the new binary: this process's own executable path now points at the replaced inode.
		cmd := exec.Command(exe, "restart")
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return fail("Updated, but gateway restart failed: %v (run `agy-pool restart`)", err)
		}
	}
	return 0
}

// githubToken authorizes access to the private repo: GH_TOKEN, GITHUB_TOKEN, then the gh CLI login.
func githubToken() string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, _ := exec.Command("gh", "auth", "token").Output()
	return strings.TrimSpace(string(out))
}

// download fetches a GitHub API URL; asset URLs need the octet-stream Accept to return content instead of metadata.
// net/http drops Authorization on the cross-host redirect to the asset CDN.
func download(client *http.Client, url, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.Contains(url, "/releases/assets/") {
		req.Header.Set("Accept", "application/octet-stream")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// checksumFor returns the hex digest for name from sha256sum-format output, or "".
func checksumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// replaceFile atomically swaps dst for data via a temp file in the same directory.
func replaceFile(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agy-pool-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
