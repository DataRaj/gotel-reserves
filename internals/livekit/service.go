package livekit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	lkproto "github.com/livekit/protocol/livekit"

	"recallo/internals/configs"
)

type VideoQuality string

const (
	VideoQualityLow    VideoQuality = "low"
	VideoQualityMedium VideoQuality = "medium"
	VideoQualityHigh   VideoQuality = "high"
)

type ParticipantRole string

const (
	// RoleHost gets can_publish + can_subscribe + room_admin.
	RoleHost ParticipantRole = "host"

	// RoleSpeaker gets can_publish + can_subscribe.
	RoleSpeaker ParticipantRole = "speaker"

	// RoleViewer gets can_subscribe only.
	RoleViewer ParticipantRole = "viewer"
)

// ParticipantMetadata is serialised to JSON and embedded in the LiveKit token.
// All room participants can read this via the LiveKit SDK without an extra API call.
type ParticipantMetadata struct {
	DisplayName string `json:"display_name"`
	Plan        string `json:"plan"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// CreateRoomParams carries everything needed to pre-create a LiveKit room.
type CreateRoomParams struct {
	RoomName         string
	MaxParticipants  uint32
	EmptyTimeoutSecs uint32
	Metadata         string
}

// GenerateTokenParams carries everything needed to issue a capability token.
type GenerateTokenParams struct {
	RoomName        string
	Identity        string
	Role            ParticipantRole
	Metadata        ParticipantMetadata
	TTL             time.Duration
	MaxVideoQuality VideoQuality
}

// Service is the concrete LiveKit client. Uses the protocol Twirp client for
// room management and lkauth.AccessToken for JWT minting.
type Service struct {
	roomSvc   lkproto.RoomService // Twirp-generated client interface
	apiKey    string
	apiSecret string
	host      string
}

// NewService constructs a Service from the application's LiveKitConfig.
// Uses the protobuf Twirp client directly — no server-sdk-go dependency.
// The authMiddleware injects a signed JWT into every outbound Room Service request.
func NewService(cfg configs.LiveKitConfig) (*Service, error) {
	if cfg.Host == "" || cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, fmt.Errorf("livekit.NewService: host, api_key, and api_secret are all required")
	}

	// Normalise the host: the Twirp client needs https://, not wss://.
	// LiveKit Cloud WSS URLs have the same hostname as the REST API.
	httpHost := strings.Replace(cfg.Host, "wss://", "https://", 1)
	httpHost = strings.Replace(httpHost, "ws://", "http://", 1)

	// authMiddleware injects a short-lived API token into every Room Service request.
	// Room Service calls require a token with the room_admin claim or API credentials.
	apiKey := cfg.APIKey
	apiSecret := cfg.APISecret
	authMiddleware := func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			token, err := newAPIToken(apiKey, apiSecret)
			if err != nil {
				return nil, fmt.Errorf("livekit auth middleware: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			return next.RoundTrip(req)
		})
	}

	httpClient := &http.Client{
		Transport: authMiddleware(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}

	roomSvc := lkproto.NewRoomServiceProtobufClient(httpHost, httpClient)

	return &Service{
		roomSvc:   roomSvc,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		host:      cfg.Host,
	}, nil
}

// CreateRoom pre-creates a LiveKit room with plan constraints baked in.
// max_participants enforcement at LiveKit infra level = defence in depth layer 1.
func (s *Service) CreateRoom(ctx context.Context, p CreateRoomParams) error {
	req := &lkproto.CreateRoomRequest{
		Name:            p.RoomName,
		MaxParticipants: p.MaxParticipants,
		EmptyTimeout:    p.EmptyTimeoutSecs,
		Metadata:        p.Metadata,
	}
	if _, err := s.roomSvc.CreateRoom(ctx, req); err != nil {
		return fmt.Errorf("livekit.Service.CreateRoom: %w", err)
	}
	return nil
}

// DeleteRoom tears down the room. LiveKit fires room_finished webhook.
// Never mark DB room as ended on this call alone — wait for the webhook.
func (s *Service) DeleteRoom(ctx context.Context, roomName string) error {
	req := &lkproto.DeleteRoomRequest{Room: roomName}
	if _, err := s.roomSvc.DeleteRoom(ctx, req); err != nil {
		return fmt.Errorf("livekit.Service.DeleteRoom: %w", err)
	}
	return nil
}

// GenerateToken mints a signed LiveKit JWT for one participant.
// Role → VideoGrant flag mapping:
//   - RoleHost:    can_publish + can_subscribe + room_admin
//   - RoleSpeaker: can_publish + can_subscribe
//   - RoleViewer:  can_subscribe only
func (s *Service) GenerateToken(p GenerateTokenParams) (string, error) {
	metaBytes, err := json.Marshal(p.Metadata)
	if err != nil {
		return "", fmt.Errorf("livekit.Service.GenerateToken: marshal metadata: %w", err)
	}

	at := lkauth.NewAccessToken(s.apiKey, s.apiSecret)

	grant := &lkauth.VideoGrant{
		RoomJoin: true,
		Room:     p.RoomName,
	}

	switch p.Role {
	case RoleHost:
		grant.CanPublish = boolPtr(true)
		grant.CanSubscribe = boolPtr(true)
		grant.RoomAdmin = true
	case RoleSpeaker:
		grant.CanPublish = boolPtr(true)
		grant.CanSubscribe = boolPtr(true)
	case RoleViewer:
		grant.CanPublish = boolPtr(false)
		grant.CanSubscribe = boolPtr(true)
	default:
		return "", fmt.Errorf("livekit.Service.GenerateToken: unknown role %q", p.Role)
	}

	at.AddGrant(grant).
		SetIdentity(p.Identity).
		SetMetadata(string(metaBytes)).
		SetValidFor(p.TTL)

	token, err := at.ToJWT()
	if err != nil {
		return "", fmt.Errorf("livekit.Service.GenerateToken: sign jwt: %w", err)
	}
	return token, nil
}

// RemoveParticipant evicts a participant from a room. Caller handles DB-side ban logic.
func (s *Service) RemoveParticipant(ctx context.Context, roomName, identity string) error {
	req := &lkproto.RoomParticipantIdentity{Room: roomName, Identity: identity}
	if _, err := s.roomSvc.RemoveParticipant(ctx, req); err != nil {
		return fmt.Errorf("livekit.Service.RemoveParticipant: %w", err)
	}
	return nil
}

// RoomMetadata is the JSON blob injected into the active WebRTC signaling channel
// via UpdateRoomMetadata. All connected participants receive a RoomMetadataChanged
// event automatically — no polling required on the frontend.
type RoomMetadata struct {
	Tier         string            `json:"tier"`
	ActivePlugin string            `json:"active_plugin,omitempty"`
	CustomState  map[string]string `json:"custom_state,omitempty"`
}

// UpdateRoomMetadata patches the live metadata on an active LiveKit room.
// LiveKit broadcasts a RoomMetadataChanged event to every connected participant.
// Use this to propagate plugin state, tier changes, or custom signaling data
// without requiring a separate WebSocket message.
func (s *Service) UpdateRoomMetadata(ctx context.Context, roomName string, meta RoomMetadata) error {
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("livekit.Service.UpdateRoomMetadata: marshal: %w", err)
	}
	req := &lkproto.UpdateRoomMetadataRequest{
		Room:     roomName,
		Metadata: string(payload),
	}
	if _, err := s.roomSvc.UpdateRoomMetadata(ctx, req); err != nil {
		return fmt.Errorf("livekit.Service.UpdateRoomMetadata: %w", err)
	}
	return nil
}

// ListParticipantCount returns the number of live participants.
// Satisfies the rooms.LiveKitService interface — rooms domain only needs the count.
// Returns 0 (no error) when the room doesn't exist in LiveKit yet (draft state).
func (s *Service) ListParticipantCount(ctx context.Context, roomName string) (int, error) {
	req := &lkproto.ListParticipantsRequest{Room: roomName}
	resp, err := s.roomSvc.ListParticipants(ctx, req)
	if err != nil {
		// Room doesn't exist in LiveKit yet (draft state) — treat as 0 participants.
		return 0, nil
	}
	return len(resp.Participants), nil
}

// Host returns the LiveKit Cloud WSS host URL (wss://...).
func (s *Service) Host() string {
	return s.host
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// newAPIToken generates a short-lived administrative JWT for Room Service API calls.
// The token has room_create and room_admin claims so it can manage any room.
func newAPIToken(apiKey, apiSecret string) (string, error) {
	at := lkauth.NewAccessToken(apiKey, apiSecret)
	grant := &lkauth.VideoGrant{
		RoomCreate: true,
		RoomList:   true,
		RoomAdmin:  true,
	}
	at.AddGrant(grant).SetValidFor(30 * time.Second)
	return at.ToJWT()
}

// boolPtr returns a pointer to b. VideoGrant uses *bool fields.
func boolPtr(b bool) *bool { return &b }

// roundTripFunc adapts a function to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
