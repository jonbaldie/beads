package dolt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Credential storage and encryption for federation peers.
// Enables SQL user authentication when syncing with peer workspaces.

// credentialKeyFile is the filename for the random encryption key stored alongside the database.
const credentialKeyFile = ".beads-credential-key" //nolint:gosec // G101: not a credential, just a filename

const awsResponseChecksumValidationEnv = "AWS_RESPONSE_CHECKSUM_VALIDATION"

// federationEnvMutex protects process-wide env vars from concurrent access.
// Environment variables are process-global, so we need to serialize federation operations.
var federationEnvMutex sync.Mutex

// validPeerNameRegex matches valid peer names (alphanumeric, hyphens, underscores)
var validPeerNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// validatePeerName checks that a peer name is safe for use as a Dolt remote name
func validatePeerName(name string) error {
	if name == "" {
		return fmt.Errorf("peer name cannot be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("peer name too long (max 64 characters)")
	}
	if !validPeerNameRegex.MatchString(name) {
		return fmt.Errorf("peer name must start with a letter and contain only alphanumeric characters, hyphens, and underscores")
	}
	return nil
}

// initCredentialKey loads or generates the credential encryption key.
// The key file is stored in .beads/ (beadsDir), NOT in .beads/dolt/ (dbPath),
// to avoid creating ghost directories in shared-server mode (GH bd-cby).
// Falls back to the old dbPath location for transparent migration.
func (s *DoltStore) initCredentialKey(ctx context.Context) error {
	if s.beadsDir == "" {
		return nil // No filesystem path — credential encryption unavailable
	}

	keyPath := filepath.Join(s.beadsDir, credentialKeyFile)
	if key, ok := loadCredentialKey(keyPath); ok {
		s.credentialKey = key
		return nil
	}

	if s.dbPath != "" {
		oldKeyPath := filepath.Join(s.dbPath, credentialKeyFile)
		if oldKey, ok := loadCredentialKey(oldKeyPath); ok {
			migrateCredentialKeyFile(keyPath, oldKeyPath, oldKey)
			s.credentialKey = oldKey
			return nil
		}
	}

	key, err := generateCredentialKey()
	if err != nil {
		return fmt.Errorf("failed to generate credential encryption key: %w", err)
	}
	if err := s.migrateCredentialKeys(ctx, key); err != nil {
		return fmt.Errorf("failed to migrate credential keys: %w", err)
	}
	if err := persistCredentialKey(s.beadsDir, keyPath, key); err != nil {
		return err
	}
	s.credentialKey = key
	return nil
}

func loadCredentialKey(path string) ([]byte, bool) {
	key, err := os.ReadFile(path) //nolint:gosec // G304: caller derives paths from trusted directories
	return key, err == nil && len(key) == 32
}

func migrateCredentialKeyFile(newPath, oldPath string, key []byte) {
	if err := os.WriteFile(newPath, key, 0600); err == nil {
		_ = os.Remove(oldPath)
	}
}

func generateCredentialKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, key)
	return key, err
}

func persistCredentialKey(beadsDir, keyPath string, key []byte) error {
	if err := os.MkdirAll(beadsDir, 0700); err != nil {
		return fmt.Errorf("failed to create beads directory %s: %w", beadsDir, err)
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		return fmt.Errorf("failed to write credential key file: %w", err)
	}
	return nil
}

// ensureCredentialKey lazily initializes the credential key when federation
// operations actually need password encryption or decryption.
func (s *DoltStore) ensureCredentialKey(ctx context.Context) error {
	s.mu.RLock()
	if doltCredentialKeyAvailable(s) {
		s.mu.RUnlock()
		return nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.credentialKey != nil {
		return nil
	}
	return s.initCredentialKey(ctx)
}

// legacyEncryptionKey derives the old predictable key from dbPath.
// Used only during migration from the old key derivation scheme.
func (s *DoltStore) legacyEncryptionKey() []byte {
	h := sha256.New()
	h.Write([]byte(s.dbPath + "beads-federation-key-v1"))
	return h.Sum(nil)
}

// migrateCredentialKeys re-encrypts all stored federation passwords from the
// old dbPath-derived key to the new random key.
func (s *DoltStore) migrateCredentialKeys(ctx context.Context, newKey []byte) error {
	if s.db == nil {
		return nil // No database connection — nothing to migrate
	}

	entries, err := s.readCredentialMigrationEntries(ctx, s.legacyEncryptionKey())
	if err != nil {
		return err
	}
	return s.writeCredentialMigrationEntries(ctx, entries, newKey)
}

type credentialMigrationEntry struct {
	name      string
	plaintext string
}

func (s *DoltStore) readCredentialMigrationEntries(ctx context.Context, oldKey []byte) ([]credentialMigrationEntry, error) {
	rows, err := s.queryContext(ctx, `
		SELECT name, password_encrypted FROM federation_peers
		WHERE password_encrypted IS NOT NULL AND LENGTH(password_encrypted) > 0
	`)
	if err != nil {
		// Table may not exist yet (fresh install) — not an error
		return nil, nil
	}
	defer rows.Close()
	var toMigrate []credentialMigrationEntry
	for rows.Next() {
		var name string
		var encrypted []byte
		if err := rows.Scan(&name, &encrypted); err != nil {
			return nil, fmt.Errorf("failed to scan peer for migration: %w", err)
		}

		// Decrypt with old key
		plaintext, err := decryptWithKey(encrypted, oldKey)
		if err != nil {
			// Can't decrypt with old key — skip (may already use a different scheme)
			continue
		}
		toMigrate = append(toMigrate, credentialMigrationEntry{name: name, plaintext: plaintext})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate peers for migration: %w", err)
	}
	return toMigrate, nil
}

func (s *DoltStore) writeCredentialMigrationEntries(ctx context.Context, entries []credentialMigrationEntry, newKey []byte) error {
	for _, entry := range entries {
		encrypted, err := encryptWithKey(entry.plaintext, newKey)
		if err != nil {
			return fmt.Errorf("failed to re-encrypt password for peer %s: %w", entry.name, err)
		}
		if _, err := s.execContext(ctx, `
			UPDATE federation_peers SET password_encrypted = ? WHERE name = ?
		`, encrypted, entry.name); err != nil {
			return fmt.Errorf("failed to update encrypted password for peer %s: %w", entry.name, err)
		}
	}

	return nil
}

// encryptWithKey encrypts plaintext using AES-GCM with the given key.
func encryptWithKey(plaintext string, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// decryptWithKey decrypts ciphertext using AES-GCM with the given key.
func decryptWithKey(encrypted []byte, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(encrypted) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := encrypted[:nonceSize], encrypted[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// encryptPassword encrypts a password using AES-GCM with the store's credential key.
func (s *DoltStore) encryptPassword(password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	s.mu.RLock()
	key := s.credentialKey
	s.mu.RUnlock()
	if key == nil {
		return nil, fmt.Errorf("credential encryption key not initialized")
	}
	return encryptWithKey(password, key)
}

// decryptPassword decrypts a password using AES-GCM with the store's credential key.
func (s *DoltStore) decryptPassword(encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}
	s.mu.RLock()
	key := s.credentialKey
	s.mu.RUnlock()
	if key == nil {
		return "", fmt.Errorf("credential encryption key not initialized")
	}
	return decryptWithKey(encrypted, key)
}
