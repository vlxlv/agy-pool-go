package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// EncryptedBundle represents the wire format of an encrypted account backup.
type EncryptedBundle struct {
	Format     string `json:"format"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	Tag        string `json:"tag"`
}

// EncryptBundle encrypts plaintext using PBKDF2-HMAC-SHA256 and HMAC-CTR stream cipher matching Python agy-pool.
func EncryptBundle(plaintext []byte, password string, salt, nonce []byte) (*EncryptedBundle, error) {
	if len(salt) == 0 {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("failed to generate random salt: %w", err)
		}
	}
	if len(nonce) == 0 {
		nonce = make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return nil, fmt.Errorf("failed to generate random nonce: %w", err)
		}
	}

	const iterations = 100000
	dk := pbkdf2.Key([]byte(password), salt, iterations, 64, sha256.New)
	keyEnc := dk[:32]
	keyMAC := dk[32:]

	ciphertext := make([]byte, len(plaintext))
	blockSize := 32
	numBlocks := (len(plaintext) + blockSize - 1) / blockSize

	ctrBuf := make([]byte, 24)
	copy(ctrBuf[:16], nonce)

	for i := 0; i < numBlocks; i++ {
		binary.BigEndian.PutUint64(ctrBuf[16:], uint64(i))
		mac := hmac.New(sha256.New, keyEnc)
		mac.Write(ctrBuf)
		ksBlock := mac.Sum(nil)

		start := i * blockSize
		end := start + blockSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		for j := start; j < end; j++ {
			ciphertext[j] = plaintext[j] ^ ksBlock[j-start]
		}
	}

	mac := hmac.New(sha256.New, keyMAC)
	mac.Write(salt)
	mac.Write(nonce)
	mac.Write(ciphertext)
	tag := mac.Sum(nil)

	return &EncryptedBundle{
		Format:     "agy-pool-encrypted-v1",
		KDF:        "pbkdf2_hmac_sha256",
		Iterations: iterations,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		Tag:        base64.StdEncoding.EncodeToString(tag),
	}, nil
}

// DecryptBundle decrypts an EncryptedBundle matching Python agy-pool.
func DecryptBundle(bundle *EncryptedBundle, password string) ([]byte, error) {
	if bundle == nil || bundle.Format != "agy-pool-encrypted-v1" {
		return nil, errors.New("Unsupported or invalid encrypted bundle format")
	}

	salt, err := base64.StdEncoding.DecodeString(bundle.Salt)
	if err != nil {
		return nil, fmt.Errorf("Corrupted encrypted bundle metadata: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(bundle.Nonce)
	if err != nil {
		return nil, fmt.Errorf("Corrupted encrypted bundle metadata: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(bundle.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("Corrupted encrypted bundle metadata: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(bundle.Tag)
	if err != nil {
		return nil, fmt.Errorf("Corrupted encrypted bundle metadata: %w", err)
	}

	iterations := bundle.Iterations
	if iterations <= 0 {
		iterations = 100000
	}

	dk := pbkdf2.Key([]byte(password), salt, iterations, 64, sha256.New)
	keyEnc := dk[:32]
	keyMAC := dk[32:]

	mac := hmac.New(sha256.New, keyMAC)
	mac.Write(salt)
	mac.Write(nonce)
	mac.Write(ciphertext)
	expectedTag := mac.Sum(nil)

	if !hmac.Equal(tag, expectedTag) {
		return nil, errors.New("Invalid password or corrupted backup payload")
	}

	plaintext := make([]byte, len(ciphertext))
	blockSize := 32
	numBlocks := (len(ciphertext) + blockSize - 1) / blockSize

	ctrBuf := make([]byte, 24)
	copy(ctrBuf[:16], nonce)

	for i := 0; i < numBlocks; i++ {
		binary.BigEndian.PutUint64(ctrBuf[16:], uint64(i))
		ksMac := hmac.New(sha256.New, keyEnc)
		ksMac.Write(ctrBuf)
		ksBlock := ksMac.Sum(nil)

		start := i * blockSize
		end := start + blockSize
		if end > len(ciphertext) {
			end = len(ciphertext)
		}
		for j := start; j < end; j++ {
			plaintext[j] = ciphertext[j] ^ ksBlock[j-start]
		}
	}

	return plaintext, nil
}

// ExportPool exports the current pool to a file or stdout, optionally encrypted.
func ExportPool(filePath string, encrypt bool, password string, noStats bool) (string, error) {
	pool, err := storage.LoadPool()
	if err != nil {
		return "", err
	}
	if len(pool.Accounts) == 0 {
		return "", errors.New("no accounts in pool to export")
	}

	exportAccounts := make([]map[string]any, 0, len(pool.Accounts))
	for _, acc := range pool.Accounts {
		item := map[string]any{
			"email":         acc.Email,
			"name":          acc.Name,
			"refresh_token": acc.RefreshToken,
			"access_token":  acc.AccessToken,
			"token_expiry":  acc.TokenExpiry,
			"created_at":    acc.CreatedAt,
		}
		if !noStats {
			item["request_count"] = acc.RequestCount
			if acc.GenCount != nil {
				item["gen_count"] = *acc.GenCount
			} else {
				item["gen_count"] = int64(0)
			}
		}
		if acc.LastQuota != nil {
			item["last_quota"] = acc.LastQuota
		}
		exportAccounts = append(exportAccounts, item)
	}

	var activeEmail *string
	if pool.ActiveAccountID != nil {
		for _, acc := range pool.Accounts {
			if acc.ID == *pool.ActiveAccountID {
				activeEmail = &acc.Email
				break
			}
		}
	}

	bundle := map[string]any{
		"version":              1,
		"app":                  "agy-pool",
		"exported_at":          time.Now().Unix(),
		"strategy":             pool.Strategy,
		"active_account_email": activeEmail,
		"accounts":             exportAccounts,
	}

	rawJSON, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal export bundle: %w", err)
	}

	isEncrypt := encrypt || password != ""
	var outputContent []byte

	if isEncrypt {
		if password == "" {
			return "", errors.New("password required when encrypting backup")
		}
		encBundle, err := EncryptBundle(rawJSON, password, nil, nil)
		if err != nil {
			return "", fmt.Errorf("failed to encrypt bundle: %w", err)
		}
		encJSON, err := json.MarshalIndent(encBundle, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal encrypted bundle: %w", err)
		}
		outputContent = encJSON
	} else {
		outputContent = rawJSON
	}

	if filePath == "-" {
		_, err := os.Stdout.Write(append(outputContent, '\n'))
		return "-", err
	}

	targetPath := filePath
	if targetPath == "" {
		ext := ".json"
		if isEncrypt {
			ext = ".enc"
		}
		targetPath = fmt.Sprintf("agy-pool-backup-%s%s", time.Now().Format("20060102_150405"), ext)
	}

	if err := config.AssertSafeWritePath(targetPath); err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return "", fmt.Errorf("failed to create export directory: %w", err)
	}

	if err := os.WriteFile(targetPath, append(outputContent, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("failed to write export file: %w", err)
	}

	return targetPath, nil
}

// ImportResult describes the outcome of an ImportPool operation.
type ImportResult struct {
	Added   int
	Updated int
	Skipped int
	Total   int
}

// ImportPool imports account credentials from a backup file, merging or replacing existing accounts.
func ImportPool(filePath, password string, replace, skipExisting bool) (*ImportResult, error) {
	var content []byte
	var err error

	if filePath == "-" {
		content, err = io.ReadAll(os.Stdin)
	} else {
		content, err = os.ReadFile(filePath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read backup file %q: %w", filePath, err)
	}

	var rawObj map[string]any
	if err := json.Unmarshal(content, &rawObj); err != nil {
		return nil, fmt.Errorf("failed to parse backup file as JSON: %w", err)
	}

	if fmtStr, _ := rawObj["format"].(string); fmtStr == "agy-pool-encrypted-v1" {
		if password == "" {
			return nil, errors.New("password required to decrypt backup")
		}
		var bundle EncryptedBundle
		if err := json.Unmarshal(content, &bundle); err != nil {
			return nil, fmt.Errorf("corrupted encrypted bundle format: %w", err)
		}
		plaintext, err := DecryptBundle(&bundle, password)
		if err != nil {
			return nil, fmt.Errorf("decryption failed: %w", err)
		}
		if err := json.Unmarshal(plaintext, &rawObj); err != nil {
			return nil, fmt.Errorf("failed to parse decrypted JSON: %w", err)
		}
	}

	rawAccounts, ok := rawObj["accounts"].([]any)
	if !ok {
		return nil, errors.New("invalid backup format: missing 'accounts' list")
	}

	if len(rawAccounts) == 0 {
		pool, _ := storage.LoadPool()
		total := 0
		if pool != nil {
			total = len(pool.Accounts)
		}
		return &ImportResult{Added: 0, Updated: 0, Skipped: 0, Total: total}, nil
	}

	importedStrategy, _ := rawObj["strategy"].(string)
	targetActiveEmail, _ := rawObj["active_account_email"].(string)

	var result ImportResult
	err = storage.PoolTransaction(func(pool *storage.Pool) error {
		if replace {
			newList := make([]*storage.Account, 0, len(rawAccounts))
			for i, item := range rawAccounts {
				accMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				email, _ := accMap["email"].(string)
				rf, _ := accMap["refresh_token"].(string)
				if email == "" || rf == "" {
					continue
				}

				name, _ := accMap["name"].(string)
				at, _ := accMap["access_token"].(string)
				var expiry *float64
				if expVal, ok := accMap["token_expiry"].(float64); ok {
					expiry = &expVal
				}
				var createdAt *int64
				if crVal, ok := accMap["created_at"].(float64); ok {
					crInt := int64(crVal)
					createdAt = &crInt
				} else {
					now := time.Now().Unix()
					createdAt = &now
				}

				var reqCount int64
				if rcVal, ok := accMap["request_count"].(float64); ok {
					reqCount = int64(rcVal)
				}
				var genCount *int64
				if gcVal, ok := accMap["gen_count"].(float64); ok {
					gcInt := int64(gcVal)
					genCount = &gcInt
				}

				newAcc := &storage.Account{
					ID:           fmt.Sprintf("acc_%d", i+1),
					Email:        email,
					Name:         name,
					RefreshToken: rf,
					AccessToken:  at,
					TokenExpiry:  expiry,
					CreatedAt:    createdAt,
					RequestCount: reqCount,
					GenCount:     genCount,
					ErrorCount:   0,
				}
				newList = append(newList, newAcc)
				result.Added++
			}

			pool.Accounts = newList
			if importedStrategy != "" {
				pool.Strategy = importedStrategy
			}
			var activeAcc *storage.Account
			if targetActiveEmail != "" {
				for _, a := range newList {
					if a.Email == targetActiveEmail {
						activeAcc = a
						break
					}
				}
			}
			if activeAcc == nil && len(newList) > 0 {
				activeAcc = newList[0]
			}
			if activeAcc != nil {
				pool.ActiveAccountID = &activeAcc.ID
			} else {
				pool.ActiveAccountID = nil
			}
			pool.RoundRobinLastAccountID = nil
			result.Total = len(pool.Accounts)
			return nil
		}

		// Merge mode
		emailMap := make(map[string]*storage.Account)
		for _, a := range pool.Accounts {
			if a.Email != "" {
				emailMap[a.Email] = a
			}
		}

		for _, item := range rawAccounts {
			accMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			email, _ := accMap["email"].(string)
			rf, _ := accMap["refresh_token"].(string)
			if email == "" || rf == "" {
				continue
			}

			if existing, exists := emailMap[email]; exists {
				if skipExisting {
					result.Skipped++
					continue
				}
				existing.RefreshToken = rf
				if at, ok := accMap["access_token"].(string); ok && at != "" {
					existing.AccessToken = at
					if expVal, ok := accMap["token_expiry"].(float64); ok {
						existing.TokenExpiry = &expVal
					}
				}
				if name, ok := accMap["name"].(string); ok && name != "" && existing.Name == "" {
					existing.Name = name
				}
				existing.Status = ""
				existing.RateLimitedUntil = nil
				result.Updated++
			} else {
				nextID := storage.NextAccountID(pool.Accounts)
				name, _ := accMap["name"].(string)
				at, _ := accMap["access_token"].(string)
				var expiry *float64
				if expVal, ok := accMap["token_expiry"].(float64); ok {
					expiry = &expVal
				}
				now := time.Now().Unix()
				newAcc := &storage.Account{
					ID:           nextID,
					Email:        email,
					Name:         name,
					RefreshToken: rf,
					AccessToken:  at,
					TokenExpiry:  expiry,
					CreatedAt:    &now,
					RequestCount: 0,
					ErrorCount:   0,
				}
				pool.Accounts = append(pool.Accounts, newAcc)
				emailMap[email] = newAcc
				result.Added++
			}
		}

		result.Total = len(pool.Accounts)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &result, nil
}
