-- Records the task smart routing detected for each request, so usage analytics
-- can show which kinds of work went to which model.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS task VARCHAR(32);
