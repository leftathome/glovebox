package schoology

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// The assignment_detail surface.
//
// The assignment surface stages one short item per overdue/upcoming row:
// title, course, due date, status. This surface reads each of those
// assignments' own pages and stages what a parent can see there -- the
// description, the grading category and period, an assessment's status,
// whether the teacher has commented -- as a second item linked to the first
// by the assignment_id tag.
//
// ATTRIBUTION. GetAssignment takes no child: the page is the same whichever
// child the session is viewing. Which child an item belongs to therefore
// comes from exactly one place, the list the id was found in. The work list
// is built only from the rows GetOverdueSubmissions returned for a kid, an
// id that is in two kids' lists is staged once under each kid's own rule,
// and a page whose course or title does not agree with the list row is not
// staged at all.
//
// REQUEST BUDGET. One page read per assignment per poll would be the single
// largest source of requests in the connector, against a real parent
// account. A detail is read only when its list row is new or changed, or
// when it has not been read for RefreshHours; reads are spaced, capped per
// poll and stop at the end of the poll window.

// detailSurface is the surface name: the rule-key suffix, the checkpoint
// key prefix and the `kind` tag.
const detailSurface = "assignment_detail"

// detailParser is the parser label on receipts and parse-failure metrics.
const detailParser = "assignment_detail"

// Receipt error classes specific to this surface. The library's own codes
// (auth, permission, not_found, parse, ...) are used as-is.
const (
	detailClassCourseMismatch   = "course_mismatch"
	detailClassTitleMismatch    = "title_mismatch"
	detailClassCourseUnverified = "course_unverified"
	detailClassIDMismatch       = "id_mismatch"
	detailClassRowParse         = "row_parse"
	detailClassCommentsParse    = "comments_parse"
	detailClassUnexpectedPage   = "unexpected_page"
	detailClassStageRejected    = "stage_rejected"
	detailClassSurfaceDegraded  = "surface_degraded"
)

// Outcomes for schoology_assignment_detail_fetches_total.
const (
	detailOutcomeStaged    = "staged"
	detailOutcomeUnchanged = "unchanged"
	detailOutcomeRejected  = "rejected"
	detailOutcomeError     = "error"
	detailOutcomeDeferred  = "deferred"
)

// Size limits applied in the connector, on top of the library's own.
const (
	// maxDetailContentBytes caps the whole staged content.
	maxDetailContentBytes = 96 << 10
	// maxDetailCommentBytes caps everything derived from comments.
	maxDetailCommentBytes = 16 << 10
	// maxDetailMetaBytes is the staging limit for the subject, the sender
	// and each tag value (internal/staging/metadata.go).
	maxDetailMetaBytes = 1024
	// detailTruncationMark ends a metadata value that was cut.
	detailTruncationMark = " [truncated]"
)

// detailRequestAllowance is how long one request is allowed to take when
// deciding whether it can still finish inside the poll window: the HTTP
// client's 30 s timeout plus slack.
const detailRequestAllowance = 45 * time.Second

// detailStaleFactor promotes a refresh to the front of the queue once it is
// this many refresh intervals overdue, so a steady supply of new rows can
// never hold a refresh back forever.
const detailStaleFactor = 3

// detailWholesaleMin is the fewest details that must have been handled in a
// poll before "every one of them failed" is read as the pages having
// changed rather than as a coincidence.
const detailWholesaleMin = 3

// detailMatchKey is the rule key for a kid's detail items.
func detailMatchKey(kid string) string {
	return "schoology:" + kid + ":" + detailSurface
}

// KidAssignments is one kid's overdue/upcoming listing from this poll. It
// is the only source of work for the detail surface.
type KidAssignments struct {
	Kid  Kid
	Rows []*schoologylib.Assignment
}

// DetailDeps carries what ProcessAssignmentDetails needs.
type DetailDeps struct {
	Client     SchoologyClient
	Writer     connector.StagingBackend
	Matcher    *connector.RuleMatcher
	Checkpoint connector.Checkpoint
	Dedup      *ReceiptDedup
	Tel        *Telemetry
	LibVersion string
	Config     AssignmentDetailConfig

	// Location renders parsed due/posted times in the item body. nil is
	// UTC.
	Location *time.Location
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Sleep waits between requests; nil sleeps for real. It returns
	// ctx.Err() when the context ends first.
	Sleep func(ctx context.Context, d time.Duration) error
	// Deadline is the end of the poll window this poll belongs to. No
	// request is started that could not finish before it. Zero is no
	// deadline (a poll made outside any window).
	Deadline time.Time
}

// detailWork is one assignment whose detail is due to be read for one kid.
type detailWork struct {
	kid    *detailKidState
	row    *schoologylib.Assignment
	rowFP  string
	since  int64 // when the row first needed reading (fresh work)
	lastAt int64 // when it was last read (refresh work)
}

// detailKidState is one enabled kid's rule and checkpoint state for the
// duration of a poll.
type detailKidState struct {
	kid    Kid
	rule   connector.MatchResult
	rows   detailRows
	seen   detailSeen
	listed map[int64]bool
}

// detailFetch is the result of reading one assignment, shared between kids
// within a poll so an id listed for two of them is requested once.
type detailFetch struct {
	detail    *schoologylib.AssignmentDetail
	comments  schoologylib.AssignmentComments
	parseErrs schoologylib.ParseErrors
	// commentsErr is a comments page that could not be read as one; the
	// detail is still staged, with comments_state=unknown.
	commentsErr   error
	commentsClass string
	// err is a per-assignment refusal, with its receipt class.
	err      error
	errClass string
}

// detailReceipt accumulates one (kid, error class) bucket of a poll.
type detailReceipt struct {
	kid   Kid
	class string
	trace string
	ids   []string
	lines []string
}

// detailRun is the state of one ProcessAssignmentDetails call.
type detailRun struct {
	deps       DetailDeps
	now        time.Time
	requested  bool
	cache      map[int64]*detailFetch
	receipts   map[string]*detailReceipt
	order      []string
	staged     int
	errsLogged bool
	// handled and failed count the details that reached a verdict this
	// poll and how many of those were rejected or did not parse.
	handled int
	failed  int
}

// ProcessAssignmentDetails reads and stages assignment details for the kids
// the surface is enabled for.
//
// lists holds this poll's overdue/upcoming rows per kid; a kid whose list
// call failed must be left out (its remembered rows are then kept as they
// are). Returns the number of items staged and whether an error that should
// feed the schema-drift counter was logged. A per-assignment refusal
// (permission, not found, a course or title that does not match) is a
// receipt, not an error in that sense -- unless every detail handled in the
// poll ended that way, which is reported once as the surface being broken.
//
// An expired session is recorded as dedup (assignment_detail, auth) and
// stops the surface at once; pollNow turns that into the rejected-session
// exit shared with the other surfaces. An auth answer for one assignment is
// only believed after a session check that does not involve that assignment
// (see confirmAuth).
func ProcessAssignmentDetails(ctx context.Context, deps DetailDeps, lists []KidAssignments) (staged int, errsLogged bool) {
	if len(deps.Config.Kids) == 0 {
		return 0, false
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Sleep == nil {
		deps.Sleep = sleepCtx
	}
	if deps.Location == nil {
		deps.Location = time.UTC
	}

	run := &detailRun{
		deps:     deps,
		now:      deps.Now(),
		cache:    map[int64]*detailFetch{},
		receipts: map[string]*detailReceipt{},
	}
	work := run.plan(lists)
	if len(work) > 0 {
		ctx, span := deps.Tel.StartSpan(ctx, "schoology.poll.assignment_detail",
			attribute.Int("candidates", len(work)),
			attribute.Int("max_per_poll", deps.Config.maxPerPoll()),
		)
		run.execute(ctx, work)
		span.SetAttributes(attribute.Int("staged", run.staged))
		span.End()
	}
	run.checkWholesale()
	run.emitReceipts()
	return run.staged, run.errsLogged
}

// plan loads each enabled kid's state and returns the assignments whose
// detail is due, most urgent first.
//
// New or changed rows come first, then refreshes (longest-unread first).
// Within the first group the kids take turns -- one row each, round and
// round -- and the kid who goes first rotates from poll to poll, so one
// kid's long list cannot use up the per-poll cap ahead of another's. Each
// kid's own rows are ordered by how long they have been waiting: a row the
// cap deferred is remembered with the time it was first seen and goes ahead
// of rows that appeared later. A refresh that is several intervals overdue
// is promoted into the first group. Together these mean no assignment can
// be passed over indefinitely.
func (run *detailRun) plan(lists []KidAssignments) []detailWork {
	deps := run.deps
	refreshAfter := deps.Config.refreshInterval()
	var perKid [][]detailWork
	var refresh []detailWork

	for _, l := range lists {
		if !deps.Config.Enabled(l.Kid.Name) {
			continue
		}
		key := detailMatchKey(l.Kid.Name)
		rule, ok := deps.Matcher.Match(key)
		if !ok {
			slog.Warn("schoology assignment detail no rule match",
				"match_key", key, "kid", l.Kid.Name)
			continue
		}

		rows, err := loadDetailRows(deps.Checkpoint, l.Kid.Name)
		if err != nil {
			slog.Error("schoology assignment detail row state unreadable; starting fresh",
				"kid", l.Kid.Name, "error", err)
		}
		seen, err := loadDetailSeen(deps.Checkpoint, l.Kid.Name)
		if err != nil {
			slog.Error("schoology assignment detail seen-set unreadable; starting fresh",
				"kid", l.Kid.Name, "duplicate_risk", true, "error", err)
		}

		ks := &detailKidState{kid: l.Kid, rule: rule, rows: rows, seen: seen, listed: map[int64]bool{}}
		var ordered []*schoologylib.Assignment
		for _, a := range l.Rows {
			if a == nil || a.ID <= 0 || ks.listed[a.ID] {
				continue
			}
			ks.listed[a.ID] = true
			ordered = append(ordered, a)
		}
		if ageDetailRows(ks.rows, ks.listed) {
			run.saveRows(ks)
		}

		var fresh []detailWork
		for _, a := range ordered {
			fp := assignmentRowFingerprint(a)
			prev, known := ks.rows[a.ID]
			switch {
			case !known || prev.FetchedAt == 0 || prev.Fingerprint != fp:
				since := prev.Since
				if since == 0 {
					since = run.now.Unix()
				}
				fresh = append(fresh, detailWork{kid: ks, row: a, rowFP: fp, since: since})
			case !run.now.Before(prev.fetchedAt().Add(detailStaleFactor * refreshAfter)):
				fresh = append(fresh, detailWork{kid: ks, row: a, rowFP: fp, since: prev.FetchedAt})
			case !run.now.Before(prev.fetchedAt().Add(refreshAfter)):
				refresh = append(refresh, detailWork{kid: ks, row: a, rowFP: fp, lastAt: prev.FetchedAt})
			}
		}
		sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].since < fresh[j].since })
		perKid = append(perKid, fresh)
	}

	work := interleave(perKid, run.nextTurn(len(perKid)))
	sort.SliceStable(refresh, func(i, j int) bool { return refresh[i].lastAt < refresh[j].lastAt })
	return append(work, refresh...)
}

// detailTurnKey holds the index of the kid whose turn it is to go first.
const detailTurnKey = detailSurface + ":turn"

// nextTurn returns which of n kids goes first this poll and advances the
// stored turn for the next one.
func (run *detailRun) nextTurn(n int) int {
	if n <= 1 {
		return 0
	}
	cp := run.deps.Checkpoint
	turn := 0
	if v, ok := cp.Load(detailTurnKey); ok {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			turn = parsed % n
		}
	}
	if err := cp.Save(detailTurnKey, strconv.Itoa((turn+1)%n)); err != nil {
		slog.Error("schoology assignment detail turn save failed", "error", err)
	}
	return turn
}

// interleave takes one item from each queue in turn, starting with queue
// first, until all are empty.
func interleave(queues [][]detailWork, first int) []detailWork {
	var out []detailWork
	for round := 0; ; round++ {
		took := false
		for i := range queues {
			q := queues[(first+i)%len(queues)]
			if round < len(q) {
				out = append(out, q[round])
				took = true
			}
		}
		if !took {
			return out
		}
	}
}

// execute works through the plan within the per-poll cap. It returns early
// when the session is refused, when a request fails in a way that is not
// about one assignment (network, server, rate limit), when staging is
// failing, when the poll window is about to close or when ctx ends:
// continuing would only repeat the failure against the school's site.
func (run *detailRun) execute(ctx context.Context, work []detailWork) {
	deps := run.deps
	budget := deps.Config.maxPerPoll()
	deferred := 0

	for _, w := range work {
		if ctx.Err() != nil {
			return
		}
		kid := w.kid.kid
		f, cached := run.cache[w.row.ID]
		if !cached {
			if budget == 0 {
				deferred++
				deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeDeferred)
				run.markWaiting(w)
				continue
			}
			budget--
			var stop bool
			f, stop = run.fetch(ctx, kid, w.row.ID)
			if stop {
				return
			}
			run.cache[w.row.ID] = f
		}
		if stop := run.handle(ctx, w, f); stop {
			return
		}
	}

	if deferred > 0 {
		slog.Info("schoology assignment detail cap reached; remainder deferred to the next poll",
			"max_per_poll", deps.Config.maxPerPoll(), "deferred", deferred)
	}
}

// markWaiting records when a row the cap deferred first needed reading, so
// it goes ahead of later arrivals next poll.
func (run *detailRun) markWaiting(w detailWork) {
	ks := w.kid
	row := ks.rows[w.row.ID]
	if row.Since != 0 {
		return
	}
	row.Since = w.since
	ks.rows[w.row.ID] = row
	run.saveRows(ks)
}

// before gates every request: it refuses once ctx has ended or the poll
// window is about to close, and otherwise waits the configured spacing
// (except ahead of the first request). ok=false means make no request.
func (run *detailRun) before(ctx context.Context) (ok bool) {
	if ctx.Err() != nil {
		return false
	}
	deps := run.deps
	if !deps.Deadline.IsZero() && deps.Now().Add(detailRequestAllowance).After(deps.Deadline) {
		slog.Info("schoology assignment detail stopping at the end of the poll window; remainder deferred to the next poll",
			"window_end", deps.Deadline.Format(time.RFC3339))
		return false
	}
	if !run.requested {
		run.requested = true
		return true
	}
	if err := deps.Sleep(ctx, deps.Config.requestSpacing()); err != nil {
		return false
	}
	return ctx.Err() == nil
}

// fetch reads one assignment's page and, for an assessment, its comments
// page. stop=true means the surface must not make another request this
// poll; the result is then nil.
func (run *detailRun) fetch(ctx context.Context, kid Kid, id int64) (f *detailFetch, stop bool) {
	deps := run.deps
	if !run.before(ctx) {
		return nil, true
	}
	detail, parseErrs, err := getAssignmentTraced(ctx, deps.Client, deps.Tel, id)
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled mid-request: not a failure of anything.
			return nil, true
		}
		class, perItem := classifyDetailErr(err)
		if class == "auth" {
			if stop := run.confirmAuth(ctx, kid, id, "GetAssignment", err); stop {
				return nil, true
			}
			class, perItem = detailClassUnexpectedPage, true
		}
		if perItem {
			return &detailFetch{err: err, errClass: class}, false
		}
		run.hardFailure(kid, id, "GetAssignment", class, err.Error())
		return nil, true
	}
	if detail == nil {
		run.hardFailure(kid, id, "GetAssignment", "unknown", "library returned no detail and no error")
		return nil, true
	}
	f = &detailFetch{detail: detail, parseErrs: parseErrs, comments: detail.Comments}

	// A regular assignment carries its comment block inline. Only an
	// assessment has to be asked separately; for a regular assignment the
	// comments page answers 403, so asking would be a wasted request.
	if detail.Comments.State != schoologylib.CommentsStateUnknown || detail.Kind != schoologylib.AssignmentKindAssessment {
		return f, false
	}
	if !run.before(ctx) {
		return nil, true
	}
	comments, err := getAssignmentCommentsTraced(ctx, deps.Client, deps.Tel, id)
	switch {
	case err == nil && comments != nil:
		// State unavailable is a normal answer: nothing to report.
		f.comments = *comments
	case err == nil:
		// No block and no error: leave the state unknown.
	case ctx.Err() != nil:
		return nil, true
	default:
		class, perItem := classifyDetailErr(err)
		if class == "auth" {
			if stop := run.confirmAuth(ctx, kid, id, "GetAssignmentComments", err); stop {
				return nil, true
			}
			class, perItem = detailClassUnexpectedPage, true
		}
		if !perItem {
			run.hardFailure(kid, id, "GetAssignmentComments", class, err.Error())
			return nil, true
		}
		// Something was served but it is not a comments page we
		// understand. Keep the detail; the state stays unknown.
		if class == "parse" {
			class = detailClassCommentsParse
		}
		f.commentsErr, f.commentsClass = err, class
	}
	return f, false
}

// confirmAuth decides whether an auth error on one assignment means the
// session is dead.
//
// schoology-go reports ErrCodeAuth for any answer that ends on the site's
// front, home, parent or user pages, because that is where an expired
// session is sent. An assignment that simply redirects there -- one that was
// withdrawn, say -- looks exactly the same. Believing it would mark a live
// cookie as rejected and exit the process, and the same assignment would do
// it again after every restart.
//
// So the session is checked with a request that has nothing to do with the
// assignment. Only if that is refused too is the session treated as dead
// (stop=true, and the auth observation pollNow acts on is recorded). If the
// session is fine, stop=false and the caller treats the assignment as an
// unexpected page. If the check cannot be made (network), the surface stops
// for this poll without calling the session dead.
func (run *detailRun) confirmAuth(ctx context.Context, kid Kid, id int64, op string, authErr error) (stop bool) {
	deps := run.deps
	if !run.before(ctx) {
		return true
	}
	err := validateSessionTraced(ctx, deps.Client, deps.Tel)
	switch {
	case err == nil:
		slog.Warn("schoology assignment detail got an auth answer on a live session; treating it as an unexpected page",
			"kid", kid.Name, "assignment_id", id, "op", op)
		return false
	case ctx.Err() != nil:
		return true
	case schoologylib.IsAuthError(err):
		run.hardFailure(kid, id, op, "auth", authErr.Error())
		return true
	default:
		run.hardFailure(kid, id, "ValidateSession", classifyAssignmentsErr(err),
			"could not confirm whether the session is still valid: "+err.Error())
		return true
	}
}

// classifyDetailErr classifies a GetAssignment / GetAssignmentComments
// error and says whether it is about this one assignment (perItem) or about
// the session or the site as a whole.
//
// Per-item: permission, not found, and any answer that arrived but was not
// this assignment's page -- unparseable HTML, or a "server" error with no
// HTTP status, which is how the library reports a redirect to some other
// page, a non-HTML body or an oversized one. Those are properties of the
// item (an id that resolves to a page type the library does not read, say)
// and will be the same on the next poll, so they must not stop the surface:
// the assignment would otherwise sit at the head of the queue forever and
// starve every other one.
//
// Everything else -- the network, a rate limit, a 5xx -- ends the surface
// for this poll. "auth" is returned as not per-item; the caller confirms it
// first (confirmAuth).
func classifyDetailErr(err error) (class string, perItem bool) {
	class = classifyAssignmentsErr(err)
	switch class {
	case "permission", "not_found", "parse":
		return class, true
	case "server":
		var libErr *schoologylib.Error
		if errors.As(err, &libErr) && libErr.StatusCode == 0 {
			return detailClassUnexpectedPage, true
		}
	}
	return class, false
}

// hardFailure records a failure that ends the surface for this poll.
func (run *detailRun) hardFailure(kid Kid, id int64, op, class, msg string) {
	deps := run.deps
	run.errsLogged = true
	deps.Tel.RecordParseFailure(detailParser, class, kid.Name, detailSurface)
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeError)
	// The (parser, class) observation is what pollNow reads to detect an
	// expired session.
	deps.Dedup.Observe(detailParser, class, strconv.FormatInt(id, 10))
	run.addReceipt(kid, class, op, id, msg)
	slog.Warn("schoology parse failure",
		"parser", detailParser,
		"error_class", class,
		"kid", kid.Name,
		"assignment_id", id,
		"op", op,
	)
}

// handle decides what one fetched assignment means for one kid: reject it,
// leave it (unchanged), or stage it. stop=true means staging is failing and
// the surface must not go on.
func (run *detailRun) handle(ctx context.Context, w detailWork, f *detailFetch) (stop bool) {
	deps := run.deps
	ks := w.kid
	kid := ks.kid
	id := w.row.ID

	if f.err != nil {
		// This assignment only. Remember the read so it is retried at
		// the refresh interval, not every poll.
		run.reject(w, f.errClass, "GetAssignment", f.err.Error())
		return false
	}
	d := f.detail

	// The library refuses a page for a different id; check again here
	// because everything downstream trusts it.
	if d.ID != 0 && d.ID != id {
		run.reject(w, detailClassIDMismatch, "GetAssignment",
			fmt.Sprintf("assignment %d: the page returned is for assignment %d; not staged", id, d.ID))
		return false
	}
	if !sameCourse(w.row.CourseTitle, d.CourseTitle) {
		run.reject(w, detailClassCourseMismatch, "GetAssignment", fmt.Sprintf(
			"assignment %d: the detail page is filed under course %q but %s's list shows it under %q; "+
				"not staged, because the list is the only thing that ties an assignment to a child",
			id, oneLine(d.CourseTitle), kid.Name, oneLine(w.row.CourseTitle)))
		return false
	}
	if !sameTitle(w.row.Title, d.Title) {
		run.reject(w, detailClassTitleMismatch, "GetAssignment", fmt.Sprintf(
			"assignment %d: the detail page is titled %q but %s's list shows %q; "+
				"not staged, because the list is the only thing that ties an assignment to a child",
			id, oneLine(d.Title), kid.Name, oneLine(w.row.Title)))
		return false
	}

	run.handled++
	degraded := ""
	parseFailed := false
	for _, pe := range f.parseErrs {
		if pe == nil {
			continue
		}
		parseFailed = true
		run.errsLogged = true
		deps.Tel.RecordParseFailure(detailParser, detailClassRowParse, kid.Name, detailSurface)
		deps.Dedup.Observe(detailParser, detailClassRowParse, strconv.FormatInt(id, 10))
		run.addReceipt(kid, detailClassRowParse, pe.Op, id, pe.Error())
		if degraded == "" {
			degraded = pe.Error()
		}
	}
	if f.commentsErr != nil {
		parseFailed = true
		run.errsLogged = true
		deps.Tel.RecordParseFailure(detailParser, f.commentsClass, kid.Name, detailSurface)
		deps.Dedup.Observe(detailParser, f.commentsClass, strconv.FormatInt(id, 10))
		run.addReceipt(kid, f.commentsClass, "GetAssignmentComments", id, f.commentsErr.Error())
	}
	if parseFailed {
		run.failed++
	}

	// The list names a course and the page shows none: the cross-check
	// could not be made. That is what a changed breadcrumb would look like,
	// so say so -- on the item and in a receipt -- rather than pass silently.
	courseUnverified := strings.TrimSpace(w.row.CourseTitle) != "" && strings.TrimSpace(d.CourseTitle) == ""
	if courseUnverified {
		deps.Tel.RecordParseFailure(detailParser, detailClassCourseUnverified, kid.Name, detailSurface)
		deps.Dedup.Observe(detailParser, detailClassCourseUnverified, strconv.FormatInt(id, 10))
		run.addReceipt(kid, detailClassCourseUnverified, "GetAssignment", id, fmt.Sprintf(
			"assignment %d: the detail page shows no course, so it could not be checked against %s's list; staged with course_unverified=true",
			id, kid.Name))
		if degraded == "" {
			degraded = "the detail page shows no course to check against the list row"
		}
	}

	opts := buildDetailItemOptions(w.row, d, f.comments, ks.rule)
	if courseUnverified {
		opts.Tags["course_unverified"] = "true"
	}
	body := buildDetailBody(w.row, d, f.comments, deps.Location)
	hash := detailContentHash(opts, body)

	if ks.seen[id] == hash {
		deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeUnchanged)
		run.rememberRow(w)
		return false
	}

	if degraded != "" {
		opts = BuildDegradedOptions(opts, degraded, "", strconv.FormatInt(id, 10), detailSurface)
	}
	if err := stageAssignmentItem(ctx, deps.Tel, deps.Writer, opts, []byte(body), id); err != nil {
		if ctx.Err() != nil {
			return true
		}
		if stagePermanent(err) {
			// Staging will refuse this item every time. Reading the
			// page again next poll would not change that.
			run.handled--
			if parseFailed {
				run.failed--
			}
			run.reject(w, detailClassStageRejected, "stage", fmt.Sprintf(
				"assignment %d: staging refused the item: %v", id, err))
			return false
		}
		// Staging is unreachable or failing. Nothing is marked seen or
		// remembered, and no more requests are spent on pages that could
		// not be staged either.
		run.errsLogged = true
		deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeError)
		slog.Warn("schoology assignment detail stage failed; stopping the surface for this poll",
			"kid", kid.Name, "assignment_id", id, "error", err)
		return true
	}
	run.staged++
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeStaged)

	ks.seen[id] = hash
	if err := saveDetailSeen(deps.Checkpoint, kid.Name, ks.seen, ks.listed); err != nil {
		slog.Error("schoology assignment detail checkpoint save failed",
			"kid", kid.Name, "assignment_id", id, "duplicate_risk", true, "error", err)
	}
	run.rememberRow(w)
	return false
}

// stagePermanent reports whether a staging error will repeat for the same
// item: the ingest endpoint rejected it (400/413, a PermanentError) or its
// metadata failed validation.
func stagePermanent(err error) bool {
	return connector.IsPermanent(err) || strings.Contains(err.Error(), "metadata validation")
}

// reject records a per-assignment refusal: a receipt, no staged item, and
// the read remembered so the assignment is next tried at the refresh
// interval (or sooner if its list row changes). It does not feed the
// schema-drift counter on its own (see checkWholesale).
func (run *detailRun) reject(w detailWork, class, trace, msg string) {
	kid := w.kid.kid
	deps := run.deps
	run.handled++
	run.failed++
	deps.Tel.RecordParseFailure(detailParser, class, kid.Name, detailSurface)
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeRejected)
	deps.Dedup.Observe(detailParser, class, strconv.FormatInt(w.row.ID, 10))
	run.addReceipt(kid, class, trace, w.row.ID, msg)
	slog.Warn("schoology assignment detail not staged",
		"kid", kid.Name, "assignment_id", w.row.ID, "error_class", class)
	run.rememberRow(w)
}

// checkWholesale reports the surface as broken when every detail handled in
// the poll was rejected or did not parse. One odd assignment is a receipt;
// all of them at once is what a change to Schoology's pages looks like, and
// that must reach the schema-drift counter.
func (run *detailRun) checkWholesale() {
	if run.handled < detailWholesaleMin || run.failed < run.handled {
		return
	}
	run.errsLogged = true
	run.deps.Tel.RecordParseFailure(detailParser, detailClassSurfaceDegraded, "", detailSurface)
	run.deps.Dedup.Observe(detailParser, detailClassSurfaceDegraded, "")
	key := "|" + detailClassSurfaceDegraded
	run.receipts[key] = &detailReceipt{
		class: detailClassSurfaceDegraded,
		trace: "ProcessAssignmentDetails",
		ids:   []string{strconv.Itoa(run.handled)},
		lines: []string{fmt.Sprintf(
			"all %d assignment details handled in this poll were rejected or did not parse; "+
				"Schoology's assignment pages may have changed (see the per-assignment receipts)", run.handled)},
	}
	run.order = append(run.order, key)
	slog.Error("schoology assignment detail surface degraded", "handled", run.handled)
}

// rememberRow records that the row was read now.
func (run *detailRun) rememberRow(w detailWork) {
	ks := w.kid
	ks.rows[w.row.ID] = detailRow{Fingerprint: w.rowFP, FetchedAt: run.now.Unix()}
	run.saveRows(ks)
}

func (run *detailRun) saveRows(ks *detailKidState) {
	if err := saveDetailRows(run.deps.Checkpoint, ks.kid.Name, ks.rows); err != nil {
		slog.Error("schoology assignment detail row state save failed",
			"kid", ks.kid.Name, "error", err)
	}
}

// addReceipt adds one affected assignment to its (kid, class) bucket.
func (run *detailRun) addReceipt(kid Kid, class, trace string, id int64, line string) {
	key := kid.Name + "|" + class
	r, ok := run.receipts[key]
	if !ok {
		r = &detailReceipt{kid: kid, class: class, trace: trace}
		run.receipts[key] = r
		run.order = append(run.order, key)
	}
	r.ids = append(r.ids, strconv.FormatInt(id, 10))
	r.lines = append(r.lines, line)
}

// emitReceipts stages one receipt per (kid, error class) seen this poll,
// after the work is done so each carries its full count. The other
// surfaces key receipts on (parser, class) alone; this one adds the kid
// because the surface runs once for all kids and a receipt that named only
// the first would misreport whose assignments were affected.
func (run *detailRun) emitReceipts() {
	deps := run.deps
	for _, key := range run.order {
		r := run.receipts[key]
		in := ReceiptInputs{
			Parser:            detailParser,
			ErrorClass:        r.class,
			ErrorMsg:          r.lines[0],
			Trace:             r.trace,
			SourceURL:         "schoology://assignment/" + r.ids[0],
			TargetKid:         r.kid.Name,
			TargetContentType: detailSurface,
			LibVersion:        deps.LibVersion,
			AffectedCount:     len(r.ids),
			AffectedItemIDs:   r.ids,
		}
		if r.class == detailClassSurfaceDegraded {
			in.SourceURL = "schoology://assignment"
			in.AffectedCount = run.handled
			in.AffectedItemIDs = nil
		}
		emitAssignmentsReceipt(deps.Writer, deps.Matcher, in, []byte(strings.Join(r.lines, "\n")+"\n"))
		deps.Tel.RecordParseFailureReceipt(detailParser, r.class)
	}
}

// getAssignmentTraced wraps client.GetAssignment in the
// schoology.lib.GetAssignment span.
func getAssignmentTraced(ctx context.Context, client SchoologyClient, tel *Telemetry, id int64) (*schoologylib.AssignmentDetail, schoologylib.ParseErrors, error) {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.GetAssignment",
		attribute.Int64("assignment_id", id),
	)
	defer span.End()
	return client.GetAssignment(ctx, id)
}

// getAssignmentCommentsTraced wraps client.GetAssignmentComments in the
// schoology.lib.GetAssignmentComments span.
func getAssignmentCommentsTraced(ctx context.Context, client SchoologyClient, tel *Telemetry, id int64) (*schoologylib.AssignmentComments, error) {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.GetAssignmentComments",
		attribute.Int64("assignment_id", id),
	)
	defer span.End()
	return client.GetAssignmentComments(ctx, id)
}

// validateSessionTraced wraps client.ValidateSession in the
// schoology.lib.ValidateSession span.
func validateSessionTraced(ctx context.Context, client SchoologyClient, tel *Telemetry) error {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.ValidateSession")
	defer span.End()
	return client.ValidateSession(ctx)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// assignmentRowFingerprint hashes the fields of a list row. When it changes
// the detail is read again.
func assignmentRowFingerprint(a *schoologylib.Assignment) string {
	due := ""
	if !a.DueAt.IsZero() {
		due = strconv.FormatInt(a.DueAt.Unix(), 10)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		a.Title, a.CourseTitle, due, string(a.Status), a.URL,
	}, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// foldText folds case and runs of whitespace for comparison.
func foldText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// normalizeCourse folds a course title for comparison: case, runs of
// whitespace, and the spacing around a colon.
func normalizeCourse(s string) string {
	s = foldText(s)
	s = strings.ReplaceAll(s, " :", ":")
	s = strings.ReplaceAll(s, ": ", ":")
	return s
}

// sameCourse reports whether a list row's course and a detail page's course
// can be the same course.
//
// The two pages do not print a course the same way: the overdue/upcoming
// list shows the course name alone ("Example Course A") and the detail
// page's breadcrumb shows it with its section ("Example Course A: Section
// 1"). So one title may be the other followed by a ":" suffix; that held
// for every assignment checked on the live site. Anything else is a
// mismatch. An empty title on either side cannot be compared and is not a
// mismatch here (a page with no course is flagged separately).
func sameCourse(listTitle, detailTitle string) bool {
	a, b := normalizeCourse(listTitle), normalizeCourse(detailTitle)
	if a == "" || b == "" || a == b {
		return true
	}
	return strings.HasPrefix(b, a+":") || strings.HasPrefix(a, b+":")
}

// sameTitle reports whether a list row's title and a detail page's title
// agree. Unlike the course, the two pages print the title identically, so
// only case and whitespace are folded. An empty title on either side cannot
// be compared.
func sameTitle(listTitle, detailTitle string) bool {
	a, b := foldText(listTitle), foldText(detailTitle)
	return a == "" || b == "" || a == b
}

// detailCourse picks the course title to stage: the detail page's (it names
// the section) or, when the page shows none, the list row's.
func detailCourse(row *schoologylib.Assignment, d *schoologylib.AssignmentDetail) string {
	if c := oneLine(d.CourseTitle); c != "" {
		return c
	}
	return oneLine(row.CourseTitle)
}

// detailTitle picks the title to stage, with the list row as a fallback for
// a page whose heading could not be read.
func detailTitle(row *schoologylib.Assignment, d *schoologylib.AssignmentDetail) string {
	if t := oneLine(d.Title); t != "" {
		return t
	}
	if t := oneLine(row.Title); t != "" {
		return t
	}
	return "(untitled assignment)"
}

// commentsStateTag renders a comment state as a tag value.
func commentsStateTag(s schoologylib.CommentsState) string {
	if s == schoologylib.CommentsStateUnknown {
		return "unknown"
	}
	return string(s)
}

// commentCount is the number of top-level entries in a comment block.
func commentCount(c schoologylib.AssignmentComments) int {
	n := 0
	for _, e := range c.Comments {
		if e != nil {
			n++
		}
	}
	return n
}

// commentTextMapped is the switch for staging comment text.
//
// No page with a real comment had been seen when this was written, so the
// library cannot tell a comment's author, time and body apart and says so
// with LayoutUnmapped. Text read that way is not staged, and is not part of
// the item's identity: only the state and the number of entries are. When
// the library maps the layout it stops setting LayoutUnmapped, and the text
// is then staged through renderComments.
func commentTextMapped(c schoologylib.AssignmentComments) bool {
	return c.State == schoologylib.CommentsStatePopulated && !c.LayoutUnmapped
}

// oneLine makes s safe as a single-line value: unsafe control characters
// are dropped and every kind of line break becomes a space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(cleanText(s, false)), " ")
}

// cleanText drops the control characters staging refuses and neutralises
// the line separators a renderer might honour. With keepNewlines, "\n" is
// kept and everything else that breaks a line becomes a space.
func cleanText(s string, keepNewlines bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			if keepNewlines {
				return r
			}
			return ' '
		case r == '\t', r == '\r', r == 0x2028, r == 0x2029, r == 0x85, r == '\v', r == '\f':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			return -1
		}
		return r
	}, s)
}

// cutUTF8 returns s cut to at most max bytes on a rune boundary, and
// whether it was cut.
func cutUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max < 0 {
		max = 0
	}
	cut := max
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut], true
}

// metaValue fits a value into a metadata field: one line, within the
// staging length limit, with a marker when it had to be cut.
func metaValue(s string) (string, bool) {
	s = oneLine(s)
	if len(s) <= maxDetailMetaBytes {
		return s, false
	}
	head, _ := cutUTF8(s, maxDetailMetaBytes-len(detailTruncationMark))
	return head + detailTruncationMark, true
}

// buildDetailItemOptions composes the staged item's metadata. Optional
// fields the page does not show are left out rather than staged empty.
// Every value is teacher-authored or page-derived, so each is reduced to a
// single line within the staging limits before it is used: an over-long
// title must cost a marker, not the item.
func buildDetailItemOptions(
	row *schoologylib.Assignment,
	d *schoologylib.AssignmentDetail,
	comments schoologylib.AssignmentComments,
	rule connector.MatchResult,
) connector.ItemOptions {
	truncated := false
	fit := func(s string) string {
		v, cut := metaValue(s)
		truncated = truncated || cut
		return v
	}

	course := fit(detailCourse(row, d))
	sender := course
	if sender == "" {
		// Staging validation rejects an empty Sender.
		sender = "schoology-course"
	}
	kind := string(d.Kind)
	if kind == "" {
		kind = "unknown"
	}
	tags := map[string]string{
		"kind":            detailSurface,
		"assignment_id":   strconv.FormatInt(row.ID, 10),
		"assignment_kind": fit(kind),
		"comments_state":  fit(commentsStateTag(comments.State)),
		"comment_count":   strconv.Itoa(commentCount(comments)),
		"has_description": strconv.FormatBool(d.DescriptionText != ""),
	}
	setTag := func(k, v string) {
		if v = fit(v); v != "" {
			tags[k] = v
		}
	}
	setTag("course", course)
	setTag("category", d.Category)
	setTag("grading_period", d.GradingPeriod)
	setTag("due_raw", d.DueRaw)
	if !d.DueAt.IsZero() {
		tags["due_date"] = d.DueAt.UTC().Format(time.RFC3339)
	}
	if !d.PostedAt.IsZero() {
		tags["posted"] = d.PostedAt.UTC().Format(time.RFC3339)
	} else {
		setTag("posted", d.PostedRaw)
	}
	setTag("assessment_status", d.AssessmentStatus)
	subject := fit(detailTitle(row, d))
	if truncated {
		tags["metadata_truncated"] = "true"
	}

	return connector.ItemOptions{
		Source:           "schoology",
		Sender:           sender,
		Subject:          subject,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: rule.Destination,
		ContentType:      "text/plain",
		Tags:             tags,
		RuleTags:         rule.Tags,
		DataSubject:      rule.DataSubject,
		Audience:         rule.Audience,
	}
}

// renderedTime prints a page's own date text followed by the parsed
// instant, when there is one.
func renderedTime(raw string, at time.Time, loc *time.Location) string {
	raw = oneLine(raw)
	if at.IsZero() {
		return raw
	}
	return raw + " (" + at.In(loc).Format(time.RFC3339) + ")"
}

// Framing for teacher-authored multi-line text. Every line of such text is
// written with untrustedPrefix, between a BEGIN and an END line, so nothing
// a teacher types can start a line at column zero and pass for one of the
// connector's own "Field: value" lines or section headings.
const (
	untrustedPrefix      = "| "
	descriptionBegin     = "----- BEGIN DESCRIPTION: untrusted teacher-authored text, each line prefixed \"| \" -----"
	descriptionEnd       = "----- END DESCRIPTION -----"
	descriptionCutMarker = "[The description was cut off at the size limit.]"
	commentsBegin        = "----- BEGIN TEACHER COMMENTS: untrusted teacher-authored text, each line prefixed \"| \" -----"
	commentsEnd          = "----- END TEACHER COMMENTS -----"
	commentsCutMarker    = "[The comments were cut off at the size limit.]"
	contentCutMarker     = "[The content was cut off at the connector's size limit.]"
)

// writeUntrusted writes text line by line with untrustedPrefix, stopping
// before budget bytes are exceeded. It reports whether anything was left
// out.
func writeUntrusted(b *strings.Builder, text string, budget int) (cut bool) {
	for _, line := range strings.Split(strings.TrimRight(cleanText(text, true), "\n"), "\n") {
		if len(untrustedPrefix)+len(line)+1 > budget {
			if room := budget - len(untrustedPrefix) - 1; room > 0 {
				head, _ := cutUTF8(line, room)
				b.WriteString(untrustedPrefix + head + "\n")
			}
			return true
		}
		b.WriteString(untrustedPrefix + line + "\n")
		budget -= len(untrustedPrefix) + len(line) + 1
	}
	return false
}

// renderComments writes the comments section.
//
// While the layout is unmapped (see commentTextMapped) it is one fixed line
// with the number of entries and no teacher text at all.
func renderComments(comments schoologylib.AssignmentComments) string {
	if comments.State != schoologylib.CommentsStatePopulated {
		return ""
	}
	n := commentCount(comments)
	if !commentTextMapped(comments) {
		return fmt.Sprintf("\nTeacher comments: %d present (text not captured yet)\n", n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nTeacher comments: %d\n%s\n", n, commentsBegin)
	budget := maxDetailCommentBytes
	cut := false
	for _, c := range comments.Comments {
		if c == nil {
			continue
		}
		before := b.Len()
		if writeUntrusted(&b, c.Text, budget) {
			cut = true
			break
		}
		budget -= b.Len() - before
		if c.Truncated {
			cut = true
		}
		if budget > len(untrustedPrefix)+1 {
			b.WriteString(untrustedPrefix + "\n")
			budget -= len(untrustedPrefix) + 1
		}
	}
	b.WriteString(commentsEnd + "\n")
	if cut {
		b.WriteString(commentsCutMarker + "\n")
	}
	return b.String()
}

// buildDetailBody projects the detail into the plain text that is staged as
// content, in the style of buildAssignmentBody. The description is the
// library's plain-text rendering; the raw HTML is never staged. The whole
// is kept within maxDetailContentBytes, the description giving way first.
func buildDetailBody(
	row *schoologylib.Assignment,
	d *schoologylib.AssignmentDetail,
	comments schoologylib.AssignmentComments,
	loc *time.Location,
) string {
	var b strings.Builder
	line := func(label, value string) {
		if value = oneLine(value); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, value)
		}
	}
	line("Assignment", detailTitle(row, d))
	line("Kind", string(d.Kind))
	line("Course", detailCourse(row, d))
	line("Folder", d.Folder)
	line("Category", d.Category)
	line("Period", d.GradingPeriod)
	if oneLine(d.DueRaw) != "" {
		line("Due", renderedTime(d.DueRaw, d.DueAt, loc))
	} else {
		b.WriteString("Due: no due date listed\n")
	}
	if oneLine(d.PostedRaw) != "" {
		line("Posted", renderedTime(d.PostedRaw, d.PostedAt, loc))
	}
	line("Status", d.AssessmentStatus)
	line("Link", row.URL)

	tail := renderComments(comments)

	if d.DescriptionText != "" {
		b.WriteString("\nDescription:\n" + descriptionBegin + "\n")
		reserve := len(descriptionEnd) + len(descriptionCutMarker) + len(tail) + 64
		cut := writeUntrusted(&b, d.DescriptionText, maxDetailContentBytes-b.Len()-reserve)
		b.WriteString(descriptionEnd + "\n")
		if cut || d.DescriptionTruncated {
			b.WriteString(descriptionCutMarker + "\n")
		}
	}
	b.WriteString(tail)

	out := b.String()
	if len(out) > maxDetailContentBytes {
		// Not reachable with the budgets above; kept as the hard stop.
		head, _ := cutUTF8(out, maxDetailContentBytes-len(contentCutMarker)-2)
		out = head + "\n" + contentCutMarker + "\n"
	}
	return out
}

// detailContentHash hashes everything that is staged for a detail item and
// nothing that is not: sender, subject, the tags and the body. The staging
// timestamp, the routing fields and the fetch time are left out, so the
// hash moves exactly when a reader of the item would see something
// different.
func detailContentHash(opts connector.ItemOptions, body string) string {
	keys := make([]string, 0, len(opts.Tags))
	for k := range opts.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0x1f})
	}
	write(opts.Sender)
	write(opts.Subject)
	for _, k := range keys {
		write(k)
		write(opts.Tags[k])
	}
	write(body)
	return hex.EncodeToString(h.Sum(nil)[:16])
}
