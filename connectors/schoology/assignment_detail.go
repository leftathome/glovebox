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
// description, the grading category and period, an assessment's status, the
// teacher's comments -- as a second item linked to the first by the
// assignment_id tag.
//
// ATTRIBUTION. GetAssignment takes no child: the page is the same whichever
// child the session is viewing. Which child an item belongs to therefore
// comes from exactly one place, the list the id was found in. The work list
// is built only from the rows GetOverdueSubmissions returned for a kid, an
// id that is in two kids' lists is staged once under each kid's own rule,
// and a page whose course does not agree with the list row is not staged at
// all.
//
// REQUEST BUDGET. One page read per assignment per poll would be the single
// largest source of requests in the connector, against a real parent
// account. A detail is read only when its list row is new or changed, or
// when it has not been read for RefreshHours; reads are spaced and capped
// per poll.

// detailSurface is the surface name: the rule-key suffix, the checkpoint
// key prefix and the `kind` tag.
const detailSurface = "assignment_detail"

// detailParser is the parser label on receipts and parse-failure metrics.
const detailParser = "assignment_detail"

// Receipt error classes specific to this surface. The library's own codes
// (auth, permission, not_found, parse, ...) are used as-is.
const (
	detailClassCourseMismatch = "course_mismatch"
	detailClassIDMismatch     = "id_mismatch"
	detailClassRowParse       = "row_parse"
	detailClassCommentsParse  = "comments_parse"
	detailClassUnexpectedPage = "unexpected_page"
)

// Outcomes for schoology_assignment_detail_fetches_total.
const (
	detailOutcomeStaged    = "staged"
	detailOutcomeUnchanged = "unchanged"
	detailOutcomeRejected  = "rejected"
	detailOutcomeError     = "error"
	detailOutcomeDeferred  = "deferred"
)

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
}

// detailWork is one assignment whose detail is due to be read for one kid.
type detailWork struct {
	kid     *detailKidState
	row     *schoologylib.Assignment
	rowFP   string
	refresh bool
	lastAt  int64
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
	// commentsErr is a comments page that could not be understood; the
	// detail is still staged, with comments_state=unknown.
	commentsErr error
	// err is a per-assignment refusal (permission, not found).
	err error
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
}

// ProcessAssignmentDetails reads and stages assignment details for the kids
// the surface is enabled for.
//
// lists holds this poll's overdue/upcoming rows per kid; a kid whose list
// call failed must be left out (its remembered rows are then kept as they
// are). Returns the number of items staged and whether an error that should
// feed the schema-drift counter was logged. A per-assignment refusal
// (permission, not found, a course that does not match) is a receipt, not
// an error in that sense.
//
// An expired session is recorded as dedup (assignment_detail, auth) and
// stops the surface at once; pollNow turns that into the rejected-session
// exit shared with the other surfaces.
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
	run.emitReceipts()
	return run.staged, run.errsLogged
}

// plan loads each enabled kid's state and returns the assignments whose
// detail is due, most urgent first: every new or changed row (kids in
// config order, rows in list order), then refreshes, longest-unread first.
// With the per-poll cap, that ordering means a backlog of refreshes can
// never hold back a new assignment.
func (run *detailRun) plan(lists []KidAssignments) []detailWork {
	deps := run.deps
	refreshAfter := deps.Config.refreshInterval()
	var fresh, refresh []detailWork

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
		if pruneDetailRows(ks.rows, ks.listed) {
			if err := saveDetailRows(deps.Checkpoint, l.Kid.Name, ks.rows); err != nil {
				slog.Error("schoology assignment detail row state save failed",
					"kid", l.Kid.Name, "error", err)
			}
		}

		for _, a := range ordered {
			fp := assignmentRowFingerprint(a)
			prev, known := ks.rows[a.ID]
			switch {
			case !known || prev.Fingerprint != fp:
				fresh = append(fresh, detailWork{kid: ks, row: a, rowFP: fp})
			case !run.now.Before(prev.fetchedAt().Add(refreshAfter)):
				refresh = append(refresh, detailWork{kid: ks, row: a, rowFP: fp, refresh: true, lastAt: prev.FetchedAt})
			}
		}
	}

	sort.SliceStable(refresh, func(i, j int) bool { return refresh[i].lastAt < refresh[j].lastAt })
	return append(fresh, refresh...)
}

// execute works through the plan within the per-poll cap. It returns early
// when the session is refused or a request fails in a way that is not about
// one assignment (network, server, rate limit): continuing would only
// repeat the failure against the school's site.
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
		run.handle(ctx, w, f)
	}

	if deferred > 0 {
		slog.Info("schoology assignment detail cap reached; remainder deferred to the next poll",
			"max_per_poll", deps.Config.maxPerPoll(), "deferred", deferred)
	}
}

// pace waits the configured spacing before every request but the first.
func (run *detailRun) pace(ctx context.Context) error {
	if !run.requested {
		run.requested = true
		return ctx.Err()
	}
	return run.deps.Sleep(ctx, run.deps.Config.requestSpacing())
}

// fetch reads one assignment's page and, for an assessment, its comments
// page. stop=true means the surface must not make another request this
// poll; the result is then nil.
func (run *detailRun) fetch(ctx context.Context, kid Kid, id int64) (f *detailFetch, stop bool) {
	deps := run.deps
	if err := run.pace(ctx); err != nil {
		return nil, true
	}
	detail, parseErrs, err := getAssignmentTraced(ctx, deps.Client, deps.Tel, id)
	if err != nil {
		class, perItem := classifyDetailErr(err)
		if perItem {
			return &detailFetch{err: err}, false
		}
		run.hardFailure(kid, id, "GetAssignment", class, err)
		return nil, true
	}
	if detail == nil {
		run.hardFailure(kid, id, "GetAssignment", "unknown", fmt.Errorf("library returned no detail and no error"))
		return nil, true
	}
	f = &detailFetch{detail: detail, parseErrs: parseErrs, comments: detail.Comments}

	// A regular assignment carries its comment block inline. Only an
	// assessment has to be asked separately; for a regular assignment the
	// comments page answers 403, so asking would be a wasted request.
	if detail.Comments.State != schoologylib.CommentsStateUnknown || detail.Kind != schoologylib.AssignmentKindAssessment {
		return f, false
	}
	if err := run.pace(ctx); err != nil {
		return nil, true
	}
	comments, err := getAssignmentCommentsTraced(ctx, deps.Client, deps.Tel, id)
	switch {
	case err == nil && comments != nil:
		// State unavailable is a normal answer: nothing to report.
		f.comments = *comments
	case err == nil:
		// No block and no error: leave the state unknown.
	default:
		class, perItem := classifyDetailErr(err)
		if !perItem {
			run.hardFailure(kid, id, "GetAssignmentComments", class, err)
			return nil, true
		}
		// Something was served but it is not a comments page we
		// understand. Keep the detail; the state stays unknown.
		f.commentsErr = err
	}
	return f, false
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
// Everything else -- an expired session, the network, a rate limit, a 5xx
// -- ends the surface for this poll.
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
func (run *detailRun) hardFailure(kid Kid, id int64, op, class string, err error) {
	deps := run.deps
	run.errsLogged = true
	deps.Tel.RecordParseFailure(detailParser, class, kid.Name, detailSurface)
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeError)
	// The (parser, class) observation is what pollNow reads to detect an
	// expired session.
	deps.Dedup.Observe(detailParser, class, strconv.FormatInt(id, 10))
	run.addReceipt(kid, class, op, id, err.Error())
	slog.Warn("schoology parse failure",
		"parser", detailParser,
		"error_class", class,
		"kid", kid.Name,
		"assignment_id", id,
		"op", op,
	)
}

// handle decides what one fetched assignment means for one kid: reject it,
// leave it (unchanged), or stage it.
func (run *detailRun) handle(ctx context.Context, w detailWork, f *detailFetch) {
	deps := run.deps
	ks := w.kid
	kid := ks.kid
	id := w.row.ID

	if f.err != nil {
		// This assignment only. Remember the read so it is retried at
		// the refresh interval, not every poll.
		class, _ := classifyDetailErr(f.err)
		run.reject(w, class, "GetAssignment", f.err.Error())
		return
	}
	d := f.detail

	// The library refuses a page for a different id; check again here
	// because everything downstream trusts it.
	if d.ID != 0 && d.ID != id {
		run.reject(w, detailClassIDMismatch, "GetAssignment",
			fmt.Sprintf("assignment %d: the page returned is for assignment %d; not staged", id, d.ID))
		return
	}
	if !sameCourse(w.row.CourseTitle, d.CourseTitle) {
		run.reject(w, detailClassCourseMismatch, "GetAssignment", fmt.Sprintf(
			"assignment %d: the detail page is filed under course %q but %s's list shows it under %q; "+
				"not staged, because the list is the only thing that ties an assignment to a child",
			id, d.CourseTitle, kid.Name, w.row.CourseTitle))
		return
	}

	for _, pe := range f.parseErrs {
		if pe == nil {
			continue
		}
		run.errsLogged = true
		deps.Tel.RecordParseFailure(detailParser, detailClassRowParse, kid.Name, detailSurface)
		deps.Dedup.Observe(detailParser, detailClassRowParse, strconv.FormatInt(id, 10))
		run.addReceipt(kid, detailClassRowParse, pe.Op, id, pe.Error())
	}
	if f.commentsErr != nil {
		run.errsLogged = true
		deps.Tel.RecordParseFailure(detailParser, detailClassCommentsParse, kid.Name, detailSurface)
		deps.Dedup.Observe(detailParser, detailClassCommentsParse, strconv.FormatInt(id, 10))
		run.addReceipt(kid, detailClassCommentsParse, "GetAssignmentComments", id, f.commentsErr.Error())
	}

	opts := buildDetailItemOptions(w.row, d, f.comments, ks.rule)
	body := buildDetailBody(w.row, d, f.comments, deps.Location)
	hash := detailContentHash(opts, body)

	if ks.seen[id] == hash {
		deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeUnchanged)
		run.rememberRow(w)
		return
	}

	if msg := firstParseError(f.parseErrs); msg != "" {
		opts = BuildDegradedOptions(opts, msg, "", strconv.FormatInt(id, 10), detailSurface)
	}
	if err := stageAssignmentItem(ctx, deps.Tel, deps.Writer, opts, []byte(body), id); err != nil {
		// Not remembered: the next poll reads and tries again.
		deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeError)
		slog.Warn("schoology assignment detail stage failed",
			"kid", kid.Name, "assignment_id", id, "error", err)
		return
	}
	run.staged++
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeStaged)

	ks.seen[id] = hash
	if err := saveDetailSeen(deps.Checkpoint, kid.Name, ks.seen, ks.listed); err != nil {
		slog.Error("schoology assignment detail checkpoint save failed",
			"kid", kid.Name, "assignment_id", id, "duplicate_risk", true, "error", err)
	}
	run.rememberRow(w)
}

// reject records a per-assignment refusal: a receipt, no staged item, and
// the read remembered so the assignment is next tried at the refresh
// interval (or sooner if its list row changes). It does not feed the
// schema-drift counter.
func (run *detailRun) reject(w detailWork, class, trace, msg string) {
	kid := w.kid.kid
	deps := run.deps
	deps.Tel.RecordParseFailure(detailParser, class, kid.Name, detailSurface)
	deps.Tel.RecordAssignmentDetailFetch(kid.Name, detailOutcomeRejected)
	deps.Dedup.Observe(detailParser, class, strconv.FormatInt(w.row.ID, 10))
	run.addReceipt(kid, class, trace, w.row.ID, msg)
	slog.Warn("schoology assignment detail not staged",
		"kid", kid.Name, "assignment_id", w.row.ID, "error_class", class)
	run.rememberRow(w)
}

// rememberRow records that the row was read now.
func (run *detailRun) rememberRow(w detailWork) {
	ks := w.kid
	ks.rows[w.row.ID] = detailRow{Fingerprint: w.rowFP, FetchedAt: run.now.Unix()}
	if err := saveDetailRows(run.deps.Checkpoint, ks.kid.Name, ks.rows); err != nil {
		slog.Error("schoology assignment detail row state save failed",
			"kid", ks.kid.Name, "assignment_id", w.row.ID, "error", err)
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

// normalizeCourse folds a course title for comparison: case, runs of
// whitespace, and the spacing around a colon.
func normalizeCourse(s string) string {
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
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
// 1"). So one title may be the other followed by a ":" suffix. Anything
// else is a mismatch. An empty title on either side cannot be compared and
// is not a mismatch.
func sameCourse(listTitle, detailTitle string) bool {
	a, b := normalizeCourse(listTitle), normalizeCourse(detailTitle)
	if a == "" || b == "" || a == b {
		return true
	}
	return strings.HasPrefix(b, a+":") || strings.HasPrefix(a, b+":")
}

// detailCourse picks the course title to stage: the detail page's (it names
// the section) or, when the page shows none, the list row's.
func detailCourse(row *schoologylib.Assignment, d *schoologylib.AssignmentDetail) string {
	if c := strings.TrimSpace(d.CourseTitle); c != "" {
		return c
	}
	return strings.TrimSpace(row.CourseTitle)
}

// detailTitle picks the title to stage, with the list row as a fallback for
// a page whose heading could not be read.
func detailTitle(row *schoologylib.Assignment, d *schoologylib.AssignmentDetail) string {
	if t := strings.TrimSpace(d.Title); t != "" {
		return t
	}
	if t := strings.TrimSpace(row.Title); t != "" {
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

// buildDetailItemOptions composes the staged item's metadata. Optional
// fields the page does not show are left out rather than staged empty.
func buildDetailItemOptions(
	row *schoologylib.Assignment,
	d *schoologylib.AssignmentDetail,
	comments schoologylib.AssignmentComments,
	rule connector.MatchResult,
) connector.ItemOptions {
	course := detailCourse(row, d)
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
		"assignment_kind": kind,
		"comments_state":  commentsStateTag(comments.State),
		"has_description": strconv.FormatBool(d.DescriptionText != ""),
	}
	setTag := func(k, v string) {
		if v != "" {
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

	return connector.ItemOptions{
		Source:           "schoology",
		Sender:           sender,
		Subject:          detailTitle(row, d),
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
	if at.IsZero() {
		return raw
	}
	return raw + " (" + at.In(loc).Format(time.RFC3339) + ")"
}

// buildDetailBody projects the detail into the plain text that is staged as
// content, in the style of buildAssignmentBody. The description is the
// library's plain-text rendering; the raw HTML is never staged.
func buildDetailBody(
	row *schoologylib.Assignment,
	d *schoologylib.AssignmentDetail,
	comments schoologylib.AssignmentComments,
	loc *time.Location,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Assignment: %s\n", detailTitle(row, d))
	if d.Kind != schoologylib.AssignmentKindUnknown {
		fmt.Fprintf(&b, "Kind: %s\n", d.Kind)
	}
	if c := detailCourse(row, d); c != "" {
		fmt.Fprintf(&b, "Course: %s\n", c)
	}
	if d.Folder != "" {
		fmt.Fprintf(&b, "Folder: %s\n", d.Folder)
	}
	if d.Category != "" {
		fmt.Fprintf(&b, "Category: %s\n", d.Category)
	}
	if d.GradingPeriod != "" {
		fmt.Fprintf(&b, "Period: %s\n", d.GradingPeriod)
	}
	if d.DueRaw != "" {
		fmt.Fprintf(&b, "Due: %s\n", renderedTime(d.DueRaw, d.DueAt, loc))
	} else {
		b.WriteString("Due: no due date listed\n")
	}
	if d.PostedRaw != "" {
		fmt.Fprintf(&b, "Posted: %s\n", renderedTime(d.PostedRaw, d.PostedAt, loc))
	}
	if d.AssessmentStatus != "" {
		fmt.Fprintf(&b, "Status: %s\n", d.AssessmentStatus)
	}
	if row.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", row.URL)
	}

	if d.DescriptionText != "" {
		b.WriteString("\nDescription:\n")
		b.WriteString(strings.TrimRight(d.DescriptionText, "\n"))
		b.WriteString("\n")
		if d.DescriptionTruncated {
			b.WriteString("[The description was cut off at the size limit.]\n")
		}
	}

	if comments.State == schoologylib.CommentsStatePopulated {
		b.WriteString("\nTeacher comments:\n")
		if comments.LayoutUnmapped {
			b.WriteString("[The comment layout is unverified: author, time and text may run together below.]\n")
		}
		for _, c := range comments.Comments {
			if c == nil {
				continue
			}
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", text)
			if c.Truncated {
				b.WriteString("  [This comment was cut off at the size limit.]\n")
			}
		}
	}
	return b.String()
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

// firstParseError returns the first non-nil parse error's text.
func firstParseError(errs schoologylib.ParseErrors) string {
	for _, pe := range errs {
		if pe != nil {
			return pe.Error()
		}
	}
	return ""
}
