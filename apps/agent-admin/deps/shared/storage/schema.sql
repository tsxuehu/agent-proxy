CREATE TABLE IF NOT EXISTS endpoints (
    id VARCHAR(64) PRIMARY KEY,
    user_id VARCHAR(128) NOT NULL,
    protocol VARCHAR(16) NOT NULL,
    backend VARCHAR(32) NOT NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    models JSON NOT NULL,
    key_hash BINARY(32) NOT NULL,
    key_id VARCHAR(64) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    INDEX user_endpoints (user_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS codex_accounts (
    id VARCHAR(128) PRIMARY KEY,
    label VARCHAR(255) NOT NULL,
    credentials BLOB NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    refresh_state VARCHAR(16) NOT NULL DEFAULT 'ready',
    refresh_owner VARCHAR(64) NOT NULL DEFAULT '',
    refresh_started DATETIME(6),
    cooldown_until DATETIME(6),
    quota_score DOUBLE,
    quota_observed DATETIME(6)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS model_cooldowns (
    account_id VARCHAR(128) NOT NULL,
    model VARCHAR(128) NOT NULL,
    until_at DATETIME(6) NOT NULL,
    PRIMARY KEY (account_id, model),
    FOREIGN KEY (account_id) REFERENCES codex_accounts(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS session_bindings (
    scope BINARY(32) PRIMARY KEY,
    account_id VARCHAR(128) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    INDEX session_expiry (expires_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS response_bindings (
    scope BINARY(32) PRIMARY KEY,
    account_id VARCHAR(128) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    INDEX response_expiry (expires_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS portal_users (
    id VARCHAR(128) PRIMARY KEY,
    key_hash BINARY(32) UNIQUE NOT NULL
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS model_api_keys (
    id VARCHAR(64) PRIMARY KEY,
    provider VARCHAR(32) NOT NULL,
    label VARCHAR(255) NOT NULL,
    credentials BLOB NOT NULL
) ENGINE=InnoDB;
