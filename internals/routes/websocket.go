package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"recallo/internals/logger"
	"recallo/internals/middleware"
	"recallo/internals/models"
	"recallo/internals/realtime"
	"recallo/internals/utils"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func HandleWebSocketHealthCheck(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		opts := websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		}
		conn, err := websocket.Accept(w, r, &opts)
		if err != nil {
			logger.App.Printf("[WEBSOCKET-HEALTH] error=accept_failed remote=%s err=%v", r.RemoteAddr, err)
			return
		}
		// Upgrade successful, we can immediately close it.
		conn.Close(websocket.StatusNormalClosure, "healthcheck complete")
		return
	}
	utils.JSON(w, http.StatusOK, true, "WebSocket healthcheck OK", map[string]string{"status": "ready"})
}

func HandleWebSocketConnection(hub *realtime.Hub, w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get(middleware.CtxAuthorization)
	accessToken := ""

	if authHeader != "" && strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		accessToken = strings.TrimSpace(authHeader[7:])
	} else {
		// Fallback for browser WebSocket clients which cannot send custom headers
		accessToken = r.URL.Query().Get("token")
	}

	if accessToken == "" {
		logger.App.Printf("[WEBSOCKET] error=missing_or_invalid_auth_header remote=%s", r.RemoteAddr)
		utils.JSON(w, http.StatusUnauthorized, false, "User is unauthorized", nil)
		return
	}

	userId, _, _, err := utils.VerifyJWT(accessToken)
	if err != nil {
		logger.App.Printf("[WEBSOCKET] error=invalid_jwt remote=%s err=%v", r.RemoteAddr, err)
		utils.JSON(w, http.StatusUnauthorized, false, "User is unauthorized", nil)
		return
	}

	user, err := models.GetUserByID(userId)
	if err != nil {
		logger.App.Printf("[WEBSOCKET] error=user_not_found user_id=%d err=%v", userId, err)
		utils.JSON(w, http.StatusUnauthorized, false, "User is unauthorized", nil)
		return
	}

	opts := websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	}

	conn, err := websocket.Accept(w, r, &opts)
	if err != nil {
		logger.App.Printf("[WEBSOCKET] error=accept_failed user_id=%d remote=%s err=%v", user.ID, r.RemoteAddr, err)
		// NOTE: websocket.Accept already wrote the HTTP error; do not call utils.JSON here.
		return
	}

	logger.App.Printf("[WEBSOCKET] connected user_id=%d remote=%s", user.ID, r.RemoteAddr)

	client := realtime.NewClient(user, conn)

	// Use the shared hub that was injected — NOT a fresh NewHub() each time.
	hub.RegisterClientConnection(client)
	hub.SentCurrentClients(client)

	defer func() {
		hub.UnRegisterClientConnection(client)
		client.Close()
		logger.App.Printf("[WEBSOCKET] disconnected user_id=%d remote=%s", user.ID, r.RemoteAddr)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go heartbeat(ctx, client)
	go writePump(ctx, client)

	readPump(ctx, cancel, hub, client)
}

func heartbeat(ctx context.Context, client *realtime.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := client.Conn.Ping(pingCtx)
			cancel()
			if err != nil {
				logger.App.Printf("[WEBSOCKET] error=ping_failed user_id=%d err=%v", client.User.ID, err)
				return
			}
			client.Send <- realtime.Event{
				EventType: realtime.EventHeartbeat,
				Payload:   nil,
			}
		}
	}
}

func writePump(ctx context.Context, client *realtime.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-client.Send:
			if !ok {
				return
			}

			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(writeCtx, client.Conn, event)
			cancel()
			if err != nil {
				logger.App.Printf("[WEBSOCKET] error=write_failed user_id=%d event=%s err=%v", client.User.ID, event.EventType, err)
				return
			}
		}
	}
}

// readPump reads incoming events from the WebSocket connection and dispatches
// them to handleIncomingEvents. Exits on read error or context cancellation.
func readPump(ctx context.Context, cancelCtx context.CancelFunc, hub *realtime.Hub, client *realtime.Client) {
	defer cancelCtx()
	defer func() {
		if r := recover(); r != nil {
			logger.App.Printf("[WEBSOCKET] error=readpump_panic user_id=%d panic=%v", client.User.ID, r)
		}
	}()

	for {
		var event realtime.Event
		err := wsjson.Read(ctx, client.Conn, &event)
		if err != nil {
			// Log only unexpected errors — normal closure / context cancel are expected.
			if ctx.Err() == nil {
				logger.App.Printf("[WEBSOCKET] error=read_failed user_id=%d err=%v", client.User.ID, err)
			}
			return
		}

		handleIncomingEvents(hub, client, event)
	}
}

// handleIncomingEvents dispatches a parsed client event to the correct handler.
func handleIncomingEvents(hub *realtime.Hub, client *realtime.Client, event realtime.Event) {
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		hub.SendError(client.User.ID, "invalid event payload format")
		return
	}

	switch event.EventType {

	// ── EventMessage ────────────────────────────────────────────────────────────
	case realtime.EventMessage:
		privateId, ok := extractInt64(payload, "private_id")
		if !ok {
			hub.SendError(client.User.ID, "private_id is missing or not a number")
			return
		}

		receiverId, ok := extractInt64(payload, "receiver_id")
		if !ok {
			hub.SendError(client.User.ID, "receiver_id is missing or not a number")
			return
		}

		messageTypeAny, ok := payload["message_type"]
		if !ok {
			hub.SendError(client.User.ID, "message_type is missing")
			return
		}
		messageType, ok := messageTypeAny.(string)
		if !ok {
			hub.SendError(client.User.ID, "message_type must be a string")
			return
		}

		msgBytes, _ := json.Marshal(payload)
		var msg models.Message
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			hub.SendError(client.User.ID, "invalid message format")
			return
		}

		msg.FromID = client.User.ID
		msg.PrivateID = privateId
		msg.MessageType = messageType
		msg.CreatedAt = time.Now()

		// For gif messages persist the image URL in content (no dedicated
		// column) so it survives REST history reads; keep gif_url populated
		// on the broadcast so clients render the image immediately.
		if messageType == "gif" && msg.GifURL != "" {
			msg.Content = msg.GifURL
		}

		if err := models.CreateMessage(&msg); err != nil {
			logger.App.Printf("[WEBSOCKET] error=create_message user_id=%d err=%v", client.User.ID, err)
			hub.SendError(client.User.ID, "failed to save message")
			return
		}

		if msg.ID == 0 {
			logger.App.Printf("[WEBSOCKET] error=message_id_zero user_id=%d from_id=%d private_id=%d", client.User.ID, msg.FromID, msg.PrivateID)
			hub.SendError(client.User.ID, "message ID was not populated from database")
			return
		}

		hub.SendEventToUserIds([]int64{msg.FromID, receiverId}, msg.FromID, realtime.EventMessage, map[string]any{
			"message": msg,
		})

	// ── EventDelivered ───────────────────────────────────────────────────────────
	case realtime.EventDelivered:
		msgId, ok := extractInt64(payload, "message_id")
		if !ok {
			hub.SendError(client.User.ID, "message_id is missing or not a number")
			return
		}

		msg, err := models.GetMessageByID(msgId)
		if err != nil {
			hub.SendError(client.User.ID, "message not found")
			return
		}

		if msg.FromID == client.User.ID {
			hub.SendError(client.User.ID, "cannot mark own message as delivered")
			return
		}

		if err := models.MarkMessageAsDelivered(msgId); err != nil {
			logger.App.Printf("[WEBSOCKET] error=mark_delivered user_id=%d msg_id=%d err=%v", client.User.ID, msgId, err)
			hub.SendError(client.User.ID, "failed to mark message as delivered")
			return
		}

		hub.SendEventToUserIds([]int64{msg.FromID}, client.User.ID, realtime.EventDelivered, map[string]any{
			"message_id": msgId,
			"to_id":      client.User.ID,
		})

	// ── EventRead ────────────────────────────────────────────────────────────────
	case realtime.EventRead:
		msgId, ok := extractInt64(payload, "message_id")
		if !ok {
			hub.SendError(client.User.ID, "message_id is missing or not a number")
			return
		}

		msg, err := models.GetMessageByID(msgId)
		if err != nil {
			hub.SendError(client.User.ID, "message not found")
			return
		}

		if msg.FromID == client.User.ID {
			hub.SendError(client.User.ID, "cannot mark own message as read")
			return
		}

		if err := models.MarkMessageAsRead(msgId); err != nil {
			logger.App.Printf("[WEBSOCKET] error=mark_read user_id=%d msg_id=%d err=%v", client.User.ID, msgId, err)
			hub.SendError(client.User.ID, "failed to mark message as read")
			return
		}

		hub.SendEventToUserIds([]int64{msg.FromID}, client.User.ID, realtime.EventRead, map[string]any{
			"message_id": msgId,
		})

	// ── EventTyping ──────────────────────────────────────────────────────────────
	case realtime.EventTyping:
		privateId, ok := extractInt64(payload, "private_id")
		if !ok {
			hub.SendError(client.User.ID, "private_id is missing or not a number")
			return
		}

		receiverId, ok := extractInt64(payload, "receiver_id")
		if !ok {
			hub.SendError(client.User.ID, "receiver_id is missing or not a number")
			return
		}

		isTypingAny, ok := payload["is_typing"]
		if !ok {
			hub.SendError(client.User.ID, "is_typing is missing")
			return
		}
		isTyping, ok := isTypingAny.(bool)
		if !ok {
			hub.SendError(client.User.ID, "is_typing must be a boolean")
			return
		}

		hub.SendEventToUserIds([]int64{receiverId}, client.User.ID, realtime.EventTyping, map[string]any{
			"private_id": privateId,
			"user_id":    client.User.ID,
			"is_typing":  isTyping,
		})

	default:
		hub.SendError(client.User.ID, "unknown event type: "+string(event.EventType))
	}
}

// extractInt64 safely pulls a numeric value from a JSON-decoded map and
// converts it to int64. JSON numbers decode as float64, so this handles that.
func extractInt64(payload map[string]any, key string) (int64, bool) {
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}
