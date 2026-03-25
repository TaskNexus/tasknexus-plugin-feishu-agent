// Package main implements a Feishu long-connection agent that receives
// card interaction callbacks via WebSocket from Feishu and forwards them
// to specific WebSocket clients identified by their unique client_id.
//
// Callback flow:
//  1. Python client connects to the WS server, receives a unique client_id
//  2. Python client sends a Feishu card with client_id in action.value
//  3. Feishu sends card.action.trigger via long-connection WebSocket
//  4. This agent extracts client_id, forwards the callback to the WS client
//  5. This agent returns nil (SDK ACK is sent to Feishu)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/tasknexus/feishu-agent/wsserver"
)

// cardActionEvent mirrors the actual Feishu card.action.trigger event payload.
type cardActionEvent struct {
	Event cardEventBody `json:"event"`
}

type cardEventBody struct {
	Operator cardOperator `json:"operator"`
	Action   cardAction   `json:"action"`
	Context  cardContext  `json:"context"`
}

type cardOperator struct {
	OpenID string `json:"open_id"`
}

type cardAction struct {
	Value map[string]interface{} `json:"value"`
}

type cardContext struct {
	OpenMessageID string `json:"open_message_id"`
}

func main() {
	appID := mustEnv("APP_ID")
	appSecret := mustEnv("APP_SECRET")
	wsPort := getEnv("WS_PORT", "8765")

	// Initialize WebSocket hub for client connections
	hub := wsserver.NewHub()

	// Start HTTP server for WebSocket connections.
	// The public ingress exposes this service under /feishu/ws.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/feishu/ws", hub.HandleWS)

		addr := ":" + wsPort
		fmt.Printf("[feishu-agent] WebSocket server listening on %s\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[feishu-agent] WebSocket server error: %v\n", err)
			os.Exit(1)
		}
	}()

	// Register event dispatcher: subscribe to card.action.trigger
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnCustomizedEvent("card.action.trigger", func(ctx context.Context, event *larkevent.EventReq) error {
			return handleCardAction(ctx, event, hub)
		})

	// Create the Feishu long-connection WebSocket client
	cli := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	// Exponential backoff retry loop — keeps the container alive on transient errors
	backoff := 5 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		fmt.Println("[feishu-agent] connecting to Feishu via long-connection WebSocket...")
		if err := cli.Start(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "[feishu-agent] connection error: %v — retrying in %s\n", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = 5 * time.Second
		}
	}
}

// handleCardAction parses card.action.trigger and forwards it to the
// WebSocket client identified by client_id in action.value.
func handleCardAction(ctx context.Context, event *larkevent.EventReq, hub *wsserver.Hub) error {
	fmt.Printf("[feishu-agent] card.action.trigger received, body=%s\n", string(event.Body))

	var payload cardActionEvent
	if err := json.Unmarshal(event.Body, &payload); err != nil {
		fmt.Printf("[feishu-agent] failed to parse card action payload: %v\n", err)
		return nil
	}

	// Extract client_id from action.value
	clientID, _ := payload.Event.Action.Value["client_id"].(string)
	if clientID == "" {
		fmt.Printf("[feishu-agent] no client_id in action.value, skipping WS forward\n")
		return nil
	}

	// Build the message to forward to the WebSocket client
	forward := map[string]interface{}{
		"type":            "card_callback",
		"open_message_id": payload.Event.Context.OpenMessageID,
		"action": map[string]interface{}{
			"value":   payload.Event.Action.Value,
			"open_id": payload.Event.Operator.OpenID,
		},
	}

	fmt.Printf("[feishu-agent] forwarding to WS client %s: decision=%v open_id=%v msg_id=%v\n",
		clientID,
		payload.Event.Action.Value["decision"],
		payload.Event.Operator.OpenID,
		payload.Event.Context.OpenMessageID,
	)

	// Forward to WS client asynchronously — return nil first to ACK Feishu
	go func() {
		if err := hub.SendToClient(clientID, forward); err != nil {
			fmt.Printf("[feishu-agent] failed to forward to WS client: %v\n", err)
		}
	}()

	return nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "[feishu-agent] fatal: required env var %s is not set\n", key)
		os.Exit(1)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
