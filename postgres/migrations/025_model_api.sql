-- How to call a model: '' for Chat Completions, 'responses' for OpenAI's
-- Responses API (needed for tools with reasoning on newer OpenAI models).
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS api text NOT NULL DEFAULT '';
