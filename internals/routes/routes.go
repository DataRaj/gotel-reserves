package routes

import (
	"net/http"

	"recallo/internals/handlers"
	"recallo/internals/middleware"
	"recallo/internals/realtime"
	"recallo/internals/rooms"
	"recallo/internals/webhooks"
)

// RegisterRoutes builds and returns the root HTTP mux with all application
// routes registered. The mux is wrapped with CORS middleware so every route
// benefits from it automatically.
//
// Parameters are passed explicitly (no package-level state) so this function
// is fully testable: pass a nil hub and stub handlers for unit tests.
func RegisterRoutes(
	hub *realtime.Hub,
	roomsHandler *rooms.Handler,
	webhookHandler *webhooks.Handler,
) http.Handler {
	mux := http.NewServeMux()

	// ── Health ────────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/v1/healthcheck", Healthcheck)

	// ── Auth (public) ─────────────────────────────────────────────────────────
	mux.HandleFunc("POST /api/v1/auth/register", handleUserRegistration)
	mux.HandleFunc("POST /api/v1/auth/login", handlers.HandleEmailLogin)
	mux.HandleFunc("POST /api/v1/auth/refresh-session", handlers.HandleRefreshSession)
	mux.HandleFunc("POST /api/v1/auth/refresh", handlers.HandleRefreshSession) // BFF proxy alias

	// ── OAuth (public) ────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/v1/auth/github", handlers.HandleBeginGithubAuth)
	mux.HandleFunc("GET /api/v1/auth/github/callback", handlers.HandleGithubAuthCallback)

	// ── Auth (protected) ──────────────────────────────────────────────────────
	mux.Handle("POST /api/v1/auth/logout", middleware.Authenticate(http.HandlerFunc(handlers.HandleLogout)))
	mux.Handle("GET /api/v1/auth/current-user", middleware.Authenticate(http.HandlerFunc(handlers.HandleGetCurrentUser)))

	// ── Users (protected) ─────────────────────────────────────────────────────
	mux.Handle("GET /api/v1/users/{id}", middleware.Authenticate(http.HandlerFunc(handlers.GetUserByID)))

	// ── Conversations (protected) ─────────────────────────────────────────────
	mux.Handle("GET /api/v1/conversations", middleware.Authenticate(http.HandlerFunc(handlers.HandleGetConversations)))
	mux.Handle("POST /api/v1/conversation/private/create", middleware.Authenticate(http.HandlerFunc(handlers.HandleJoinPrivate)))
	mux.Handle("GET /api/v1/conversation/private/{private_id}", middleware.Authenticate(http.HandlerFunc(handlers.HandleGetPrivate)))
	mux.Handle("GET /api/v1/conversation/private/{private_id}/messages", middleware.Authenticate(http.HandlerFunc(handlers.HandleGetPrivateMessages)))

	// ── Files (protected) ─────────────────────────────────────────────────────
	mux.Handle("POST /api/v1/files/{private_id}", middleware.Authenticate(http.HandlerFunc(handlers.HandleFileUpload)))
	mux.Handle("GET /api/v1/files/", middleware.AuthenticateHandler(handlers.HandleGetFile()))

	// ── WebSocket ─────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		HandleWebSocketConnection(hub, w, r)
	})
	mux.HandleFunc("GET /api/v1/ws/healthcheck", HandleWebSocketHealthCheck)

	// ── Rooms (guest tier — public, no JWT auth middleware) ───────────────────
	// Token issuance enforces plan limits internally — no auth middleware needed here.
	// Guest identity is carried in the request body/query params, not in a session cookie.
	roomsHandler.RegisterRoutes(mux)

	// ── LiveKit Webhooks ──────────────────────────────────────────────────────
	// No rate limiting on this endpoint — LiveKit retry behaviour needs reliable delivery.
	// HMAC validation inside the handler authenticates the caller.
	webhookHandler.RegisterRoutes(mux)

	return middleware.CORSMiddleware(mux)
}
