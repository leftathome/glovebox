-- 0001_init: the record journal's schema.
-- Spec: docs/superpowers/specs/2026-10-16-record-journal-design.md, 4.2.
-- Every column is described in docs/journal-schema.md.

CREATE TABLE record_type (
    name             text        NOT NULL,
    schema_version   integer     NOT NULL CHECK (schema_version >= 1),
    owner            text        NOT NULL,
    fields           jsonb       NOT NULL,
    renames          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    default_audience text[]      NOT NULL DEFAULT '{}',
    fixed_audience   text[],
    allow_no_subject boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (name, schema_version)
);

CREATE TABLE snapshot (
    id             uuid        PRIMARY KEY,
    record_type    text        NOT NULL,
    schema_version integer     NOT NULL,
    scope          text        NOT NULL,
    data_subject   text        NOT NULL,
    complete       boolean     NOT NULL,
    approximate    boolean     NOT NULL,
    observed_at    timestamptz NOT NULL,
    received_at    timestamptz NOT NULL,
    writer         text        NOT NULL,
    status         text        NOT NULL CHECK (status IN ('applied', 'late', 'held', 'discarded')),
    payload        jsonb       NOT NULL,
    stats          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    FOREIGN KEY (record_type, schema_version) REFERENCES record_type (name, schema_version)
);

CREATE INDEX snapshot_scope_observed ON snapshot (record_type, scope, observed_at);
CREATE INDEX snapshot_subject ON snapshot (data_subject);
CREATE INDEX snapshot_held ON snapshot (record_type, scope) WHERE status = 'held';

CREATE TABLE record_current (
    record_type             text        NOT NULL,
    data_subject            text        NOT NULL,
    key                     text        NOT NULL,
    -- event-derived: always equal to the fold of this record's events
    scope                   text        NOT NULL,
    audience                text[]      NOT NULL,
    fields                  jsonb       NOT NULL,
    removed                 boolean     NOT NULL DEFAULT false,
    -- observation bookkeeping: not events
    first_seen_at           timestamptz NOT NULL,
    last_seen_at            timestamptz NOT NULL,
    last_confirmed_snapshot uuid,
    last_confirmed_at       timestamptz NOT NULL,
    content_hash            bytea       NOT NULL,
    last_event_seq          bigint      NOT NULL,
    scan                    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (record_type, data_subject, key)
);

CREATE INDEX record_current_subject ON record_current (data_subject);
CREATE INDEX record_current_scope ON record_current (scope);
CREATE INDEX record_current_audience ON record_current USING gin (audience);

CREATE TABLE record_event (
    seq              bigserial   PRIMARY KEY,
    record_type      text        NOT NULL,
    data_subject     text        NOT NULL,
    key              text        NOT NULL,
    kind             text        NOT NULL CHECK (kind IN ('created', 'updated', 'removed', 'restored')),
    schema_version   integer     NOT NULL,
    scope            text        NOT NULL,
    prev_scope       text,
    audience         text[]      NOT NULL,
    prev_audience    text[],
    fields           jsonb,
    changed          jsonb,
    scan             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    observed_at      timestamptz NOT NULL,
    prev_observed_at timestamptz,
    approximate      boolean     NOT NULL,
    snapshot_id      uuid,
    actor            text        NOT NULL,
    importance       text        NOT NULL CHECK (importance IN ('high', 'normal', 'low')),
    rule             text,
    notify           jsonb,
    group_key        text,
    written_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX record_event_subject_seq ON record_event (data_subject, seq);
CREATE INDEX record_event_record_seq ON record_event (record_type, data_subject, key, seq);
CREATE INDEX record_event_audience ON record_event USING gin (audience);

-- Append-only: a rescan (spec 4.11) may rewrite verdicts, nothing else may
-- change; rows leave only through erase (4.13), which is a DELETE.
CREATE FUNCTION record_event_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'scan') IS DISTINCT FROM (to_jsonb(OLD) - 'scan') THEN
        RAISE EXCEPTION 'record_event is append-only; only scan may change (seq %)', OLD.seq;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER record_event_append_only
    BEFORE UPDATE ON record_event
    FOR EACH ROW EXECUTE FUNCTION record_event_append_only();

CREATE TABLE erasure (
    id           bigserial   PRIMARY KEY,
    data_subject text        NOT NULL,
    erased_at    timestamptz NOT NULL,
    actor        text        NOT NULL,
    rows         jsonb       NOT NULL
);

CREATE INDEX erasure_subject ON erasure (data_subject, erased_at);
