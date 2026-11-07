-- Panel-wide settings (for example the SMTP server and the panel's address for links).
-- Secret values are encrypted with the panel's key before they are stored.
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Where a user wants to be told about events. Secret parts (bot tokens, webhook URLs,
-- signing secrets) are encrypted in `secret`.
CREATE TABLE notification_channels (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         text        NOT NULL CHECK (kind IN ('email', 'telegram', 'discord', 'slack', 'webhook')),
    name         text        NOT NULL,
    config       jsonb       NOT NULL DEFAULT '{}',
    secret       bytea,
    events       text[]      NOT NULL DEFAULT '{}', -- empty: every event the user receives
    enabled      boolean     NOT NULL DEFAULT true,
    last_sent_at timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_channels_user_idx ON notification_channels (user_id);

-- Each user's notifications: the panel's inbox, and what goes out to their channels.
CREATE TABLE notifications (
    id         bigserial PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event      text        NOT NULL,
    severity   text        NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    params     jsonb       NOT NULL DEFAULT '{}',
    link       text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at    timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);

CREATE TABLE notification_deliveries (
    notification_id bigint      NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    channel_id      uuid        NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    attempts        integer     NOT NULL DEFAULT 0,
    next_at         timestamptz NOT NULL DEFAULT now(),
    done            boolean     NOT NULL DEFAULT false,
    error           text,
    PRIMARY KEY (notification_id, channel_id)
);
CREATE INDEX notification_deliveries_due_idx ON notification_deliveries (next_at) WHERE NOT done;

-- Remembered conditions, so an alert is sent when something changes, not every minute.
CREATE TABLE alert_state (
    key        text PRIMARY KEY,
    value      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
