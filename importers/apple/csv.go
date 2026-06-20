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

// appleQuoteReader wraps an io.Reader and rewrites Apple's non-RFC
// backslash-escaped double-quote (\") into the RFC-4180 doubled-quote ("") that
// encoding/csv understands. The rewrite is length-preserving for the common \"
// case (two bytes in, two bytes out). A backslash followed by any other byte is
// passed through unchanged (the data contains no such case, but this keeps the
// reader safe if one appears). A backslash that lands on a read boundary is held
// across Read calls via pendBsl so the lookahead is never lost.
type appleQuoteReader struct {
	r       io.Reader
	pendBsl bool // a backslash from a prior chunk awaits its following byte
}

func (a *appleQuoteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Loop until we emit at least one byte or hit a terminal error, so we never
	// return (0, nil) and stall a bufio-backed consumer.
	for {
		// Reserve room: when a backslash is pending, a non-quote follow byte
		// expands to two output bytes (the held backslash + that byte), so read
		// one fewer raw byte to guarantee the transform fits in p.
		rawCap := len(p)
		if a.pendBsl {
			rawCap = len(p) - 1
		}
		if rawCap <= 0 {
			// p has room for exactly one byte and a backslash is pending; flush
			// it literally to make progress (its follow byte is handled next call).
			p[0] = '\\'
			a.pendBsl = false
			return 1, nil
		}

		buf := make([]byte, rawCap)
		n, err := a.r.Read(buf)
		out := 0
		for i := 0; i < n; i++ {
			c := buf[i]
			if a.pendBsl {
				a.pendBsl = false
				if c == '"' {
					p[out] = '"'
					out++
					p[out] = '"'
					out++
					continue
				}
				// Lone backslash: emit it literally, then fall through to handle c.
				p[out] = '\\'
				out++
			}
			if c == '\\' {
				a.pendBsl = true
				continue
			}
			p[out] = c
			out++
		}
		// On EOF, a still-pending backslash has no follow byte; emit it literally.
		if err == io.EOF && a.pendBsl {
			p[out] = '\\'
			out++
			a.pendBsl = false
		}
		if out > 0 || err != nil {
			return out, err
		}
		// out == 0 with err == nil (e.g. the only byte read was a held
		// backslash): loop to read more rather than returning (0, nil).
	}
}

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
	// Real Apple media-services exports escape an embedded double-quote inside a
	// quoted field with a BACKSLASH (e.g. a book title rendered as
	// "\"Who Could That Be at This Hour?\"") instead of the RFC-4180
	// doubled-quote form (""). Go's encoding/csv only understands the doubled
	// form, so strict parsing aborts the whole file ("extraneous or missing
	// quote in quoted-field") and LazyQuotes silently merges the field into its
	// successors. appleQuoteReader translates the non-RFC \" sequence into ""
	// on the fly so the standard reader parses the field correctly; this is
	// unambiguous for these exports because every backslash present is part of a
	// \" pair (no lone backslashes appear in the data). The ragged-row policy is
	// unaffected: FieldsPerRecord still locks to the header width.
	cr := csv.NewReader(&appleQuoteReader{r: r})

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
