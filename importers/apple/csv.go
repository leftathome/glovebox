// csv.go -- header-aware, streaming CSV reader for Apple purchase exports.
//
// Apple's purchase/media-services CSVs carry a header row and can be large, so
// this reader pulls rows one at a time with encoding/csv's Reader.Read (never
// ReadAll) and hands each data row to a caller-supplied callback as a
// map[string]string keyed by the header columns. It is a pure utility consumed
// by a later wiring task (glovebox-ot7v).
//
// Ragged-row policy: the field count of the first record (the header) locks
// encoding/csv's FieldsPerRecord. Any later row with a different field count is
// a hard error wrapping csv.ErrFieldCount; iteration stops and the error is
// returned. We do NOT silently pad or truncate, so a malformed export fails
// loudly rather than producing rows with missing or misaligned columns.
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
)

// ErrCSVNoHeader is returned when the input is empty (no header row to key
// data rows against).
var ErrCSVNoHeader = errors.New("csv has no header row")

// ReadCSV streams CSV from r. The first record is treated as the header. Each
// subsequent record is delivered to fn as a map keyed by the header columns
// (column name -> field value). A fresh map is allocated per row, so callers
// may retain it without copying.
//
// Iteration stops and the offending error is returned on the first of: a read
// error, a ragged row (csv.ErrFieldCount per the package doc policy), or a
// non-nil error returned by fn. A nil return means every data row was
// delivered. An empty input yields ErrCSVNoHeader.
//
// Quoted fields and embedded separators/newlines are handled by encoding/csv.
func ReadCSV(r io.Reader, fn func(row map[string]string) error) error {
	cr := csv.NewReader(r)

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return ErrCSVNoHeader
		}
		return fmt.Errorf("read csv header: %w", err)
	}
	// After the first Read, cr.FieldsPerRecord is locked to len(header), so
	// subsequent ragged rows surface as csv.ErrFieldCount automatically.

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read csv row: %w", err)
		}

		row := make(map[string]string, len(header))
		for i, col := range header {
			row[col] = rec[i]
		}
		if err := fn(row); err != nil {
			return err
		}
	}
}
