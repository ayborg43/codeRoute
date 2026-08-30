-- Records why a request failed, not merely that it did.
--
-- Without this the dashboard could show a red "error" row and nothing else, so
-- diagnosing a failing provider meant reading container logs. The upstream's
-- own words are the useful part: "deposit required" and "invalid api key" call
-- for completely different responses from the operator.

ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS failure TEXT;
