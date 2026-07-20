package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
)

var DB *sql.DB

type Config struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultConfig returns sensible production defaults for the connection pool.
func DefaultConfig() Config {
	return Config{
		MaxOpenConns:    25,
		MaxIdleConns:    10,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
	}
}

// InitDB opens a PostgreSQL connection pool using the provided DSN and applies
// the given pool Config. It performs a connectivity check (Ping) before returning.
//
// DSN format: postgres://user:password@host:port/dbname?sslmode=disable
func InitDB(dsn string, cfg Config) error {
	if dsn == "" {
		return fmt.Errorf("database DSN must not be empty")
	}

	var err error
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Apply connection pool settings.
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	// Verify the connection is alive.
	if err = db.Ping(); err != nil {
		return fmt.Errorf("database ping failed: %w", err)
	}

	pragmas := []string{
		"SET synchronous_commit = 'local';",
		"SET lock_timeout = 5000;",
		"SET statement_timeout = 10000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return fmt.Errorf("failed to apply session setting '%s': %w", p, err)
		}
	}

	tables := []string{
		// Users
		`CREATE TABLE IF NOT EXISTS users (
        id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        name TEXT NOT NULL,
        email TEXT NOT NULL UNIQUE,
        password TEXT NOT NULL,
        refresh_token_web TEXT,
        refresh_token_web_updated_at TIMESTAMP,
        refresh_token_mobile TEXT,
        refresh_token_mobile_updated_at TIMESTAMP,
        avatar_url TEXT,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,

		// Privates
		`CREATE TABLE IF NOT EXISTS privates (
        id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        user1_id INTEGER NOT NULL,
        user2_id INTEGER NOT NULL,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        UNIQUE(user1_id, user2_id),
        CHECK(user1_id < user2_id),
        FOREIGN KEY(user1_id) REFERENCES users(id) ON DELETE CASCADE,
        FOREIGN KEY(user2_id) REFERENCES users(id) ON DELETE CASCADE
		);`,

		// Messages
		`CREATE TABLE IF NOT EXISTS messages (
        id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		from_id INTEGER NOT NULL,
        private_id INTEGER,
        message_type TEXT NOT NULL,
        content TEXT NOT NULL,
        delivered INTEGER NOT NULL DEFAULT 0, -- Keeps 0/1 integers or you could switch to BOOLEAN DEFAULT FALSE
        read INTEGER NOT NULL DEFAULT 0,      -- Keeps 0/1 integers or you could switch to BOOLEAN DEFAULT FALSE
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY(from_id) REFERENCES users(id) ON DELETE CASCADE,
        FOREIGN KEY(private_id) REFERENCES privates(id) ON DELETE CASCADE
		);`,

		// OAuth Connections
		`CREATE TABLE IF NOT EXISTS oauth_connections (
        user_id INTEGER PRIMARY KEY,
        provider TEXT NOT NULL,
        provider_user_id TEXT NOT NULL,
        encrypted_token bytea NOT NULL,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
        UNIQUE(provider, provider_user_id)
		);`,

		// Rooms
		`CREATE TABLE IF NOT EXISTS rooms (
        id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        livekit_room_name    TEXT        NOT NULL UNIQUE,
        host_guest_id        TEXT        NOT NULL,
        title                TEXT        NOT NULL,
        status               TEXT        NOT NULL DEFAULT 'draft'
                             CHECK (status IN ('draft', 'live', 'ended')),
        tier                 TEXT        NOT NULL DEFAULT 'guest'
                             CHECK (tier IN ('guest', 'pro')),
        session_duration_mins INTEGER    NOT NULL DEFAULT 30,
        extend_used          BOOLEAN     NOT NULL DEFAULT FALSE,
        started_at           TIMESTAMPTZ,
        ended_at             TIMESTAMPTZ,
        created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`,

		// Room Participants
		`CREATE TABLE IF NOT EXISTS room_participants (
        id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        room_livekit_name    TEXT        NOT NULL,
        identity             TEXT        NOT NULL,
        display_name         TEXT        NOT NULL DEFAULT '',
        joined_at            TIMESTAMPTZ,
        left_at              TIMESTAMPTZ,
        UNIQUE (room_livekit_name, identity)
		);`,

		// Webhook Events
		`CREATE TABLE IF NOT EXISTS webhook_events (
        event_id             TEXT        PRIMARY KEY,
        event_type           TEXT        NOT NULL,
        payload              JSONB       NOT NULL,
        received_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`,

		// Recordings — tracks egress lifecycle, updated via webhooks.
		// file_url is null until egress_ended confirms the upload to Spaces.
		`CREATE TABLE IF NOT EXISTS recordings (
        id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        room_livekit_name    TEXT        NOT NULL,
        egress_id            TEXT        NOT NULL UNIQUE,
        status               TEXT        NOT NULL DEFAULT 'recording'
                             CHECK (status IN ('recording', 'completed', 'failed')),
        file_url             TEXT,
        file_size_bytes      BIGINT,
        duration_sec         INTEGER,
        created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        completed_at         TIMESTAMPTZ
		);`,

		// Job Queue — Postgres durable mirror of the Redis ready queue.
		// Redis is the fast path; this table is the crash-recovery safety net.
		// On startup / reconciler tick, rows WHERE status='pending' AND
		// next_run_at <= now() that are absent from Redis get re-pushed.
		`CREATE TABLE IF NOT EXISTS job_queue (
        id                   TEXT        PRIMARY KEY,
        type                 TEXT        NOT NULL
                             CHECK (type IN ('transcribe', 'summarize')),
        payload              JSONB       NOT NULL,
        status               TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'running', 'completed', 'failed')),
        attempts             INTEGER     NOT NULL DEFAULT 0,
        max_retries          INTEGER     NOT NULL DEFAULT 5,
        created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        next_run_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`,

		// Transcripts — stores the final Deepgram word-level output per recording.
		// words_json holds the full []WordResult array for downstream summarisation.
		// confidence is the utterance-level mean from Deepgram's response.
		`CREATE TABLE IF NOT EXISTS transcripts (
        id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        room_livekit_name    TEXT        NOT NULL,
        recording_id         BIGINT      NOT NULL REFERENCES recordings(id),
        egress_id            TEXT        NOT NULL UNIQUE,
        text                 TEXT        NOT NULL,
        words_json           JSONB       NOT NULL DEFAULT '[]',
        confidence           NUMERIC(5,4),
        duration_sec         INTEGER,
        model                TEXT        NOT NULL DEFAULT 'nova-3',
        language             TEXT        NOT NULL DEFAULT 'en',
        created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`,

		`CREATE TABLE IF NOT EXISTS summaries (
        id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        transcript_id        BIGINT      NOT NULL UNIQUE REFERENCES transcripts(id),
        room_livekit_name    TEXT        NOT NULL,
        category             TEXT        NOT NULL DEFAULT 'business_sync',
        executive_summary    TEXT        NOT NULL,
        key_points           JSONB       NOT NULL DEFAULT '[]',
        action_items         JSONB       NOT NULL DEFAULT '[]',
        decisions_made       JSONB       NOT NULL DEFAULT '[]',
        discussion_tags      JSONB       NOT NULL DEFAULT '[]',
        model                TEXT        NOT NULL DEFAULT 'gpt-4o-mini',
        created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`,
	}
	for _, table := range tables {
		_, err := db.Exec(table)
		if err != nil {
			_ = db.Close()
			return fmt.Errorf("schema migration failed: %w", err)
		}
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_private_id ON messages(private_id);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_id ON messages(from_id);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_privates_user1_id ON privates(user1_id);`,
		`CREATE INDEX IF NOT EXISTS idx_privates_user2_id ON privates(user2_id);`,
		`CREATE INDEX IF NOT EXISTS idx_rooms_status ON rooms(status);`,
		`CREATE INDEX IF NOT EXISTS idx_rooms_host_guest_id ON rooms(host_guest_id);`,
		`CREATE INDEX IF NOT EXISTS idx_rooms_livekit_name ON rooms(livekit_room_name);`,
		`CREATE INDEX IF NOT EXISTS idx_rooms_live_guest_expiry ON rooms(started_at, session_duration_mins) WHERE tier = 'guest' AND status = 'live' AND started_at IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_room_participants_room ON room_participants(room_livekit_name);`,
		`CREATE INDEX IF NOT EXISTS idx_room_participants_ident ON room_participants(identity);`,
		`CREATE INDEX IF NOT EXISTS idx_webhook_events_received_at ON webhook_events(received_at);`,
		`CREATE INDEX IF NOT EXISTS idx_recordings_room ON recordings(room_livekit_name);`,
		`CREATE INDEX IF NOT EXISTS idx_recordings_egress_id ON recordings(egress_id);`,
		`CREATE INDEX IF NOT EXISTS idx_recordings_status ON recordings(status);`,
		`CREATE INDEX IF NOT EXISTS idx_job_queue_status_next ON job_queue(status, next_run_at) WHERE status = 'pending';`,
		`CREATE INDEX IF NOT EXISTS idx_transcripts_room ON transcripts(room_livekit_name);`,
		`CREATE INDEX IF NOT EXISTS idx_transcripts_recording ON transcripts(recording_id);`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_transcript ON summaries(transcript_id);`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_room ON summaries(room_livekit_name);`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_tags ON summaries USING GIN (discussion_tags jsonb_path_ops);`,
	}
	for _, index := range indexes {
		_, err := db.Exec(index)
		if err != nil {
			_ = db.Close()
			return fmt.Errorf("index migration failed: %w", err)
		}
	}

	DB = db
	log.Println("[db] connection pool established — Recallo schema ready")
	return nil
}

// GetDB returns the global *sql.DB instance.
func GetDB() (*sql.DB, error) {
	if DB == nil {
		return nil, fmt.Errorf("database not initialised — call InitDB first")
	}
	return DB, nil
}

// CloseDBConnection drains and closes the connection pool gracefully.
func CloseDBConnection() {
	if DB == nil {
		return
	}

	if err := DB.Close(); err != nil {
		log.Printf("[db] error closing pool: %v", err)
	} else {
		log.Println("[db] connection pool closed")
	}
}
