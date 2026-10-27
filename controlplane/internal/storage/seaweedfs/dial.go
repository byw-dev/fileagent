package seaweedfs

// dial.go — the production FilerClient over the vendored filerpb stubs (D-038:
// wire format only, no upstream module). This file is the ONLY place outside
// the filerpb package itself that touches the generated client, and it stays
// inside internal/storage/seaweedfs per the import boundary (boundary_test.go
// Rule A): cmd/server wiring sees only FilerClient / DialFunc.

import (
	"context"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// maxSubscribeRecvMsgSize bounds one SubscribeMetadataResponse. A backlog
// batch is packed into a single response (client_supports_batching), so the
// floor must be generous — the research prototype used the same 64MiB after
// measuring real batch sizes; a batch larger than this fails the stream,
// which the loop then resubscribes into (and would hit again — hence the
// large bound rather than a small one).
const maxSubscribeRecvMsgSize = 64 << 20

// NewGRPCDialer returns a DialFunc for plaintext filer gRPC connections, one
// connection per subscription attempt (a broken proxy/TCP path must not pin a
// dead connection; the loop dials fresh on every attempt and closes on exit).
//
// Plaintext only: filer gRPC has no token auth, only mTLS (D-037 known
// constraint), and mTLS is B7. Config.Validate refuses to start the adapter
// unless SEAWEEDFS_FILER_INSECURE=true was set explicitly, so reaching this
// constructor means the deployment opted into plaintext knowingly.
func NewGRPCDialer(addr string) DialFunc {
	return func(ctx context.Context) (FilerClient, func(), error) {
		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSubscribeRecvMsgSize)),
		)
		if err != nil {
			return nil, nil, err
		}
		return &grpcFilerClient{c: filerpb.NewSeaweedFilerClient(conn)}, func() { _ = conn.Close() }, nil
	}
}

// grpcFilerClient adapts the generated filerpb client to the adapter's narrow
// FilerClient interface (stream types satisfy it structurally via Recv).
type grpcFilerClient struct {
	c filerpb.SeaweedFilerClient
}

// SubscribeLocalMetadata opens the local metadata stream.
func (g *grpcFilerClient) SubscribeLocalMetadata(ctx context.Context, in *filerpb.SubscribeMetadataRequest) (FilerStream, error) {
	return g.c.SubscribeLocalMetadata(ctx, in)
}

// ListEntries lists directory entries (the startup self-check's earliest-log
// probe).
func (g *grpcFilerClient) ListEntries(ctx context.Context, in *filerpb.ListEntriesRequest) (ListEntriesStream, error) {
	return g.c.ListEntries(ctx, in)
}
