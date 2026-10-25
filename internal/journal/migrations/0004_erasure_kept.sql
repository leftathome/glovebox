-- 0004_erasure_kept: a kept note's history is released to its frozen
-- readers only from the audience they were frozen from; its author sees all
-- of it (spec 4.13, review of 2026-10-24). Described in
-- docs/journal-schema.md.

-- One row per kept note: the audience its readers were frozen from (the
-- note's audience at erase time, sorted and distinct as stored).
CREATE TABLE erasure_kept (
    pseudonym text   PRIMARY KEY CHECK (pseudonym ~ '^erased:[0-9a-f]{32}:[1-9][0-9]*$'),
    audience  text[] NOT NULL CHECK (cardinality(audience) > 0)
);

-- Whether the reader is the note's author (who sees its whole history).
ALTER TABLE erasure_reader ADD COLUMN author boolean NOT NULL DEFAULT false;

-- Rows kept before this migration: frozen from their current audience;
-- the author is the on_behalf_of part of the key.
INSERT INTO erasure_kept (pseudonym, audience)
    SELECT DISTINCT c.data_subject, c.audience FROM record_current c
    WHERE c.data_subject IN (SELECT pseudonym FROM erasure_reader);
UPDATE erasure_reader r SET author = (split_part(c.key, ':', 2) = r.member)
    FROM record_current c WHERE c.data_subject = r.pseudonym;
DELETE FROM erasure_reader r WHERE NOT EXISTS (SELECT 1 FROM erasure_kept k WHERE k.pseudonym = r.pseudonym);

ALTER TABLE erasure_reader ADD CONSTRAINT erasure_reader_kept
    FOREIGN KEY (pseudonym) REFERENCES erasure_kept (pseudonym) ON DELETE CASCADE;
