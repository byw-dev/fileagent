// Package credential manages the Edge Agent's authentication credentials:
//   - Auth Token (JWT): persisted to disk encrypted with AES-256-GCM; the
//     encryption key is derived from a machineID (fingerprint) string.
//   - STS Credentials: kept in memory only; never written to disk.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Auth Token ────────────────────────────────────────────────────────────────

// TokenManager handles encrypted persistence and validity checks for a JWT.
type TokenManager struct {
	path      string
	machineID string

	mu    sync.RWMutex
	token string // cached plaintext JWT
}

// NewTokenManager creates a TokenManager that stores the token at path,
// encrypting it with a key derived from machineID.
func NewTokenManager(path, machineID string) *TokenManager {
	return &TokenManager{path: path, machineID: machineID}
}

// Save encrypts token with AES-256-GCM and writes it to disk.
func (m *TokenManager) Save(token string) error {
	enc, err := encrypt([]byte(token), deriveKey(m.machineID))
	if err != nil {
		return fmt.Errorf("credential: encrypt token: %w", err)
	}
	if err = os.WriteFile(m.path, []byte(enc), 0o600); err != nil {
		return fmt.Errorf("credential: write token file %q: %w", m.path, err)
	}
	m.mu.Lock()
	m.token = token
	m.mu.Unlock()
	return nil
}

// Load reads the token file, decrypts it, and caches the plaintext JWT.
// Returns an error if the file does not exist or decryption fails.
func (m *TokenManager) Load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("credential: read token file %q: %w", m.path, err)
	}
	plain, err := decrypt(strings.TrimSpace(string(data)), deriveKey(m.machineID))
	if err != nil {
		return fmt.Errorf("credential: decrypt token: %w", err)
	}
	m.mu.Lock()
	m.token = plain
	m.mu.Unlock()
	return nil
}

// Clear removes the persisted token file (if present) and clears the cached
// plaintext token from memory.
func (m *TokenManager) Clear() error {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credential: remove token file %q: %w", m.path, err)
	}
	m.mu.Lock()
	m.token = ""
	m.mu.Unlock()
	return nil
}

// Token returns the cached plaintext JWT or an empty string if none is loaded.
func (m *TokenManager) Token() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.token
}

// IsTokenValid returns true if the cached JWT is non-empty, not yet expired,
// and the remaining lifetime represents more than renewThreshold of the total TTL.
//
// renewThreshold=0.2 means "renew when less than 20% of TTL remains".
// When the JWT lacks an iat claim, only expiry is checked (threshold is ignored).
func (m *TokenManager) IsTokenValid(renewThreshold float64) bool {
	m.mu.RLock()
	tok := m.token
	m.mu.RUnlock()

	if tok == "" {
		return false
	}
	exp, iat, hasIat, err := parseJWTTimes(tok)
	if err != nil {
		return false
	}
	now := time.Now()
	if now.After(exp) {
		return false
	}
	if !hasIat {
		// No iat claim — can only verify the token is not expired.
		return true
	}
	ttl := exp.Sub(iat)
	remaining := exp.Sub(now)
	if ttl <= 0 {
		return false
	}
	return float64(remaining)/float64(ttl) > renewThreshold
}

// Identity returns the agent id and name carried by the cached JWT.
//
// The Control Plane mints agent tokens with the agent UUID as "sub" and the
// agent's display name as "username" (see controlplane agent.Manager), so an
// agent that starts from a cached token can recover its own identity without
// re-registering. Before this existed, that startup path left both fields empty
// and every dest_path_template referencing {agent_name} or {agent_id} silently
// fell back to the bare file name (IC-BUG-17).
//
// The claims are read without signature verification: the token was minted by
// the Control Plane and stored locally, and a forged local token would only let
// the agent mislabel its own uploads, which the Control Plane rejects anyway
// because it derives the agent id from the verified token on its side.
func (m *TokenManager) Identity() (agentID, agentName string, err error) {
	m.mu.RLock()
	tok := m.token
	m.mu.RUnlock()

	if tok == "" {
		return "", "", errors.New("no cached token")
	}
	claims, err := parseJWTClaims(tok)
	if err != nil {
		return "", "", err
	}
	if claims.Sub == "" {
		return "", "", errors.New("JWT missing sub claim")
	}
	return claims.Sub, claims.Username, nil
}

// ── STS Credentials ───────────────────────────────────────────────────────────

// STSCredentials holds a set of temporary MinIO / S3 credentials.
type STSCredentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expiry       time.Time
}

// STSManager manages in-memory STS credentials with monotonic generations.
//
// There are two concurrent writers — the periodic refresh goroutine and the
// AccessDenied retry path — both issuing RefreshCredentials RPCs. A response
// that was requested EARLIER but arrives LATER must not overwrite a newer
// session: that would resurrect stale credentials (e.g. ones that do not
// cover a just-added bucket) and turn the AccessDenied retry into a terminal
// failure. SetSTS therefore only accepts a strictly newer generation; stale
// responses are dropped by the caller.
type STSManager struct {
	mu         sync.RWMutex
	cred       *STSCredentials
	generation uint64
}

// NewSTSManager constructs an empty STSManager.
func NewSTSManager() *STSManager {
	return &STSManager{}
}

// SetSTS stores a new set of STS credentials, replacing any existing ones.
// It reports whether the credentials were applied: a generation that is not
// strictly newer than the currently held one is rejected (a stale response
// from a request issued before the current credentials were minted).
func (s *STSManager) SetSTS(cred *STSCredentials, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation <= s.generation {
		return false
	}
	s.cred = cred
	s.generation = generation
	return true
}

// GetSTS returns the current STS credentials, or nil if none are set.
func (s *STSManager) GetSTS() *STSCredentials {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cred
}

// Generation returns the generation of the currently held credentials.
// Monotonically increasing across the manager's lifetime, including across
// Clear — a clear must not lower the water mark, or a stale response arriving
// after the clear would be accepted.
func (s *STSManager) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// Clear removes any in-memory STS credentials. The generation watermark is
// deliberately kept: it must only ever move forward.
func (s *STSManager) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = nil
}

// IsSTSValid returns true if STS credentials are set and will not expire
// within the next 10 minutes.
func (s *STSManager) IsSTSValid() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cred == nil {
		return false
	}
	return time.Until(s.cred.Expiry) > 10*time.Minute
}

// ── Crypto helpers ────────────────────────────────────────────────────────────

// deriveKey produces a 32-byte AES-256 key from an arbitrary string using
// SHA-256.  The same machineID always produces the same key, which is the
// desired property for deterministic encryption of the token file.
func deriveKey(machineID string) []byte {
	sum := sha256.Sum256([]byte(machineID))
	return sum[:]
}

// encrypt encrypts plaintext with AES-256-GCM and returns a base64-encoded
// string in the format "<nonce_b64>.<ciphertext_b64>".
func encrypt(plaintext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(nonce) + "." +
		base64.StdEncoding.EncodeToString(ct), nil
}

// decrypt is the inverse of encrypt.
func decrypt(encoded string, key []byte) (string, error) {
	parts := strings.SplitN(encoded, ".", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid ciphertext format")
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(plain), nil
}

// ── JWT parsing ───────────────────────────────────────────────────────────────

// jwtClaims is a minimal subset of standard JWT claims used for validity checks.
type jwtClaims struct {
	Exp      int64  `json:"exp"`
	Iat      int64  `json:"iat"`
	Sub      string `json:"sub"`
	Username string `json:"username"`
}

// parseJWTTimes base64-decodes the JWT payload section and extracts the
// "exp" and "iat" fields without performing any signature verification.
// hasIat is false when the "iat" claim is absent (zero value after unmarshal).
func parseJWTTimes(token string) (exp, iat time.Time, hasIat bool, err error) {
	claims, err := parseJWTClaims(token)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if claims.Exp == 0 {
		return time.Time{}, time.Time{}, false, errors.New("JWT missing exp claim")
	}
	return time.Unix(claims.Exp, 0), time.Unix(claims.Iat, 0), claims.Iat != 0, nil
}

// parseJWTClaims base64-decodes the JWT payload section without performing any
// signature verification.
func parseJWTClaims(token string) (jwtClaims, error) {
	var claims jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, fmt.Errorf("decode JWT payload: %w", err)
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("unmarshal JWT claims: %w", err)
	}
	return claims, nil
}
