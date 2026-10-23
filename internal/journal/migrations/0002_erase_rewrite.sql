-- 0002_erase_rewrite: erase keeps the notes others wrote about an erased
-- subject, moved to the household under a pseudonym (spec 4.13, decision
-- 2026-10-23). Their events are rewritten in place, so they keep their
-- sequence numbers and the fold invariant; this widens the append-only
-- trigger by exactly that rewrite. Everything else stays as 0001 made it.
-- Described in docs/journal-schema.md.

CREATE OR REPLACE FUNCTION record_event_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    -- The columns the erase rewrite may change, besides scan.
    moved CONSTANT text[] := ARRAY['scan', 'data_subject', 'key', 'scope', 'prev_scope',
                                   'audience', 'prev_audience', 'group_key'];
BEGIN
    IF (to_jsonb(NEW) - 'scan') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'scan') THEN
        RETURN NEW;
    END IF;
    -- The erase rewrite: only inside a transaction that set the flag
    -- (set_config(..., true) is transaction-local), only from a subject to
    -- the household, the key only gaining a ":erased:<token>" suffix, no
    -- scope naming a subject, the audiences only shrinking and the group key
    -- (which may embed the subject) cleared.
    IF current_setting('glovebox_journal.erase_rewrite', true) = 'on'
       AND OLD.data_subject <> '' AND NEW.data_subject = ''
       AND starts_with(NEW.key, OLD.key || ':erased:') AND length(NEW.key) > length(OLD.key) + 8
       AND position('data_subject=' IN NEW.scope) = 0
       AND (NEW.prev_scope IS NULL) = (OLD.prev_scope IS NULL)
       AND (NEW.prev_scope IS NULL OR position('data_subject=' IN NEW.prev_scope) = 0)
       AND NEW.audience <@ OLD.audience
       AND (NEW.prev_audience IS NULL) = (OLD.prev_audience IS NULL)
       AND (NEW.prev_audience IS NULL OR NEW.prev_audience <@ OLD.prev_audience)
       AND NEW.group_key IS NULL
       AND (to_jsonb(NEW) - moved) = (to_jsonb(OLD) - moved) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'record_event is append-only; only scan may change, or an erase rewrite (seq %)', OLD.seq;
END
$$;
