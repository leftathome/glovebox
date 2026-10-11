package schoology

import (
	"context"
	"errors"
	"io"
	"testing"

	schoologylib "github.com/leftathome/schoology-go"
)

// fakeClient is a hand-rolled SchoologyClient for connector tests. Each
// field can be set per-test to control what the fake returns; nil
// defaults to a "not configured" error.
type fakeClient struct {
	OverdueSubmissionsFunc func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error)
	FeedFunc               func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error)
	InboxFunc              func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error)
	DownloadFunc           func(ctx context.Context, url string) (io.ReadCloser, error)
	AssignmentFunc         func(ctx context.Context, assignmentID int64) (*schoologylib.AssignmentDetail, schoologylib.ParseErrors, error)
	CommentsFunc           func(ctx context.Context, assignmentID int64) (*schoologylib.AssignmentComments, error)
}

func (f *fakeClient) GetAssignment(ctx context.Context, assignmentID int64) (*schoologylib.AssignmentDetail, schoologylib.ParseErrors, error) {
	if f.AssignmentFunc != nil {
		return f.AssignmentFunc(ctx, assignmentID)
	}
	return nil, nil, errors.New("fakeClient.GetAssignment not configured")
}

func (f *fakeClient) GetAssignmentComments(ctx context.Context, assignmentID int64) (*schoologylib.AssignmentComments, error) {
	if f.CommentsFunc != nil {
		return f.CommentsFunc(ctx, assignmentID)
	}
	return nil, errors.New("fakeClient.GetAssignmentComments not configured")
}

func (f *fakeClient) GetOverdueSubmissions(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
	if f.OverdueSubmissionsFunc != nil {
		return f.OverdueSubmissionsFunc(ctx, childUID)
	}
	return nil, nil, errors.New("fakeClient.GetOverdueSubmissions not configured")
}

func (f *fakeClient) GetFeed(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
	if f.FeedFunc != nil {
		return f.FeedFunc(ctx, childUID)
	}
	return nil, nil, errors.New("fakeClient.GetFeed not configured")
}

func (f *fakeClient) GetInbox(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
	if f.InboxFunc != nil {
		return f.InboxFunc(ctx)
	}
	return nil, nil, errors.New("fakeClient.GetInbox not configured")
}

func (f *fakeClient) DownloadAttachment(ctx context.Context, url string) (io.ReadCloser, error) {
	if f.DownloadFunc != nil {
		return f.DownloadFunc(ctx, url)
	}
	return nil, errors.New("fakeClient.DownloadAttachment not configured")
}

// Compile-time check: fake implements the interface.
var _ SchoologyClient = (*fakeClient)(nil)

// TestSchoologyClient_InterfaceImplementations is a sanity check that
// production wrappers and fakes both compile against the interface.
// No behavior is exercised here -- this just guards against drift.
func TestSchoologyClient_InterfaceImplementations(t *testing.T) {
	var _ SchoologyClient = (*productionClient)(nil)
	var _ SchoologyClient = (*fakeClient)(nil)
}
