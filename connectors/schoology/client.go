// Package schoology is the Glovebox connector for the Schoology LMS.
//
// All library access flows through the SchoologyClient interface; the
// production code wraps schoology-go's *schoology.Client, and tests pass
// a hand-rolled fake. This keeps connector-level tests offline and
// allows the library to evolve without churning connector tests.
//
// TODO: candidate for extraction to connector primitive base type
// (the interface-boundary pattern is shared with every future
// LMS-shaped connector).
package schoology

import (
	"context"
	"io"

	schoologylib "github.com/leftathome/schoology-go"
)

// SchoologyClient is the connector's narrowed view of the schoology-go
// library surface. Methods correspond 1:1 to library functions used by
// the connector; new library surfaces require widening this interface.
//
// Notes on the real library signatures:
//   - GetOverdueSubmissions and GetFeed and GetInbox also return ParseErrors
//     (a []*Error slice for partial HTML-parse failures); the interface
//     mirrors those signatures exactly.
//   - DownloadAttachment takes a URL string (not a numeric id) and returns
//     the raw body without a content-type header value; callers inspect the
//     body or use Attachment.MimeType from the owning resource.
type SchoologyClient interface {
	GetChildren(ctx context.Context) ([]*schoologylib.Child, error)
	GetCoursesForChild(ctx context.Context, childUID int64) ([]*schoologylib.Course, error)
	GetOverdueSubmissions(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error)
	GetFeed(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error)
	GetInbox(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error)
	DownloadAttachment(ctx context.Context, url string) (io.ReadCloser, error)
}

// productionClient is the real SchoologyClient backed by schoology-go.
type productionClient struct {
	lib *schoologylib.Client
}

// NewProductionClient wraps a schoology-go client so it satisfies SchoologyClient.
func NewProductionClient(lib *schoologylib.Client) SchoologyClient {
	return &productionClient{lib: lib}
}

func (p *productionClient) GetChildren(ctx context.Context) ([]*schoologylib.Child, error) {
	return p.lib.GetChildren(ctx)
}

func (p *productionClient) GetCoursesForChild(ctx context.Context, childUID int64) ([]*schoologylib.Course, error) {
	return p.lib.GetCoursesForChild(ctx, childUID)
}

func (p *productionClient) GetOverdueSubmissions(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
	return p.lib.GetOverdueSubmissions(ctx, childUID)
}

func (p *productionClient) GetFeed(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
	return p.lib.GetFeed(ctx, childUID)
}

func (p *productionClient) GetInbox(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
	return p.lib.GetInbox(ctx)
}

func (p *productionClient) DownloadAttachment(ctx context.Context, url string) (io.ReadCloser, error) {
	return p.lib.DownloadAttachment(ctx, url)
}
