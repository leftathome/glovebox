-- 0003_erasure_reader: notes kept when their subject is erased are read by
-- the readers frozen at erase time, and only by them (spec 4.13, decisions
-- 2026-10-23). Described in docs/journal-schema.md.

-- One row per reader of a kept note. pseudonym is the kept note's
-- data_subject ("erased:<32 hex>:<n>"); member is a roster member id that
-- could read the note when it was kept, or its author. Never the erased id.
CREATE TABLE erasure_reader (
    pseudonym text NOT NULL CHECK (pseudonym ~ '^erased:[0-9a-f]{32}:[1-9][0-9]*$'),
    member    text NOT NULL CHECK (member <> '' AND position(':' IN member) = 0),
    PRIMARY KEY (pseudonym, member)
);

CREATE INDEX erasure_reader_member ON erasure_reader (member);

-- The append-only trigger now admits the erase rewrite of 0003, which
-- replaces 0002's: a kept note's events move from the subject to its
-- pseudonym (data_subject "erased:<32 hex>:<n>"), scope and prev_scope
-- become scopes without a subject, group_key is cleared; key, audience,
-- prev_audience and everything else stay. Still only inside a transaction
-- that set glovebox_journal.erase_rewrite = on (transaction-local). The
-- flag guards against application mistakes; it is not a permission
-- boundary.
CREATE OR REPLACE FUNCTION record_event_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- The columns the erase rewrite may change, besides scan.
    moved CONSTANT text[] := ARRAY['scan', 'data_subject', 'scope', 'prev_scope', 'group_key'];
BEGIN
    IF (to_jsonb(NEW) - 'scan') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'scan') THEN
        RETURN NEW;
    END IF;
    IF current_setting('glovebox_journal.erase_rewrite', true) = 'on'
       AND OLD.data_subject <> '' AND NOT starts_with(OLD.data_subject, 'erased:')
       AND NEW.data_subject ~ '^erased:[0-9a-f]{32}:[1-9][0-9]*$'
       AND position('data_subject=' IN NEW.scope) = 0
       AND (NEW.prev_scope IS NULL) = (OLD.prev_scope IS NULL)
       AND (NEW.prev_scope IS NULL OR position('data_subject=' IN NEW.prev_scope) = 0)
       AND NEW.group_key IS NULL
       AND (to_jsonb(NEW) - moved) = (to_jsonb(OLD) - moved) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'record_event is append-only; only scan may change, or an erase rewrite (seq %)', OLD.seq;
END
$$;
