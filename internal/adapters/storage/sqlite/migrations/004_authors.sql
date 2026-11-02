CREATE TABLE IF NOT EXISTS telegram_users (
    user_id INTEGER PRIMARY KEY,
    username TEXT,
    name TEXT NOT NULL DEFAULT '',
    last_update_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS conversation_members (
    chat_id INTEGER NOT NULL,
    thread_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    member_order INTEGER NOT NULL,
    PRIMARY KEY (chat_id, thread_id, user_id),
    UNIQUE (chat_id, thread_id, member_order)
);
ALTER TABLE processed_updates ADD COLUMN author_id INTEGER;
ALTER TABLE processed_updates ADD COLUMN author_username TEXT;
ALTER TABLE processed_updates ADD COLUMN author_name TEXT;
ALTER TABLE processed_updates ADD COLUMN author_order INTEGER;
ALTER TABLE conversation_messages ADD COLUMN author_id INTEGER;
ALTER TABLE conversation_messages ADD COLUMN author_username TEXT;
ALTER TABLE conversation_messages ADD COLUMN author_name TEXT;
ALTER TABLE conversation_messages ADD COLUMN author_order INTEGER;
