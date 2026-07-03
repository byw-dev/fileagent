// Package grpcclient provides a gRPC client for the Edge Agent to communicate
// with the Control Plane. It handles TLS configuration, automatic reconnection
// with exponential backoff, and periodic heartbeat sending.
package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/config"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	heartbeatInterval = 30 * time.Second
	backoffBase       = time.Second
	backoffMax        = 60 * time.Second
	backoffFactor     = 2.0
)

// Client manages a gRPC connection to the Control Plane and the long-lived
// bidirectional Connect stream.
type Client struct {
	cfg    *config.Config
	logger *zap.Logger

	mu         sync.Mutex
	conn       *grpc.ClientConn
	svc        agentv1.AgentServiceClient
	stream     agentv1.AgentService_ConnectClient
	token      string
	agentID    string
	msgHandler func(*agentv1.ServerMessage)
	reauthFunc func(context.Context) (string, error)
}

// New constructs a Client. Call Connect to establish the connection.
func New(cfg *config.Config, logger *zap.Logger) *Client {
	return &Client{
		cfg:    cfg,
		logger: logger,
	}
}

// SetToken stores the Bearer JWT that will be attached to outgoing RPCs.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// SetAgentID stores the agent identifier used in outgoing RPCs such as RefreshCredentials.
func (c *Client) SetAgentID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentID = id
}

// SetMessageHandler registers a callback invoked for every ServerMessage received
// from the Control Plane. The handler is called synchronously in the receive loop,
// so heavy work should be dispatched to a goroutine.
func (c *Client) SetMessageHandler(h func(*agentv1.ServerMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgHandler = h
}

// SetReauthFunc registers a callback used to obtain a fresh Bearer token when
// the Control Plane rejects the current one (gRPC Unauthenticated). The callback
// typically re-runs the approval poll (which is exempt from JWT auth) to mint a
// new agent token. It is optional; when unset, the client only retries with the
// existing token.
func (c *Client) SetReauthFunc(f func(context.Context) (string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reauthFunc = f
}

// ServiceClient returns the underlying AgentServiceClient after Connect has been called.
func (c *Client) ServiceClient() agentv1.AgentServiceClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.svc
}

// SendMessage writes an AgentMessage to the current stream.
func (c *Client) SendMessage(msg *agentv1.AgentMessage) error {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if stream == nil {
		return fmt.Errorf("grpcclient: no active stream")
	}
	if err := stream.Send(msg); err != nil {
		return fmt.Errorf("grpcclient: send message: %w", err)
	}
	return nil
}

// RefreshCredentials calls the Control Plane to obtain fresh STS credentials.
func (c *Client) RefreshCredentials(ctx context.Context) (*agentv1.CredentialsPayload, error) {
	c.mu.Lock()
	svc := c.svc
	tok := c.token
	id := c.agentID
	c.mu.Unlock()

	if svc == nil {
		return nil, fmt.Errorf("grpcclient: service client not initialised")
	}

	outCtx := ctx
	if tok != "" {
		outCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}

	resp, err := svc.RefreshCredentials(outCtx, &agentv1.RefreshCredentialsRequest{
		AgentId: id,
	})
	if err != nil {
		return nil, fmt.Errorf("grpcclient: refresh credentials: %w", err)
	}
	return resp.GetCredentials(), nil
}

// Connect dials the Control Plane and opens the bidirectional Connect stream,
// then starts a heartbeat goroutine. It blocks until ctx is cancelled,
// reconnecting with exponential backoff on every failure.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	hasSvc := c.svc != nil
	c.mu.Unlock()
	if !hasSvc {
		if err := c.Dial(); err != nil {
			return err
		}
	}

	go c.runLoop(ctx)
	return nil
}

// Dial initialises the underlying gRPC connection and service client without
// starting the long-lived Connect stream loop.
func (c *Client) Dial() error {
	dialOpts, err := c.buildDialOpts()
	if err != nil {
		return fmt.Errorf("grpcclient: build dial options: %w", err)
	}

	conn, err := grpc.NewClient(c.cfg.Server.Endpoint, dialOpts...)
	if err != nil {
		return fmt.Errorf("grpcclient: dial %q: %w", c.cfg.Server.Endpoint, err)
	}

	c.mu.Lock()
	c.conn = conn
	c.svc = agentv1.NewAgentServiceClient(conn)
	c.mu.Unlock()

	return nil
}

// Close tears down the gRPC connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// SendHeartbeat writes a heartbeat AgentMessage to the current stream.
// It returns an error when no stream is open.
func (c *Client) SendHeartbeat(hb *agentv1.Heartbeat) error {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()

	if stream == nil {
		return fmt.Errorf("grpcclient: no active stream")
	}
	msg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Heartbeat{Heartbeat: hb},
	}
	if err := stream.Send(msg); err != nil {
		return fmt.Errorf("grpcclient: send heartbeat: %w", err)
	}
	return nil
}

// runLoop maintains the Connect stream and heartbeat goroutine, reconnecting
// with exponential backoff on failures until ctx is cancelled.
func (c *Client) runLoop(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		if attempt > 0 {
			delay := backoffDelay(attempt)
			c.logger.Info("grpcclient: reconnecting", zap.Int("attempt", attempt), zap.Duration("delay", delay))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		attempt++

		streamErr := c.openStream(ctx)
		if streamErr == nil {
			// Stream opened successfully — reset backoff counter.
			attempt = 0

			streamCtx, cancel := context.WithCancel(ctx)
			go c.heartbeatLoop(streamCtx)
			streamErr = c.receiveLoop(streamCtx)
			cancel()
		}

		if streamErr != nil {
			c.logger.Warn("grpcclient: stream error", zap.Error(streamErr))
			// A rejected token means the stored credential is expired or revoked.
			// Retrying with the same token would loop forever, so try to obtain a
			// fresh one before the next attempt (self-heal). See G-2 / 06 E-1.
			if status.Code(streamErr) == codes.Unauthenticated {
				c.reauthenticate(ctx)
			}
		}
	}
}

// reauthenticate invokes the registered reauth callback (if any) to obtain a
// fresh Bearer token and installs it for subsequent reconnect attempts. Failures
// are logged and swallowed: the run loop keeps retrying under backoff.
func (c *Client) reauthenticate(ctx context.Context) {
	c.mu.Lock()
	f := c.reauthFunc
	c.mu.Unlock()
	if f == nil {
		return
	}
	token, err := f(ctx)
	if err != nil {
		c.logger.Warn("grpcclient: reauthentication failed", zap.Error(err))
		return
	}
	if token == "" {
		// Defensive: never install an empty token. Doing so would drop the
		// Bearer header entirely and turn a recoverable auth error into
		// repeated anonymous Unauthenticated reconnects.
		c.logger.Warn("grpcclient: reauth returned empty token, keeping existing credential")
		return
	}
	c.SetToken(token)
	c.logger.Info("grpcclient: reauthenticated, refreshed token for reconnect")
}

// openStream calls Connect on the gRPC service and stores the resulting stream.
func (c *Client) openStream(ctx context.Context) error {
	c.mu.Lock()
	svc := c.svc
	tok := c.token
	c.mu.Unlock()

	if svc == nil {
		return fmt.Errorf("grpcclient: service client not initialised")
	}

	outCtx := ctx
	if tok != "" {
		outCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}

	stream, err := svc.Connect(outCtx)
	if err != nil {
		return fmt.Errorf("grpcclient: open Connect stream: %w", err)
	}

	c.mu.Lock()
	c.stream = stream
	c.mu.Unlock()

	c.logger.Info("grpcclient: stream established")
	return nil
}

// receiveLoop reads server messages from the stream until it closes or errors,
// dispatching each message to the registered handler (if any). It returns the
// terminal stream error (nil when the loop exits due to context cancellation),
// so the caller can react to auth rejections.
func (c *Client) receiveLoop(ctx context.Context) error {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()

	for {
		if ctx.Err() != nil {
			return nil
		}
		msg, err := stream.Recv()
		if err != nil {
			c.logger.Warn("grpcclient: stream recv error", zap.Error(err))
			c.mu.Lock()
			c.stream = nil
			c.mu.Unlock()
			return err
		}
		c.mu.Lock()
		h := c.msgHandler
		c.mu.Unlock()
		if h != nil {
			h(msg)
		}
	}
}

// heartbeatLoop sends a heartbeat every heartbeatInterval until ctx is cancelled.
func (c *Client) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb := &agentv1.Heartbeat{}
			if err := c.SendHeartbeat(hb); err != nil {
				c.logger.Warn("grpcclient: heartbeat failed", zap.Error(err))
				return
			}
			c.logger.Debug("grpcclient: heartbeat sent")
		}
	}
}

// buildDialOpts constructs the gRPC dial options based on the configuration.
// If TLSCACert is set, it loads the CA certificate for server verification;
// otherwise, the system root CA pool is used. No mTLS in v1.
func (c *Client) buildDialOpts() ([]grpc.DialOption, error) {
	if c.cfg.Server.TLSCACert == "" {
		// Use system cert pool with default TLS settings.
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}, nil
	}

	pemData, err := os.ReadFile(c.cfg.Server.TLSCACert)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: read CA cert %q: %w", c.cfg.Server.TLSCACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("grpcclient: parse CA cert %q: no certificates found", c.cfg.Server.TLSCACert)
	}
	tlsCfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}, nil
}

// InsecureDialOpts returns dial options without TLS, intended for testing only.
func InsecureDialOpts() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
}

// backoffDelay computes the exponential backoff delay for the nth attempt,
// capped at backoffMax.
func backoffDelay(attempt int) time.Duration {
	d := float64(backoffBase) * math.Pow(backoffFactor, float64(attempt-1))
	if d > float64(backoffMax) {
		return backoffMax
	}
	return time.Duration(d)
}
