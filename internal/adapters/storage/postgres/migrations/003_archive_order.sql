ALTER TABLE conversation_messages ADD COLUMN archive_order BIGINT;

WITH ordered_messages AS (
    SELECT chat_id, thread_id, update_id, role,
           ROW_NUMBER() OVER (
               PARTITION BY chat_id, thread_id
               ORDER BY update_id, CASE role WHEN 'user' THEN 0 ELSE 1 END
           ) AS sequence_number
    FROM conversation_messages
)
UPDATE conversation_messages
SET archive_order = ordered_messages.sequence_number
FROM ordered_messages
WHERE ordered_messages.chat_id = conversation_messages.chat_id
  AND ordered_messages.thread_id = conversation_messages.thread_id
  AND ordered_messages.update_id = conversation_messages.update_id
  AND ordered_messages.role = conversation_messages.role;

ALTER TABLE conversation_messages ALTER COLUMN archive_order SET NOT NULL;
ALTER TABLE model_contexts RENAME COLUMN through_update_id TO through_message_order;

CREATE UNIQUE INDEX conversation_messages_archive_order_idx
    ON conversation_messages (chat_id, thread_id, archive_order);
