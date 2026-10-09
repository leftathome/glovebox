package schoology

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// Reproduces the first live poll: the feed is newest-first, so the first post
// has the highest id. A single high-water mark staged that one and rejected
// the rest (1 post of 10 per child on a real account). Every listed item must
// be staged exactly once, whatever order it arrives in, and a second poll of
// the same listing must stage nothing.
func TestPoll_StagesEveryItemRegardlessOfListingOrder(t *testing.T) {
	// Newest first: ids descend, as Schoology returns them.
	var posts []*schoologylib.Post
	for id := 2010; id >= 2001; id-- {
		posts = append(posts, integrationFeedPost(fmt.Sprintf("%d", id), "body", "Teacher", "Course",
			time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)))
	}
	// Overdue work by due date: ids in no particular order.
	var assignments []*schoologylib.Assignment
	for _, id := range []int64{1005, 1001, 1008, 1003, 1007, 1002, 1006, 1004} {
		assignments = append(assignments, integrationSampleAssignment(id, fmt.Sprintf("Task %d", id), "Course"))
	}

	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return assignments, nil, nil
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return posts, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}
	c := newWiredConnector(t, client)
	cp := newTestCheckpoint(t)

	count := func(surface string) int {
		t.Helper()
		seen, err := seenIDs(cp, surface, "k1")
		if err != nil {
			t.Fatalf("seenIDs(%s): %v", surface, err)
		}
		return len(seen)
	}

	if err := c.pollNow(context.Background(), cp, "scheduled", 0); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if got := count("feed"); got != 10 {
		t.Errorf("feed posts staged = %d, want 10 (newest-first listing)", got)
	}
	if got := count("assignment"); got != 8 {
		t.Errorf("assignments staged = %d, want 8 (unordered listing)", got)
	}

	// Same listing again: nothing new.
	if err := c.pollNow(context.Background(), cp, "scheduled", 0); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if got := count("feed"); got != 10 {
		t.Errorf("after an identical poll, feed seen-set = %d, want 10", got)
	}

	// An OLDER assignment that only now becomes overdue must still be staged.
	assignments = append(assignments, integrationSampleAssignment(900, "Old task now overdue", "Course"))
	if err := c.pollNow(context.Background(), cp, "scheduled", 0); err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if got := count("assignment"); got != 9 {
		t.Errorf("assignments staged after an older id appeared = %d, want 9", got)
	}
	if d, _ := ShouldStage(cp, "assignment", "k1", 900); d != StageSkipDuplicate {
		t.Errorf("the late-appearing older assignment was not recorded as staged: decision=%v", d)
	}
}

func TestSeenSet_BoundedAndKeepsNewest(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= maxSeenIDs+50; id++ {
		if err := SaveLastSeenID(cp, "feed", "k1", id); err != nil {
			t.Fatal(err)
		}
	}
	seen, err := seenIDs(cp, "feed", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != maxSeenIDs {
		t.Fatalf("seen-set size = %d, want cap %d", len(seen), maxSeenIDs)
	}
	if _, ok := seen[int64(maxSeenIDs+50)]; !ok {
		t.Error("newest id was evicted")
	}
	if _, ok := seen[1]; ok {
		t.Error("oldest id was kept past the cap")
	}
	if last, _ := LastSeenID(cp, "feed", "k1"); last != int64(maxSeenIDs+50) {
		t.Errorf("legacy last_id = %d, want %d", last, maxSeenIDs+50)
	}
}
