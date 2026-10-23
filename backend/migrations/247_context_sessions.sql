-- Gateway-owned contexts. Large immutable content is kept outside CAS records.
CREATE TABLE IF NOT EXISTS context_records (
 scope VARCHAR(64) NOT NULL,
 kind VARCHAR(16) NOT NULL CHECK (kind IN ('session','operation','attempt','job')),
 id VARCHAR(128) NOT NULL,
 user_id BIGINT NOT NULL,
 api_key_id BIGINT NOT NULL,
 account_id BIGINT NOT NULL DEFAULT 0,
 revision BIGINT NOT NULL DEFAULT 1,
 status VARCHAR(32) NOT NULL,
 data JSONB NOT NULL,
 cancelled BOOLEAN NOT NULL DEFAULT FALSE,
 lease_owner VARCHAR(128) NOT NULL DEFAULT '',
 lease_token BIGINT NOT NULL DEFAULT 0,
 lease_until TIMESTAMPTZ,
 expires_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (scope,kind,id),
 CHECK (octet_length(data::text) <= 2097152)
);
CREATE INDEX IF NOT EXISTS context_records_owner_idx ON context_records(user_id,kind,expires_at);
CREATE INDEX IF NOT EXISTS context_records_pending_idx ON context_records(kind,status,updated_at);
CREATE INDEX IF NOT EXISTS context_records_expiry_idx ON context_records(expires_at);
CREATE INDEX IF NOT EXISTS context_records_attempt_session_idx ON context_records(scope,(data->>'session_id')) WHERE kind='attempt';
CREATE INDEX IF NOT EXISTS context_records_attempt_operation_idx ON context_records(scope,(data->>'operation_id')) WHERE kind='attempt';
CREATE TABLE IF NOT EXISTS context_history (
 seq BIGSERIAL PRIMARY KEY,
 scope VARCHAR(64) NOT NULL,
 id VARCHAR(128) NOT NULL,
 session_id VARCHAR(128) NOT NULL,
 user_id BIGINT NOT NULL,
 api_key_id BIGINT NOT NULL,
 visibility VARCHAR(16) NOT NULL CHECK (visibility IN ('public','private')),
 kind VARCHAR(24) NOT NULL,
 digest VARCHAR(64) NOT NULL,
 size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0 AND size_bytes <= 67108864),
 parts INTEGER NOT NULL CHECK (parts > 0 AND parts <= 256),
 expires_at TIMESTAMPTZ NOT NULL,
 UNIQUE(scope,id)
);
CREATE INDEX IF NOT EXISTS context_history_session_idx ON context_history(scope,session_id,visibility,seq);
CREATE INDEX IF NOT EXISTS context_history_expiry_idx ON context_history(expires_at);
CREATE INDEX IF NOT EXISTS context_history_owner_idx ON context_history(user_id,session_id);
CREATE TABLE IF NOT EXISTS context_blob_parts (
 scope VARCHAR(64) NOT NULL,
 id VARCHAR(128) NOT NULL,
 part INTEGER NOT NULL CHECK(part >= 0),
 data BYTEA NOT NULL CHECK (octet_length(data) <= 1048576),
 PRIMARY KEY(scope,id,part),
 FOREIGN KEY(scope,id) REFERENCES context_history(scope,id) ON DELETE CASCADE
);
