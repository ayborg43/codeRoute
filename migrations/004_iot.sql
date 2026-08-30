-- Devices that talk to the gateway over MQTT or the IoT HTTP endpoints.
CREATE TABLE devices (
    device_id VARCHAR(128) PRIMARY KEY,
    first_seen TIMESTAMPTZ DEFAULT NOW(),
    last_seen TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE telemetry (
    id BIGSERIAL PRIMARY KEY,
    device_id VARCHAR(128) NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
    type VARCHAR(64) NOT NULL,
    data JSONB NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_telemetry_device ON telemetry(device_id, recorded_at DESC);
CREATE INDEX idx_telemetry_created ON telemetry(created_at);
