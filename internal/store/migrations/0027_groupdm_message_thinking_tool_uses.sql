-- 0027_groupdm_message_thinking_tool_uses.sql
--
-- Adds groupdm_messages.thinking and groupdm_messages.tool_uses — the
-- extended-thinking text and tool-call trace recorded for agent replies
-- produced by a thread-room one-shot turn (ChatOneShot's done event carries
-- both on its assembled Message). tool_uses is a JSON array mirroring
-- agent.ToolUse. NULL for user/system posts and for agent posts made
-- outside a thread turn.
ALTER TABLE groupdm_messages ADD COLUMN thinking TEXT;
ALTER TABLE groupdm_messages ADD COLUMN tool_uses TEXT;
