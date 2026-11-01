CREATE TABLE IF NOT EXISTS model_contexts (
    chat_id BIGINT NOT NULL,
    thread_id BIGINT NOT NULL,
    through_update_id BIGINT NOT NULL,
    items_json TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (chat_id, thread_id),
    FOREIGN KEY (chat_id, thread_id) REFERENCES conversations (chat_id, thread_id)
);
