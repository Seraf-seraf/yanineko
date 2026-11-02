CREATE TABLE IF NOT EXISTS telegram_users (
    user_id BIGINT PRIMARY KEY,
    username TEXT,
    name TEXT NOT NULL DEFAULT '',
    last_update_id BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS conversation_members (
    chat_id BIGINT NOT NULL,
    thread_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    member_order BIGINT NOT NULL,
    PRIMARY KEY (chat_id, thread_id, user_id),
    UNIQUE (chat_id, thread_id, member_order)
);
ALTER TABLE processed_updates ADD COLUMN author_id BIGINT;
ALTER TABLE processed_updates ADD COLUMN author_username TEXT;
ALTER TABLE processed_updates ADD COLUMN author_name TEXT;
ALTER TABLE processed_updates ADD COLUMN author_order BIGINT;
ALTER TABLE conversation_messages ADD COLUMN author_id BIGINT;
ALTER TABLE conversation_messages ADD COLUMN author_username TEXT;
ALTER TABLE conversation_messages ADD COLUMN author_name TEXT;
ALTER TABLE conversation_messages ADD COLUMN author_order BIGINT;
