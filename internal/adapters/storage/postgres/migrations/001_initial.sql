CREATE TABLE IF NOT EXISTS conversations (
    chat_id BIGINT NOT NULL,
    thread_id BIGINT NOT NULL DEFAULT 0 CHECK (thread_id >= 0),
    last_update_id BIGINT NOT NULL DEFAULT -1,
    version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (chat_id, thread_id)
);

CREATE TABLE IF NOT EXISTS processed_updates (
    update_id BIGINT PRIMARY KEY,
    chat_id BIGINT NOT NULL,
    thread_id BIGINT NOT NULL,
    source_message_id BIGINT NOT NULL,
    input_text TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('text', 'about')),
    state TEXT NOT NULL CHECK (state IN ('processing', 'ready', 'sending', 'sent', 'delivery_unknown')),
    processing_owner TEXT NOT NULL,
    processing_version BIGINT NOT NULL CHECK (processing_version >= 0),
    response_text TEXT,
    telegram_message_id BIGINT,
    delivery_owner TEXT,
    delivery_lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (chat_id, thread_id) REFERENCES conversations (chat_id, thread_id),
    UNIQUE (chat_id, thread_id, update_id),
    CHECK (state NOT IN ('ready', 'sending', 'sent', 'delivery_unknown') OR response_text IS NOT NULL),
    CHECK (state <> 'sending' OR (delivery_owner IS NOT NULL AND delivery_lease_until IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS processed_updates_pending_idx
    ON processed_updates (chat_id, thread_id, update_id)
    WHERE state IN ('processing', 'ready', 'sending');

CREATE INDEX IF NOT EXISTS processed_updates_cleanup_idx
    ON processed_updates (updated_at, update_id)
    WHERE state IN ('sent', 'delivery_unknown');

CREATE TABLE IF NOT EXISTS conversation_messages (
    chat_id BIGINT NOT NULL,
    thread_id BIGINT NOT NULL,
    update_id BIGINT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content TEXT NOT NULL CHECK (length(content) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (chat_id, thread_id, update_id, role),
    FOREIGN KEY (chat_id, thread_id) REFERENCES conversations (chat_id, thread_id)
);

CREATE INDEX IF NOT EXISTS conversation_messages_history_idx
    ON conversation_messages (chat_id, thread_id, update_id DESC, role);
