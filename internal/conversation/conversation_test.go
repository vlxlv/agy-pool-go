package conversation

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	_ "modernc.org/sqlite"
)

func setupTestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "agy_conv_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	stateDir := filepath.Join(tempDir, ".gemini")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	origState := config.GetStateDir()
	origTestMode := config.IsTestMode()
	origEnv := os.Getenv("AGY_GEMINI_DIR")
	os.Setenv("AGY_GEMINI_DIR", stateDir)
	config.SetTestMode(true)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	cleanup := func() {
		_ = config.ConfigureStateDir(origState)
		config.SetTestMode(origTestMode)
		if origEnv != "" {
			os.Setenv("AGY_GEMINI_DIR", origEnv)
		} else {
			os.Unsetenv("AGY_GEMINI_DIR")
		}
		_ = os.RemoveAll(tempDir)
	}

	return tempDir, cleanup
}

func createTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	_ = os.Remove(dbPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatalf("failed to create db dir: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	_, err = db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT,
		title TEXT,
		workspace_uris TEXT,
		last_modified_time INTEGER
	);`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	return db
}

func TestFindLatestConversation_MissingDB(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	cid, title, matchedDir, err := FindLatestConversationForDir("/some/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "" || title != "" || matchedDir != "" {
		t.Fatalf("expected empty result, got cid=%q title=%q matchedDir=%q", cid, title, matchedDir)
	}
}

func TestFindLatestConversation_OrderingAndHierarchy(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	dbPath := filepath.Join(config.GetAgyCliDir(), "conversation_summaries.db")
	db := createTestDB(t, dbPath)
	defer db.Close()

	projDir := filepath.Join(tempDir, "workspace", "my-project")
	subDir := filepath.Join(projDir, "src", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}

	projURI, _ := json.Marshal([]string{"file://" + projDir})
	otherURI, _ := json.Marshal([]string{"file://" + filepath.Join(tempDir, "other")})

	// Older conversation in projDir
	_, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`,
		"cid-old", "Old Project Conv", string(projURI), 1000)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Newer conversation in otherDir
	_, err = db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`,
		"cid-other", "Other Conv", string(otherURI), 3000)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Newest conversation in projDir
	_, err = db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`,
		"cid-newest", "Newest Project Conv", string(projURI), 2000)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Test matching from subDir walking up to projDir
	cid, title, matchedDir, err := FindLatestConversationForDir(subDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "cid-newest" {
		t.Errorf("expected cid-newest, got %q", cid)
	}
	if title != "Newest Project Conv" {
		t.Errorf("expected 'Newest Project Conv', got %q", title)
	}
	if matchedDir != projDir {
		t.Errorf("expected %q, got %q", projDir, matchedDir)
	}

	// Record stat before and after to ensure DB is read-only
	fi1, _ := os.Stat(dbPath)
	_, _, _, _ = FindLatestConversationForDir(subDir)
	fi2, _ := os.Stat(dbPath)
	if fi1.ModTime() != fi2.ModTime() {
		t.Errorf("database file was modified during read! orig=%v, after=%v", fi1.ModTime(), fi2.ModTime())
	}
}

func TestFindLatestConversation_URLEncoding(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	dbPath := filepath.Join(config.GetAgyCliDir(), "conversation_summaries.db")
	db := createTestDB(t, dbPath)
	defer db.Close()

	projDir := filepath.Join(tempDir, "workspace", "my special project")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}

	// Encoded URI: spaces -> %20
	encURI := "file://" + strings.ReplaceAll(projDir, " ", "%20")
	rawJSON, _ := json.Marshal([]string{encURI})

	_, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`,
		"cid-special", "Special Project", string(rawJSON), 5000)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	cid, title, matchedDir, err := FindLatestConversationForDir(projDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "cid-special" {
		t.Errorf("expected cid-special, got %q", cid)
	}
	if matchedDir != projDir {
		t.Errorf("expected matchedDir %q, got %q", projDir, matchedDir)
	}
	_ = title
}

func TestResolveContinueArg(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	// 1. No continue flag
	origArgs := []string{"foo", "--bar", "baz"}
	res := ResolveContinueArg(origArgs, nil)
	if fmt.Sprintf("%v", res) != fmt.Sprintf("%v", origArgs) {
		t.Fatalf("expected args unchanged when no -c flag, got %v", res)
	}

	// 2. Explicit --conversation takes precedence
	explicit := []string{"--conversation", "explicit-123", "-c"}
	res = ResolveContinueArg(explicit, nil)
	if fmt.Sprintf("%v", res) != fmt.Sprintf("%v", explicit) {
		t.Fatalf("expected explicit --conversation preserved, got %v", res)
	}

	explicitEq := []string{"--conversation=explicit-123", "--continue"}
	res = ResolveContinueArg(explicitEq, nil)
	if fmt.Sprintf("%v", res) != fmt.Sprintf("%v", explicitEq) {
		t.Fatalf("expected explicit --conversation= preserved, got %v", res)
	}

	// 3. Populate DB and test resolution
	dbPath := filepath.Join(config.GetAgyCliDir(), "conversation_summaries.db")
	db := createTestDB(t, dbPath)
	defer db.Close()

	cwd, _ := os.Getwd()
	projURI, _ := json.Marshal([]string{"file://" + cwd})
	_, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`,
		"cid-resolved-456", "Active Dev Session", string(projURI), 9999)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	var buf bytes.Buffer
	res = ResolveContinueArg([]string{"-c", "--extra-flag"}, &buf)
	expected := []string{"--conversation", "cid-resolved-456", "--extra-flag"}
	if fmt.Sprintf("%v", res) != fmt.Sprintf("%v", expected) {
		t.Fatalf("expected %v, got %v", expected, res)
	}
	if !strings.Contains(buf.String(), "Resuming last conversation in") {
		t.Errorf("expected resume message, got %q", buf.String())
	}

	// 4. Presence Lock Active
	presenceDir := filepath.Join(config.GetAgyCliDir(), "presence")
	_ = os.MkdirAll(presenceDir, 0700)
	lockFile := filepath.Join(presenceDir, "cid-resolved-456.lock")
	lockFd, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("failed to create lock file: %v", err)
	}
	defer lockFd.Close()

	// Acquire exclusive lock on the file to simulate another running process
	if err := syscall.Flock(int(lockFd.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("failed to flock: %v", err)
	}

	buf.Reset()
	res = ResolveContinueArg([]string{"--continue"}, &buf)
	if fmt.Sprintf("%v", res) != fmt.Sprintf("%v", []string{"--conversation", "cid-resolved-456"}) {
		t.Fatalf("expected resolved args with lock, got %v", res)
	}
	if !strings.Contains(buf.String(), "Notice: Conversation 'Active Dev Session' (cid-reso...) is active in another process.") {
		t.Errorf("expected active in another process notice, got %q", buf.String())
	}

	_ = syscall.Flock(int(lockFd.Fd()), syscall.LOCK_UN)
}
