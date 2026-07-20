package configs

import (
	"flag"
	"log"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type HTTPServer struct {
	Address string `env:"HTTP_ADDRESS" env-default:"0.0.0.0:8080"`
}

type LiveKitConfig struct {
	// Host is the wss:// WebSocket URL of the LiveKit Cloud project.
	// Used by frontend SDKs to connect; stored here for token metadata.
	Host string `env:"LIVEKIT_HOST" env-default:""`

	// APIKey is the public identifier for signing LiveKit JWTs and calling Room Service.
	APIKey string `env:"LIVEKIT_API_KEY" env-default:""`

	// APISecret is the paired private key for APIKey.
	// It signs outbound tokens AND is used by the livekit SDK for server-side API calls.
	APISecret string `env:"LIVEKIT_API_SECRET" env-default:""`

	// WebhookSecret is the separate HMAC secret configured in LiveKit Cloud console
	// under Settings → Webhooks. Used exclusively to validate inbound webhook signatures.
	// Kept separate from APISecret so rotating one doesn't force rotating the other.
	WebhookSecret string `env:"LIVEKIT_WEBHOOK_SECRET" env-default:""`
}

// GuestTierConfig defines the hard limits applied to non-login (guest) users.
// These values gate what gets written into the LiveKit VideoGrant JWT and what
// max_participants is set on the LiveKit room at creation time.
//
// Keeping limits in config (not hardcoded) means a product decision to change
// the guest cap from 4→6 is an env-var change, not a code deploy.
type GuestTierConfig struct {
	// MaxParticipants is the hard cap on participants per guest room.
	// Enforced at two layers: LiveKit room creation (max_participants field)
	// AND at token-generation time before issuing any JWT.
	MaxParticipants int `env:"GUEST_MAX_PARTICIPANTS" env-default:"4"`

	// SessionDurationMins is the maximum call/stream length for guest rooms (minutes).
	// A background ticker ends the room when this window elapses.
	// Guests may POST /rooms/:id/extend once to add time (server validates against
	// an additional allowance, not an infinite extension).
	SessionDurationMins int `env:"GUEST_SESSION_DURATION_MINS" env-default:"30"`

	// MaxVideoQuality controls the simulcast layer granted to guest publishers.
	// Accepted values: "low" (≈360p), "medium" (≈480p), "high" (≈720p).
	// This maps to livekit.VideoQuality at token-generation time.
	MaxVideoQuality string `env:"GUEST_MAX_VIDEO_QUALITY" env-default:"medium"`
}

// DeepgramConfig groups all Deepgram API credentials and tuning parameters.
type DeepgramConfig struct {
	APIKey   string `env:"DEEPGRAM_API_KEY"    env-default:""`
	Model    string `env:"DEEPGRAM_MODEL"      env-default:"nova-3"`
	Language string `env:"DEEPGRAM_LANGUAGE"   env-default:"en"`
	// TimeoutSec is the HTTP client timeout for batch transcription requests.
	// Large files (1hr+) can take 30-60s to return from Deepgram.
	TimeoutSec int `env:"DEEPGRAM_TIMEOUT_SEC" env-default:"120"`
}

// SpacesConfig groups DigitalOcean Spaces (S3-compatible) credentials.
// Used by the transcripts package to presign GET URLs for Deepgram fetch.
type SpacesConfig struct {
	Endpoint  string `env:"SPACES_ENDPOINT"   env-default:""` // e.g. https://nyc3.digitaloceanspaces.com
	Bucket    string `env:"SPACES_BUCKET"     env-default:""`
	AccessKey string `env:"SPACES_ACCESS_KEY" env-default:""`
	SecretKey string `env:"SPACES_SECRET_KEY" env-default:""`
}

type GrokConfig struct {
	APIKey     string `env:"GROK_API_KEY"      env-default:""`
	Model      string `env:"GROK_MODEL"        env-default:"grok-3-mini"`
	TimeoutSec int    `env:"GROK_TIMEOUT_SEC"  env-default:"120"`
}

// Config is the full application configuration loaded from an .env file.
// All sub-configs are embedded as named fields, not anonymous embeds, so callers
// access them as cfg.LiveKit.APIKey — unambiguous and greppable.
type Config struct {
	Env         string `env:"ENV"          env-default:"dev"`
	DatabaseURL string `env:"DATABASE_URL" env-default:""`
	RedisURL    string `env:"REDIS_URL"    env-default:"redis://127.0.0.1:6379/0"`

	JWTSecretKey string `env:"JWT_SECRET_KEY" env-default:""`

	GithubClientID         string `env:"GITHUB_CLIENT_ID"          env-default:""`
	GithubClientSecret     string `env:"GITHUB_CLIENT_SECRET"      env-default:""`
	GithubOAuthRedirectURL string `env:"GITHUB_OAUTH_REDIRECT_URL" env-default:""`
	FrontendURL            string `env:"FRONTEND_URL"              env-default:"https://recallo.tech"`

	// LiveKit groups all credentials and endpoint config for the LiveKit SFU.
	LiveKit LiveKitConfig

	// GuestTier groups all rate/quality limits for the non-login (guest) user tier.
	GuestTier GuestTierConfig

	// Deepgram groups STT API config.
	Deepgram DeepgramConfig

	// Spaces groups DigitalOcean Spaces credentials for recording storage.
	Spaces SpacesConfig

	Grok GrokConfig

	HTTPServer
}

// LoadConfig resolves the config file path in this order:
//  1. -config <path> CLI flag
//  2. CONFIG_PATH environment variable
//  3. Default: config/dev.env
//
// Validation: fatal on any required field being empty so the server never starts
// in a misconfigured state. Fail fast, fail loudly.
func LoadConfig() *Config {
	var cfg Config
	var envPath string

	flag.StringVar(&envPath, "config", "", "path to .env config file")
	flag.Parse()

	if envPath == "" {
		envPath = os.Getenv("CONFIG_PATH")
	}
	if envPath == "" {
		envPath = "config/dev.env"
	}

	if err := cleanenv.ReadConfig(envPath, &cfg); err != nil {
		log.Fatalf("[config] failed to read config from %s: %v", envPath, err)
	}

	// ── Required: core infrastructure ─────────────────────────────────────────
	if cfg.DatabaseURL == "" {
		log.Fatal("[config] DATABASE_URL must not be empty")
	}
	if cfg.JWTSecretKey == "" {
		log.Fatal("[config] JWT_SECRET_KEY must not be empty")
	}

	// ── Required: OAuth ────────────────────────────────────────────────────────
	if cfg.GithubClientID == "" {
		log.Fatal("[config] GITHUB_CLIENT_ID must not be empty")
	}
	if cfg.GithubClientSecret == "" {
		log.Fatal("[config] GITHUB_CLIENT_SECRET must not be empty")
	}
	if cfg.GithubOAuthRedirectURL == "" {
		log.Fatal("[config] GITHUB_OAUTH_REDIRECT_URL must not be empty")
	}
	if cfg.FrontendURL == "" {
		log.Fatal("[config] FRONTEND_URL must not be empty")
	}

	// ── Required: LiveKit ─────────────────────────────────────────────────────
	// All four LiveKit values are required: missing any one causes broken token
	// generation or unvalidated webhooks — both are silent security holes.
	if cfg.LiveKit.Host == "" {
		log.Fatal("[config] LIVEKIT_HOST must not be empty")
	}
	if cfg.LiveKit.APIKey == "" {
		log.Fatal("[config] LIVEKIT_API_KEY must not be empty")
	}
	if cfg.LiveKit.APISecret == "" {
		log.Fatal("[config] LIVEKIT_API_SECRET must not be empty")
	}
	if cfg.LiveKit.WebhookSecret == "" {
		log.Fatal("[config] LIVEKIT_WEBHOOK_SECRET must not be empty")
	}

	// ── Defaults: guest tier ───────────────────────────────────────────────────
	// These have safe env-default values, but we validate the video quality string
	// to catch typos early rather than silently falling back to an unexpected tier.
	switch cfg.GuestTier.MaxVideoQuality {
	case "low", "medium", "high":
		// valid
	default:
		log.Fatalf(
			"[config] GUEST_MAX_VIDEO_QUALITY must be one of: low, medium, high — got %q",
			cfg.GuestTier.MaxVideoQuality,
		)
	}

	if cfg.Grok.APIKey == "" {
		log.Fatal("[config] GROK_API_KEY must not be empty")
	}

	log.Printf(
		"[config] loaded env=%s addr=%s livekit_host=%s frontend_url=%s guest_max_participants=%d guest_session_mins=%d guest_video=%s",
		cfg.Env,
		cfg.Address,
		cfg.LiveKit.Host,
		cfg.FrontendURL,
		cfg.GuestTier.MaxParticipants,
		cfg.GuestTier.SessionDurationMins,
		cfg.GuestTier.MaxVideoQuality,
	)

	return &cfg
}
