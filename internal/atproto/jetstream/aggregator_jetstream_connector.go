package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AggregatorJetstreamConnector handles WebSocket connection to Jetstream for aggregator events
type AggregatorJetstreamConnector struct {
	consumer *AggregatorEventConsumer
	wsURL    string
}

// NewAggregatorJetstreamConnector creates a new Jetstream WebSocket connector for aggregator events
func NewAggregatorJetstreamConnector(consumer *AggregatorEventConsumer, wsURL string) *AggregatorJetstreamConnector {
	return &AggregatorJetstreamConnector{
		consumer: consumer,
		wsURL:    wsURL,
	}
}

// Start begins consuming events from Jetstream
// Runs indefinitely, reconnecting on errors
func (c *AggregatorJetstreamConnector) Start(ctx context.Context) error {
	log.Printf("Starting Jetstream aggregator consumer: %s", c.wsURL)

	for {
		select {
		case <-ctx.Done():
			log.Println("Jetstream aggregator consumer shutting down")
			return ctx.Err()
		default:
			if err := c.connect(ctx); err != nil {
				log.Printf("Jetstream aggregator connection error: %v. Retrying in 5s...", err)
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}
}

// connect establishes WebSocket connection and processes events
func (c *AggregatorJetstreamConnector) connect(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("Failed to close WebSocket connection: %v", closeErr)
		}
	}()

	log.Println("Connected to Jetstream (aggregator consumer)")

	// Set read deadline to detect connection issues
	if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		log.Printf("Failed to set read deadline: %v", err)
	}

	// Set pong handler to keep connection alive
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			log.Printf("Failed to set read deadline in pong handler: %v", err)
		}
		return nil
	})

	// Start ping ticker
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	var closeOnce sync.Once // Ensure done channel is only closed once

	// Goroutine to send pings
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Ping error: %v", err)
					closeOnce.Do(func() { close(done) })
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Read messages
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return fmt.Errorf("connection closed")
		default:
			_, message, err := conn.ReadMessage()
			if err != nil {
				closeOnce.Do(func() { close(done) })
				return fmt.Errorf("read error: %w", err)
			}

			// Reset read deadline on successful read
			if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				log.Printf("Failed to set read deadline: %v", err)
			}

			if err := c.handleEvent(ctx, message); err != nil {
				log.Printf("Error handling aggregator event: %v", err)
				// Continue processing other events
			}
		}
	}
}

// handleEvent processes a single Jetstream event
func (c *AggregatorJetstreamConnector) handleEvent(ctx context.Context, data []byte) error {
	var event JetstreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("failed to parse event: %w", err)
	}

	// Pass to consumer's HandleEvent method
	return c.consumer.HandleEvent(ctx, &event)
}
