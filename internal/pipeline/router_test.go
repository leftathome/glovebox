package pipeline

import (
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

func orderedResponse(dirName string) ScanResponse {
	return ScanResponse{
		Item: staging.StagingItem{
			DirPath: "/staging/" + dirName,
			Metadata: staging.ItemMetadata{
				DestinationAgent: "messaging",
				Ordered:          true,
			},
		},
		Duration: time.Millisecond,
	}
}

func unorderedResponse(dirName string) ScanResponse {
	return ScanResponse{
		Item: staging.StagingItem{
			DirPath: "/staging/" + dirName,
			Metadata: staging.ItemMetadata{
				DestinationAgent: "messaging",
				Ordered:          false,
			},
		},
		Duration: time.Millisecond,
	}
}

func TestRouter_UnorderedDeliveredImmediately(t *testing.T) {
	var delivered []string
	router := NewRouter(func(resp ScanResponse) error {
		delivered = append(delivered, resp.Item.DirPath)
		return nil
	})

	router.Route(unorderedResponse("item-b"))
	router.Route(unorderedResponse("item-a"))

	if len(delivered) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(delivered))
	}
	if delivered[0] != "/staging/item-b" {
		t.Errorf("first = %q, want /staging/item-b", delivered[0])
	}
	if delivered[1] != "/staging/item-a" {
		t.Errorf("second = %q, want /staging/item-a", delivered[1])
	}
}

func TestRouter_OrderedDeliveredInFIFO(t *testing.T) {
	var delivered []string
	router := NewRouter(func(resp ScanResponse) error {
		delivered = append(delivered, resp.Item.DirPath)
		return nil
	})

	// Submit out of order -- not delivered until Flush
	router.Route(orderedResponse("20260328-0003-ccc"))
	router.Route(orderedResponse("20260328-0001-aaa"))
	router.Route(orderedResponse("20260328-0002-bbb"))

	if len(delivered) != 0 {
		t.Fatalf("ordered items should not be delivered before Flush, got %d", len(delivered))
	}

	router.Flush()

	if len(delivered) != 3 {
		t.Fatalf("expected 3 deliveries after Flush, got %d", len(delivered))
	}
	if delivered[0] != "/staging/20260328-0001-aaa" {
		t.Errorf("first = %q, want 20260328-0001-aaa", delivered[0])
	}
	if delivered[1] != "/staging/20260328-0002-bbb" {
		t.Errorf("second = %q, want 20260328-0002-bbb", delivered[1])
	}
	if delivered[2] != "/staging/20260328-0003-ccc" {
		t.Errorf("third = %q, want 20260328-0003-ccc", delivered[2])
	}
}

func TestRouter_MixedOrderedAndUnordered(t *testing.T) {
	var delivered []string
	router := NewRouter(func(resp ScanResponse) error {
		delivered = append(delivered, resp.Item.DirPath)
		return nil
	})

	router.Route(unorderedResponse("unordered-1"))
	router.Route(orderedResponse("20260328-0002-bbb"))
	router.Route(unorderedResponse("unordered-2"))
	router.Route(orderedResponse("20260328-0001-aaa"))

	// Only unordered should be delivered so far
	if len(delivered) != 2 {
		t.Fatalf("expected 2 unordered deliveries before Flush, got %d", len(delivered))
	}
	if delivered[0] != "/staging/unordered-1" {
		t.Errorf("first = %q, want unordered-1", delivered[0])
	}
	if delivered[1] != "/staging/unordered-2" {
		t.Errorf("second = %q, want unordered-2", delivered[1])
	}

	router.Flush()

	if len(delivered) != 4 {
		t.Fatalf("expected 4 total after Flush, got %d", len(delivered))
	}
	// Ordered items should be in FIFO order
	if delivered[2] != "/staging/20260328-0001-aaa" {
		t.Errorf("third = %q, want 20260328-0001-aaa", delivered[2])
	}
	if delivered[3] != "/staging/20260328-0002-bbb" {
		t.Errorf("fourth = %q, want 20260328-0002-bbb", delivered[3])
	}
}

func TestRouter_FlushEmpty(t *testing.T) {
	router := NewRouter(func(resp ScanResponse) error {
		return nil
	})
	if err := router.Flush(); err != nil {
		t.Fatalf("flush empty router: %v", err)
	}
}
