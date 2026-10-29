package records

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Part is one extra scope part; its value goes through SafeScopeValue.
func Part(name, value string) journalapi.ScopePart {
	return journalapi.ScopePart{name, SafeScopeValue(value)}
}

// Scope builds a scope in the one order every connector uses: source,
// surface, data_subject, then extra parts. The order is part of the scope's
// identity in the journal (canonical form), so nothing else builds scopes.
func Scope(source, surface, dataSubject string, extra ...journalapi.ScopePart) []journalapi.ScopePart {
	s := []journalapi.ScopePart{{"source", source}, {"surface", surface}, {"data_subject", dataSubject}}
	return append(s, extra...)
}

// NewSnapshot assembles a snapshot with a fresh UUID. observed_at is UTC,
// truncated to milliseconds (the spool's file name carries it).
func NewSnapshot(recordType string, schemaVersion int, scope []journalapi.ScopePart, complete bool,
	rng *journalapi.CompleteRange, observedAt time.Time, recs []journalapi.Record) journalapi.Snapshot {
	if recs == nil {
		recs = []journalapi.Record{}
	}
	return journalapi.Snapshot{
		SnapshotID: uuid.NewString(), RecordType: recordType, SchemaVersion: schemaVersion,
		Scope: scope, Complete: complete, CompleteRange: rng,
		ObservedAt: observedAt.UTC().Truncate(time.Millisecond), Records: recs,
	}
}

// SchoolYear names the school year of a calendar date: 1 August to 31 July,
// written "2026-27" (spec 5.1).
func SchoolYear(d time.Time) string {
	y := d.Year()
	if d.Month() < time.August {
		y--
	}
	return fmt.Sprintf("%d-%02d", y, (y+1)%100)
}
