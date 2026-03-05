// Package main implements a Feishu long-connection agent that receives
// card interaction callbacks via WebSocket and forwards them to the Django backend.
//
// Callback flow:
//   1. Feishu sends card.action.trigger via WebSocket
//   2. This agent forwards the event to Django (HTTP POST)
//   3. Django forwards callback data to bamboo node
//   4. This agent returns from handler (SDK ACK is sent to Feishu)
// Card update is handled by dedicated pipeline nodes, not by this agent.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// cardActionEvent mirrors the actual Feishu card.action.trigger event payload.
// Actual structure (from wire):
//
//	{
//	  "schema": "2.0",
//	  "header": { "event_type": "card.action.trigger", ... },
//	  "event": {
//	    "operator": { "open_id": "ou_xxx" },
//	    "action":   { "value": { "token": "...", "decision": "...", "open_id": "..." } },
//	    "context":  { "open_message_id": "om_xxx" }
//	  }
//	}
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
	backendURL := getEnv("BACKEND_URL", "http://backend:8000/api/feishu/card-callback/")

	fmt.Printf("[feishu-agent] starting, backend=%s\n", backendURL)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Register event dispatcher: subscribe to card.action.trigger
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnCustomizedEvent("card.action.trigger", func(ctx context.Context, event *larkevent.EventReq) error {
			return handleCardAction(ctx, event, backendURL, httpClient)
		})

	// Create the WebSocket long-connection client
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

// handleCardAction parses card.action.trigger and forwards it to Django.
func handleCardAction(ctx context.Context, event *larkevent.EventReq, backendURL string, client *http.Client) error {
	fmt.Printf("[feishu-agent] card.action.trigger received, body=%s\n", string(event.Body))

	var payload cardActionEvent
	if err := json.Unmarshal(event.Body, &payload); err != nil {
		fmt.Printf("[feishu-agent] failed to parse card action payload: %v\n", err)
		return nil
	}

	// Build the forwarded request body for Django
	forward := map[string]interface{}{
		"type":            "card",
		"open_message_id": payload.Event.Context.OpenMessageID,
		"action": map[string]interface{}{
			"value":   payload.Event.Action.Value,
			"open_id": payload.Event.Operator.OpenID,
		},
	}

	fmt.Printf("[feishu-agent] forwarding: token=%v decision=%v node_id=%v node_version=%v open_id=%v msg_id=%v\n",
		payload.Event.Action.Value["token"],
		payload.Event.Action.Value["decision"],
		payload.Event.Action.Value["node_id"],
		payload.Event.Action.Value["node_version"],
		payload.Event.Operator.OpenID,
		payload.Event.Context.OpenMessageID,
	)

	body, err := json.Marshal(forward)
	if err != nil {
		fmt.Printf("[feishu-agent] failed to marshal forward payload: %v\n", err)
		return nil
	}

	resp, err := client.Post(backendURL, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Printf("[feishu-agent] failed to forward to backend: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("[feishu-agent] backend responded with status %d body=%s\n", resp.StatusCode, string(respBody))

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
