CREATE TABLE IF NOT EXISTS model_contexts (
    chat_id INTEGER NOT NULL,
    thread_id INTEGER NOT NULL,
    through_update_id INTEGER NOT NULL,
    items_json TEXT NOT NULL,
    updated_at REAL NOT NULL DEFAULT (julianday('now')),
    PRIMARY KEY (chat_id, thread_id),
    FOREIGN KEY (chat_id, thread_id) REFERENCES conversations (chat_id, thread_id)
);
