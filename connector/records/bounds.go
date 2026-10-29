package records

// Bounds (spec 4.14). max_age and max_bytes apply to spool/ and
// spool/rejected/ alike:
//
//   - max_age applies to every file, the newest of a scope included. A
//     file's age is now minus the observed_at in its name (an unreadable
//     quarantined file: its mtime), so a clock that jumps forward expires
//     files early rather than keeping data at rest longer. A pending file
//     whose scope was last answered 409 unknown_subject is counted as
//     unknown_subject_expired, any other as max_age.
//   - on overflow of max_bytes, rejected files go first, oldest first
//     (they will never be sent); then the oldest intermediate pending
//     snapshots, i.e. any pending file that is not the newest of its
//     scope (record type and scope parts, the journal's identity: two
//     record types of one scope are two scopes). The newest pending file
//     of each scope is always kept, even if
//     the spool stays over the limit (logged once until it is under again).
//
// Enforce runs at the start of every drain pass and after every Put.

// Enforce applies max_age and max_bytes.
func (s *Spool) Enforce() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.enforceLocked()
	s.refreshGaugesLocked()
	return err
}

func (s *Spool) enforceLocked() error {
	pending, err := s.listPendingLocked()
	if err != nil {
		return err
	}
	rejected, err := s.listRejectedLocked()
	if err != nil {
		return err
	}
	now := s.now()
	maxAge := s.cfg.MaxAge()
	var firstErr error
	drop := func(e Entry, reason string) bool {
		if err := s.removeLocked(e); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return false
		}
		s.m.droppedN(reason, 1)
		return true
	}

	// max_age.
	var keptP, keptR []Entry
	for _, e := range pending {
		if now.Sub(e.ObservedAt) > maxAge {
			reason := DropMaxAge
			if s.unknown[e.ScopeKey] {
				reason = DropUnknownSubjectExpired
			}
			if drop(e, reason) {
				continue
			}
		}
		keptP = append(keptP, e)
	}
	// A pending file the listing could not read is aged by its name.
	for path, ts := range s.unreadable {
		if now.Sub(ts) > maxAge {
			drop(Entry{Path: path}, DropMaxAge)
		}
	}
	for _, e := range rejected {
		if now.Sub(e.ObservedAt) > maxAge && drop(e, DropMaxAge) {
			continue
		}
		keptR = append(keptR, e)
	}
	live := map[string]bool{}
	for _, e := range keptP {
		live[e.ScopeKey] = true
	}
	for k := range s.unknown {
		if !live[k] {
			delete(s.unknown, k)
		}
	}

	// max_bytes.
	var total int64
	for _, e := range keptP {
		total += e.Size
	}
	for _, e := range keptR {
		total += e.Size
	}
	limit := s.cfg.MaxBytes
	if total <= limit {
		s.overMax = false
		return firstErr
	}
	for _, e := range keptR {
		if total <= limit {
			break
		}
		if drop(e, DropMaxBytes) {
			total -= e.Size
		}
	}
	newest := map[string]string{} // scope key -> name of its newest file
	for _, e := range keptP {     // oldest first, so the last one wins
		newest[e.ScopeKey] = e.Name
	}
	for _, e := range keptP {
		if total <= limit {
			break
		}
		if newest[e.ScopeKey] == e.Name {
			continue
		}
		if drop(e, DropMaxBytes) {
			total -= e.Size
		}
	}
	if total > limit {
		if !s.overMax {
			s.log.Warn("records spool is over max_bytes with only the newest snapshot of each scope left; keeping them",
				"bytes", total, "max_bytes", limit)
		}
		s.overMax = true
	} else {
		s.overMax = false
	}
	return firstErr
}

// PurgeSubject deletes every pending and rejected file about subject (the
// journal answered 410 erased_subject: an erase bans all later data about
// the member, A10) and refuses later Puts about it until restart. Counted
// as dropped erased_subject. A quarantined unreadable file has no known
// subject and is left to max_age.
func (s *Spool) PurgeSubject(subject string) (int, error) {
	if subject == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erased[subject] = true
	defer s.refreshGaugesLocked()
	pending, err := s.listPendingLocked()
	if err != nil {
		return 0, err
	}
	rejected, err := s.listRejectedLocked()
	if err != nil {
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range append(pending, rejected...) {
		if e.DataSubject != subject {
			continue
		}
		if err := s.removeLocked(e); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	s.m.droppedN(DropErasedSubject, n)
	return n, firstErr
}

// MarkUnknown records whether the head of a scope was last answered 409
// unknown_subject, which only changes the label under which max_age drops
// its files. Kept in memory: losing it at restart is harmless.
func (s *Spool) MarkUnknown(scopeKey string, unknown bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if unknown {
		s.unknown[scopeKey] = true
	} else {
		delete(s.unknown, scopeKey)
	}
}
