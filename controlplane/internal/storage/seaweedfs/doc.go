// Package seaweedfs is the pure protocol layer of the SeaweedFS storage-event
// adapter (D-037 / D-038).
//
// It translates filer metadata-subscription responses
// (filerpb.SubscribeMetadataResponse, generated from the vendored
// proto/third_party/seaweedfs/filer.proto) into the adapter's internal Change
// values, and holds the cursor arithmetic for resuming a subscription.
//
// This package intentionally contains no I/O: no gRPC dialing, no subscription
// loop, no NATS publishing. Wiring it into the control plane (event source,
// reconnect loop, mTLS) is PR3b. Only this package (and its filerpb
// subpackage) may import the SeaweedFS protocol types.
package seaweedfs
