// Package main implements a Feishu long-connection agent that receives
// card interaction callbacks via WebSocket and forwards them to the Django backend.
//
// Card update flow (proper sequencing):
//   1. Feishu sends card.action.trigger via WebSocket
//   2. This agent forwards the event to Django (HTTP POST)
//   3. Django records the decision and returns the updated card JSON in the response body
//   4. This agent's event handler returns (WS ACK is sent to Feishu — no card data)
//   5. A goroutine then PATCHes the Feishu message with the updated card JSON
//
// Why goroutine for PATCH (not inline):
//   If PATCH runs before the WS ACK is sent, Feishu's ACK processing resets the card
//   to the original state (overriding our PATCH). Running PATCH in a goroutine that
//   starts after the handler returns ensures PATCH arrives AFTER the WS ACK.
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

// djangoResponse is the subset of Django's HTTP response we care about.
// Django returns card_update when the handler wants the agent to PATCH the card.
type djangoResponse struct {
	CardUpdate *cardUpdatePayload `json:"card_update"`
}

type cardUpdatePayload struct {
	MessageID string                 `json:"message_id"`
	Card      map[string]interface{} `json:"card"`
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
			return handleCardAction(ctx, event, backendURL, appID, appSecret, httpClient)
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

// handleCardAction parses the card.action.trigger event, forwards it to Django,
// and then PATCHes the Feishu card in a goroutine AFTER the WS ACK is sent.
func handleCardAction(ctx context.Context, event *larkevent.EventReq, backendURL, appID, appSecret string, client *http.Client) error {
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
	fmt.Printf("[feishu-agent] backend responded with status %d\n", resp.StatusCode)

	// Parse Django's response to check if there's a card update to apply
	var djangoResp djangoResponse
	if err := json.Unmarshal(respBody, &djangoResp); err == nil && djangoResp.CardUpdate != nil {
		cu := djangoResp.CardUpdate
		// PATCH the card in a goroutine — MUST happen AFTER this function returns
		// (i.e., after the SDK sends the WS ACK to Feishu). If PATCH ran inline
		// before the WS ACK, Feishu's ACK processing would reset the card.
		go func(msgID string, card map[string]interface{}) {
			// Brief pause to ensure WS ACK is delivered before our PATCH reaches Feishu
			time.Sleep(200 * time.Millisecond)
			if err := patchFeishuCard(client, appID, appSecret, msgID, card); err != nil {
				fmt.Printf("[feishu-agent] failed to update card %s: %v\n", msgID, err)
			} else {
				fmt.Printf("[feishu-agent] card %s updated successfully\n", msgID)
			}
		}(cu.MessageID, cu.Card)
	}

	return nil
}

// patchFeishuCard updates a Feishu message card via the REST API.
func patchFeishuCard(client *http.Client, appID, appSecret, messageID string, card map[string]interface{}) error {
	// Obtain a fresh tenant access token
	tokenResp, err := client.Post(
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		"application/json",
		bytes.NewBufferString(fmt.Sprintf(`{"app_id":%q,"app_secret":%q}`, appID, appSecret)),
	)
	if err != nil {
		return fmt.Errorf("get token request failed: %w", err)
	}
	defer tokenResp.Body.Close()

	var tokenData struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	if tokenData.Code != 0 {
		return fmt.Errorf("get token error: %s", tokenData.Msg)
	}

	// Build the PATCH request
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}

	patchBody, _ := json.Marshal(map[string]string{
		"msg_type": "interactive",
		"content":  string(cardJSON),
	})

	req, err := http.NewRequest(http.MethodPatch,
		"https://open.feishu.cn/open-apis/im/v1/messages/"+messageID,
		bytes.NewReader(patchBody))
	if err != nil {
		return fmt.Errorf("build patch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenData.TenantAccessToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	patchResp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("patch request failed: %w", err)
	}
	defer patchResp.Body.Close()

	var patchData struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	json.NewDecoder(patchResp.Body).Decode(&patchData)
	if patchData.Code != 0 {
		return fmt.Errorf("patch error code=%d msg=%s", patchData.Code, patchData.Msg)
	}
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
