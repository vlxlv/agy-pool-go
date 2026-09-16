package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	_ "modernc.org/sqlite"
)

// ANSI Color constants
const (
	clrReset  = "\033[0m"
	clrBold   = "\033[1m"
	clrGreen  = "\033[32m"
	clrYellow = "\033[33m"
)

// ConversationRecord holds a row from conversation_summaries
type ConversationRecord struct {
	ID           string
	Title        string
	Dirs         []string
	LastModified int64
}

// FindLatestConversationForDir finds the most recent conversation ID matching startDir (or its parent project),
// ordered strictly by last_modified_time DESC, across ALL Google accounts.
// Returns (cid, title, matchedDir, err).
func FindLatestConversationForDir(startDir string) (string, string, string, error) {
	if startDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", "", nil
		}
		startDir = cwd
	}

	realStart, err := filepath.EvalSymlinks(startDir)
	if err != nil {
		realStart = filepath.Clean(startDir)
	}

	homeDir := ""
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		if eval, err := filepath.EvalSymlinks(u.HomeDir); err == nil {
			homeDir = eval
		} else {
			homeDir = filepath.Clean(u.HomeDir)
		}
	} else if h := os.Getenv("HOME"); h != "" {
		if eval, err := filepath.EvalSymlinks(h); err == nil {
			homeDir = eval
		} else {
			homeDir = filepath.Clean(h)
		}
	}

	dbPath := config.GetConversationDBFile()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", "", "", nil
	}

	// Connect to SQLite in read-only mode
	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", "", "", nil
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON;"); err != nil {
		return "", "", "", nil
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 2000;"); err != nil {
		return "", "", "", nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT conversation_id, title, workspace_uris, last_modified_time
		FROM conversation_summaries
		ORDER BY last_modified_time DESC
	`)
	if err != nil {
		return "", "", "", nil
	}
	defer rows.Close()

	var convs []ConversationRecord
	for rows.Next() {
		var cid, title, urisRaw sql.NullString
		var mtime sql.NullInt64
		if err := rows.Scan(&cid, &title, &urisRaw, &mtime); err != nil {
			continue
		}
		if !cid.Valid || cid.String == "" {
			continue
		}

		var dirs []string
		if urisRaw.Valid && urisRaw.String != "" {
			var uris []string
			if err := json.Unmarshal([]byte(urisRaw.String), &uris); err == nil {
				for _, u := range uris {
					if u == "" {
						continue
					}
					clean := strings.TrimPrefix(u, "file://")
					if unesc, err := url.PathUnescape(clean); err == nil {
						clean = unesc
					}
					clean = strings.TrimRight(clean, "/")
					if realD, err := filepath.EvalSymlinks(clean); err == nil {
						dirs = append(dirs, realD)
					} else {
						dirs = append(dirs, filepath.Clean(clean))
					}
				}
			}
		}

		convs = append(convs, ConversationRecord{
			ID:           cid.String,
			Title:        title.String,
			Dirs:         dirs,
			LastModified: mtime.Int64,
		})
	}

	// Traverse upwards from realStart matching any conversation workspace
	curr := realStart
	for {
		targetPath := strings.TrimRight(curr, string(os.PathSeparator))
		if targetPath == "" && curr == "/" {
			targetPath = "/"
		}

		for _, conv := range convs {
			for _, d := range conv.Dirs {
				if targetPath == d {
					return conv.ID, conv.Title, targetPath, nil
				}
			}
		}

		if curr == homeDir || curr == "/" || filepath.Dir(curr) == curr {
			break
		}
		curr = filepath.Dir(curr)
	}

	return "", "", "", nil
}

// ResolveContinueArg intercepts -c / --continue and resolves it directly to --conversation <cid>
// for the current directory's most recent conversation across all accounts.
func ResolveContinueArg(extraArgs []string, errOut io.Writer) []string {
	if errOut == nil {
		errOut = os.Stderr
	}

	hasContinue := false
	var newArgs []string
	skipNext := false

	for i, arg := range extraArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "-c" || arg == "--continue" {
			hasContinue = true
		} else if arg == "--conversation" || strings.HasPrefix(arg, "--conversation=") {
			// If user explicitly passed --conversation, do not interfere
			return extraArgs
		} else {
			newArgs = append(newArgs, extraArgs[i])
		}
	}

	if !hasContinue {
		return extraArgs
	}

	cid, title, matchedDir, err := FindLatestConversationForDir("")
	if err != nil || cid == "" {
		return extraArgs
	}

	lockFile := config.GetPresenceLockFile(cid)
	isLocked := false
	if _, statErr := os.Stat(lockFile); statErr == nil {
		if f, err := os.OpenFile(lockFile, os.O_RDWR, 0); err == nil {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err != nil {
				isLocked = true
			} else {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			f.Close()
		}
	}

	dirName := ""
	if matchedDir != "" {
		dirName = filepath.Base(matchedDir)
	}
	if dirName == "" || dirName == "." || dirName == "/" {
		dirName = "workspace"
	}

	displayTitle := strings.TrimSpace(title)
	if displayTitle == "" {
		if len(cid) >= 8 {
			displayTitle = cid[:8]
		} else {
			displayTitle = cid
		}
	}

	cidShort := cid
	if len(cidShort) > 8 {
		cidShort = cidShort[:8]
	}

	if isLocked {
		fmt.Fprintf(errOut, "%s[agy-pool] Notice: Conversation '%s' (%s...) is active in another process.%s\n", clrYellow, displayTitle, cidShort, clrReset)
	} else {
		fmt.Fprintf(errOut, "%s✓ [agy-pool] Resuming last conversation in %s: %s%s%s\n", clrGreen, dirName, clrBold, displayTitle, clrReset)
	}

	return append([]string{"--conversation", cid}, newArgs...)
}
