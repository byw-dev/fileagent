// Package event provides event publication and delivery for the Control Plane.
package event

import (
	"encoding/json"
	"fmt"
)

// NATSConn is the minimal NATS interface needed by the event publisher.
type NATSConn interface {
	Publish(subject string, data []byte) error
}

// Publisher wraps a NATS connection and publishes structured events.
type Publisher struct {
	conn NATSConn
}

// NewPublisher creates a new event Publisher.
func NewPublisher(conn NATSConn) *Publisher {
	return &Publisher{conn: conn}
}

// Publish serialises payload to JSON and publishes it to the given subject.
func (p *Publisher) Publish(subject string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event publisher: marshal payload: %w", err)
	}
	return p.conn.Publish(subject, data)
}
