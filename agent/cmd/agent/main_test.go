package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type mockTokenSetter struct {
	token string
}

func (m *mockTokenSetter) SetToken(token string) { m.token = token }

func TestHandleRevokeCommand_ClearsCredentialsAndStops(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.enc")
	tokenMgr := credential.NewTokenManager(tokenPath, "machine-id")
	require.NoError(t, tokenMgr.Save("jwt-token"))

	stsMgr := credential.NewSTSManager()
	stsMgr.SetSTS(&credential.STSCredentials{Expiry: time.Now().Add(time.Hour)})

	client := &mockTokenSetter{token: "jwt-token"}
	stopped := false
	stop := func() { stopped = true }

	handleRevokeCommand(tokenMgr, stsMgr, client, stop, zap.NewNop(), "manual revoke")

	assert.True(t, stopped)
	assert.Equal(t, "", tokenMgr.Token())
	assert.Nil(t, stsMgr.GetSTS())
	assert.Equal(t, "", client.token)
}
