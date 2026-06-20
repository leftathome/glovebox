package main

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"
)

func TestReadCSVMapsHeaderToValues(t *testing.T) {
	in := "Item,Price,\"Description\"\n" +
		"Song,1.29,\"A, great track\"\n" +
		"Album,9.99,Greatest Hits\n"

	var rows []map[string]string
	err := ReadCSV(strings.NewReader(in), func(row map[string]string) error {
		// Copy to detach from any reuse by the reader.
		cp := make(map[string]string, len(row))
		for k, v := range row {
			cp[k] = v
		}
		rows = append(rows, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadCSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["Item"] != "Song" || rows[0]["Price"] != "1.29" || rows[0]["Description"] != "A, great track" {
		t.Fatalf("row0 = %v", rows[0])
	}
	if rows[1]["Item"] != "Album" || rows[1]["Price"] != "9.99" || rows[1]["Description"] != "Greatest Hits" {
		t.Fatalf("row1 = %v", rows[1])
	}
}

// Documented policy: a data row whose field count differs from the header is a
// hard error (wrapping encoding/csv's ErrFieldCount); iteration stops.
func TestReadCSVRaggedRowIsError(t *testing.T) {
	in := "A,B,C\n" +
		"1,2,3\n" +
		"4,5\n" // ragged: only 2 fields

	var seen int
	err := ReadCSV(strings.NewReader(in), func(row map[string]string) error {
		seen++
		return nil
	})
	if err == nil {
		t.Fatal("expected error for ragged row, got nil")
	}
	if !errors.Is(err, csv.ErrFieldCount) {
		t.Fatalf("error = %v, want wrapping csv.ErrFieldCount", err)
	}
	if seen != 1 {
		t.Fatalf("callback ran %d times before ragged row, want 1", seen)
	}
}

// Real Apple media-services exports use non-RFC-4180 backslash-escaped quotes
// inside quoted fields (e.g. a book title rendered as "\"Who Could That Be at
// This Hour?\""). Go's strict encoding/csv rejects this with "extraneous or
// missing \" in quoted-field". The reader must tolerate it (LazyQuotes) so a
// single malformed-by-spec title does not abort the entire purchase import.
func TestReadCSVTolueratesBackslashEscapedQuotes(t *testing.T) {
	in := "Item Description,Content Type\n" +
		"Normal App,iOS and tvOS Apps\n" +
		"\"\\\"Who Could That Be at This Hour?\\\"\",Books\n" +
		"Another App,iOS and tvOS Apps\n"

	var rows []map[string]string
	err := ReadCSV(strings.NewReader(in), func(row map[string]string) error {
		cp := make(map[string]string, len(row))
		for k, v := range row {
			cp[k] = v
		}
		rows = append(rows, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadCSV with backslash-escaped quotes: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if !strings.Contains(rows[1]["Item Description"], "Who Could That Be at This Hour") {
		t.Fatalf("row1 title = %q, want it to contain the book title", rows[1]["Item Description"])
	}
	if rows[2]["Item Description"] != "Another App" {
		t.Fatalf("row2 title = %q, want \"Another App\" (parsing must continue past the escaped row)", rows[2]["Item Description"])
	}
}

func TestReadCSVEmptyInputIsNoHeader(t *testing.T) {
	err := ReadCSV(strings.NewReader(""), func(map[string]string) error { return nil })
	if !errors.Is(err, ErrCSVNoHeader) {
		t.Fatalf("error = %v, want ErrCSVNoHeader", err)
	}
}

func TestReadCSVCallbackErrorStops(t *testing.T) {
	in := "A\n1\n2\n3\n"
	sentinel := errors.New("stop")
	var seen int
	err := ReadCSV(strings.NewReader(in), func(map[string]string) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if seen != 1 {
		t.Fatalf("callback ran %d times, want 1 (stopped on first error)", seen)
	}
}
