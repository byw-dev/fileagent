package credential

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// makeJWT builds a syntactically valid (but unsigned) JWT with the given exp/iat.
func makeJWT(exp, iat int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]int64{"exp": exp, "iat": iat})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return strings.Join([]string{header, payload, "fakesig"}, ".")
}

// tokenFile returns a temp file path for a token.
func tokenFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "token.enc")
}

// ── AES encrypt / decrypt roundtrip ──────────────────────────────────────────

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := deriveKey("machine-id-abc")
	plaintext := []byte("super-secret-jwt-token")

	enc, err := encrypt(plaintext, key)
	require.NoError(t, err)
	assert.NotEmpty(t, enc)
	assert.Contains(t, enc, ".") // nonce.ciphertext format

	dec, err := decrypt(enc, key)
	require.NoError(t, err)
	assert.Equal(t, string(plaintext), dec)
}

func TestEncryptDecrypt_WrongKey(t *testing.T) {
	key1 := deriveKey("machine-1")
	key2 := deriveKey("machine-2")

	enc, err := encrypt([]byte("payload"), key1)
	require.NoError(t, err)

	_, err = decrypt(enc, key2)
	require.Error(t, err)
}

func TestDecrypt_InvalidFormat(t *testing.T) {
	_, err := decrypt("not-valid-format", deriveKey("id"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ciphertext format")
}

// ── TokenManager ─────────────────────────────────────────────────────────────

func TestTokenManager_SaveLoad(t *testing.T) {
	path := tokenFile(t)
	mgr := NewTokenManager(path, "machine-id-test")

	now := time.Now()
	jwt := makeJWT(now.Add(2*time.Hour).Unix(), now.Unix())

	require.NoError(t, mgr.Save(jwt))

	// Load via a fresh manager (simulates restart).
	mgr2 := NewTokenManager(path, "machine-id-test")
	require.NoError(t, mgr2.Load())
	assert.Equal(t, jwt, mgr2.Token())
}

func TestTokenManager_LoadMissingFile(t *testing.T) {
	mgr := NewTokenManager("/nonexistent/token.enc", "id")
	err := mgr.Load()
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err) || strings.Contains(err.Error(), "no such file"))
}

func TestTokenManager_Save_WrongKeyOnLoad(t *testing.T) {
	path := tokenFile(t)
	// Save with one machine ID.
	mgr1 := NewTokenManager(path, "machine-A")
	require.NoError(t, mgr1.Save("my-token"))

	// Load with a different machine ID → decryption fails.
	mgr2 := NewTokenManager(path, "machine-B")
	err := mgr2.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt token")
}

// ── IsTokenValid ──────────────────────────────────────────────────────────────

func TestIsTokenValid_Valid(t *testing.T) {
	now := time.Now()
	// Token issued now, expires in 1 hour → well within 20% threshold.
	jwt := makeJWT(now.Add(time.Hour).Unix(), now.Unix())

	mgr := NewTokenManager(tokenFile(t), "id")
	mgr.mu.Lock()
	mgr.token = jwt
	mgr.mu.Unlock()

	assert.True(t, mgr.IsTokenValid(0.2))
}

func TestIsTokenValid_ExpiringSoon(t *testing.T) {
	now := time.Now()
	// Issued 55 min ago, expires in 5 min → 5/60 ≈ 8% remaining < 20% threshold.
	iat := now.Add(-55 * time.Minute)
	exp := now.Add(5 * time.Minute)
	jwt := makeJWT(exp.Unix(), iat.Unix())

	mgr := NewTokenManager(tokenFile(t), "id")
	mgr.mu.Lock()
	mgr.token = jwt
	mgr.mu.Unlock()

	assert.False(t, mgr.IsTokenValid(0.2))
}

func TestIsTokenValid_Expired(t *testing.T) {
	now := time.Now()
	jwt := makeJWT(now.Add(-1*time.Second).Unix(), now.Add(-time.Hour).Unix())

	mgr := NewTokenManager(tokenFile(t), "id")
	mgr.mu.Lock()
	mgr.token = jwt
	mgr.mu.Unlock()

	assert.False(t, mgr.IsTokenValid(0.2))
}

func TestIsTokenValid_EmptyToken(t *testing.T) {
	mgr := NewTokenManager(tokenFile(t), "id")
	assert.False(t, mgr.IsTokenValid(0.2))
}

func TestIsTokenValid_InvalidJWT(t *testing.T) {
	mgr := NewTokenManager(tokenFile(t), "id")
	mgr.mu.Lock()
	mgr.token = "not.a.jwt.with.five.parts"
	mgr.mu.Unlock()
	assert.False(t, mgr.IsTokenValid(0.2))
}

// ── STS ───────────────────────────────────────────────────────────────────────

func TestSTSManager_SetGet(t *testing.T) {
	mgr := NewSTSManager()
	assert.Nil(t, mgr.GetSTS())

	cred := &STSCredentials{
		AccessKey:    "AKIA...",
		SecretKey:    "secret",
		SessionToken: "token",
		Expiry:       time.Now().Add(time.Hour),
	}
	mgr.SetSTS(cred)
	got := mgr.GetSTS()
	require.NotNil(t, got)
	assert.Equal(t, cred.AccessKey, got.AccessKey)
}

func TestSTSManager_IsSTSValid_Fresh(t *testing.T) {
	mgr := NewSTSManager()
	mgr.SetSTS(&STSCredentials{Expiry: time.Now().Add(time.Hour)})
	assert.True(t, mgr.IsSTSValid())
}

func TestSTSManager_IsSTSValid_ExpiringInUnderTenMin(t *testing.T) {
	mgr := NewSTSManager()
	mgr.SetSTS(&STSCredentials{Expiry: time.Now().Add(9 * time.Minute)})
	assert.False(t, mgr.IsSTSValid())
}

func TestSTSManager_IsSTSValid_Expired(t *testing.T) {
	mgr := NewSTSManager()
	mgr.SetSTS(&STSCredentials{Expiry: time.Now().Add(-time.Minute)})
	assert.False(t, mgr.IsSTSValid())
}

func TestSTSManager_IsSTSValid_NoCredentials(t *testing.T) {
	mgr := NewSTSManager()
	assert.False(t, mgr.IsSTSValid())
}
