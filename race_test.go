package assistant

import (
	"context"
	"testing"

	flowstore "github.com/redelay/go-flowdsl/flowexec/store"
)

// lateStore hides the flow from the first GetFlow, as if another process
// created it between this process's read and its create.
type lateStore struct {
	flowstore.Store
	misses int
}

func (s *lateStore) GetFlow(ctx context.Context, id string) (*flowstore.Flow, error) {
	if s.misses > 0 {
		s.misses--
		return nil, flowstore.ErrNotFound
	}
	return s.Store.GetFlow(ctx, id)
}

// Two binaries (api + admin-api) booting against one empty database both
// read "no flow" and both create it; the loser got ErrAlreadyExists and its
// Startup failed, so the process exited on first boot.
func TestStartupAdoptsAFlowAnotherProcessCreatedFirst(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	ctx := context.Background()

	// The "other process" created the flow and has not published yet.
	real := asst.flowexec.Store()
	if _, err := real.CreateFlow(ctx, flowstore.CreateFlowInput{ID: defaultFlowID, Name: "Project Assistant"}); err != nil {
		t.Fatal(err)
	}
	asst.storeForTest = &lateStore{Store: real, misses: 1}

	if err := asst.Startup(ctx); err != nil {
		t.Fatalf("Startup failed on a concurrently created flow: %v", err)
	}
	f, err := real.GetFlow(ctx, defaultFlowID)
	if err != nil {
		t.Fatal(err)
	}
	if f.PublishedVersionID == "" {
		t.Fatal("the adopted flow was left with nothing published")
	}
}

// A flow that exists with nothing published (its creator died before
// seeding it) is seeded on the next boot instead of staying dead.
func TestStartupSeedsAnExistingUnpublishedFlow(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	ctx := context.Background()
	if _, err := asst.flowexec.Store().CreateFlow(ctx, flowstore.CreateFlowInput{ID: defaultFlowID, Name: "Project Assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := asst.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := asst.flowexec.Store().GetFlow(ctx, defaultFlowID)
	if f == nil || f.PublishedVersionID == "" {
		t.Fatal("an existing unpublished flow was not seeded")
	}
}

// The reseed path has the same race: a concurrent create is not an error.
func TestReseedAdoptsAFlowAnotherProcessCreatedFirst(t *testing.T) {
	asst, teardown := newFixture(t, "unused")
	defer teardown()
	ctx := context.Background()
	real := asst.flowexec.Store()
	if _, err := real.CreateFlow(ctx, flowstore.CreateFlowInput{ID: defaultFlowID, Name: "Project Assistant"}); err != nil {
		t.Fatal(err)
	}
	asst.storeForTest = &lateStore{Store: real, misses: 1}
	if err := asst.ReseedFromTemplate(ctx, "echo"); err != nil {
		t.Fatalf("reseed failed on a concurrently created flow: %v", err)
	}
}
