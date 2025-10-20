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

// PostJetstreamConnector handles WebSocket connection to Jetstream for post events
type PostJetstreamConnector struct {
	consumer *PostEventConsumer
	wsURL    string
}

// NewPostJetstreamConnector creates a new Jetstream WebSocket connector for post events
func NewPostJetstreamConnector(consumer *PostEventConsumer, wsURL string) *PostJetstreamConnector {
	return &PostJetstreamConnector{
		consumer: consumer,
		wsURL:    wsURL,
	}
}

// Start begins consuming events from Jetstream
// Runs indefinitely, reconnecting on errors
func (c *PostJetstreamConnector) Start(ctx context.Context) error {
	log.Printf("Starting Jetstream post consumer: %s", c.wsURL)

	for {
		select {
		case <-ctx.Done():
			log.Println("Jetstream post consumer shutting down")
			return ctx.Err()
		default:
			if err := c.connect(ctx); err != nil {
				log.Printf("Jetstream post connection error: %v. Retrying in 5s...", err)
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}
}

// connect establishes WebSocket connection and processes events
func (c *PostJetstreamConnector) connect(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Jetstream: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("Failed to close WebSocket connection: %v", closeErr)
		}
	}()

	log.Println("Connected to Jetstream (post consumer)")

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

	// Ping goroutine
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("Failed to send ping: %v", err)
					closeOnce.Do(func() { close(done) })
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Read loop
	for {
		select {
		case <-done:
			return fmt.Errorf("connection closed by ping failure")
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			closeOnce.Do(func() { close(done) })
			return fmt.Errorf("read error: %w", err)
		}

		// Parse Jetstream event
		var event JetstreamEvent
		if err := json.Unmarshal(message, &event); err != nil {
			log.Printf("Failed to parse Jetstream event: %v", err)
			continue
		}

		// Process event through consumer
		if err := c.consumer.HandleEvent(ctx, &event); err != nil {
			log.Printf("Failed to handle post event: %v", err)
			// Continue processing other events even if one fails
		}
	}
}
