-- +goose Up
CREATE TABLE fleets (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE
               CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    labels     jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    id                text PRIMARY KEY
                      CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    fleet_id          uuid NOT NULL REFERENCES fleets (id),
    platform          text NOT NULL CHECK (platform ~ '^[a-z0-9]+/[a-z0-9]+$'),
    labels            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
    status            text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'excluded', 'decommissioned')),
    -- SHA-256 of a high-entropy random token. The token itself is never stored.
    credential_hash   bytea NOT NULL CHECK (octet_length(credential_hash) = 32),
    installed_release bigint NOT NULL DEFAULT 0 CHECK (installed_release >= 0),
    last_seen_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX devices_fleet_id_id_idx ON devices (fleet_id, id);
