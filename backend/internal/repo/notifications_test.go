// Notification store tests (plan step 2.6, second half of the 2.6 split):
// one shared postgres:18 testcontainer (newTestEnv) with subtests
// covering the create/get round trip (trimmed fields, ref jsonb),
// validation (kind/title/body caps, ref JSON + size), list order and
// pagination with the Unread filter, MarkRead idempotency (first read
// wins), CountUnread transitions, and per-user isolation.
package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNotificationStore(t *testing.T) {
	env := newTestEnv(t, "ntf-a", "ntf-b")
	store := NewNotificationStore(env.pool)
	if store == nil {
		t.Fatal("non-nil pool must give a store")
	}
	if NewNotificationStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "ntf-a")
	b := env.user(t, "ntf-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("create + get round trip", func(t *testing.T) {
		in := NotificationIn{
			Kind: "  review  ", Title: " Review ", Body: "  3 items  ",
			Ref: json.RawMessage(`{"kind":"transaction","id":"` + zeroUUID + `","n":3}`),
		}
		created, err := store.Create(ctx, aid, in)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Fields come back trimmed; ReadAt is nil while unread.
		if created.Kind != "review" || created.Title != "Review" || created.Body != "3 items" {
			t.Fatalf("trimmed: %+v", created)
		}
		if created.ReadAt != nil {
			t.Fatalf("new row must be unread: %+v", created)
		}
		if created.CreatedAt.IsZero() {
			t.Fatalf("created_at must be set: %+v", created)
		}
		// jsonb normalizes the document — compare parsed, not bytes.
		if !jsonDocsEqual(created.Ref, in.Ref) {
			t.Fatalf("ref: want %s, got %s", in.Ref, created.Ref)
		}

		got, err := store.Get(ctx, aid, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != created.ID || got.Kind != created.Kind || got.Title != created.Title ||
			got.Body != created.Body || got.ReadAt != nil ||
			!got.CreatedAt.Equal(created.CreatedAt) {
			t.Fatalf("get: want %+v, got %+v", created, got)
		}
		if !jsonDocsEqual(got.Ref, in.Ref) {
			t.Fatalf("get ref: want %s, got %s", in.Ref, got.Ref)
		}

		// A row created without a ref stays NULL: Ref is nil.
		plain, err := store.Create(ctx, aid, NotificationIn{Kind: "info", Title: "T", Body: "B"})
		if err != nil {
			t.Fatalf("create without ref: %v", err)
		}
		if plain.Ref != nil {
			t.Fatalf("refless row must have nil Ref, got %s", plain.Ref)
		}
		gotPlain, err := store.Get(ctx, aid, plain.ID)
		if err != nil {
			t.Fatalf("get refless: %v", err)
		}
		if gotPlain.Ref != nil {
			t.Fatalf("refless row must stay nil after get, got %s", gotPlain.Ref)
		}

		if err := store.Delete(ctx, aid, created.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		if err := store.Delete(ctx, aid, plain.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		base := func() NotificationIn {
			return NotificationIn{Kind: "k", Title: "t", Body: "b"}
		}
		bad := []struct {
			name string
			mut  func(*NotificationIn)
		}{
			{"blank kind", func(in *NotificationIn) { in.Kind = "   " }},
			{"blank title", func(in *NotificationIn) { in.Title = "   " }},
			{"blank body", func(in *NotificationIn) { in.Body = "   " }},
			{"kind too long", func(in *NotificationIn) { in.Kind = strings.Repeat("k", 65) }},
			{"title too long", func(in *NotificationIn) { in.Title = strings.Repeat("t", 201) }},
			{"body too long", func(in *NotificationIn) { in.Body = strings.Repeat("b", 4001) }},
			{"ref not JSON", func(in *NotificationIn) { in.Ref = json.RawMessage(`{`) }},
			{"ref too large", func(in *NotificationIn) {
				// 11-byte envelope + 64 KiB of pad = 65547 bytes > 64 KiB.
				in.Ref = json.RawMessage(`{"pad":"` + strings.Repeat("x", 64*1024) + `"}`)
			}},
		}
		for _, tc := range bad {
			t.Run(tc.name, func(t *testing.T) {
				in := base()
				tc.mut(&in)
				if _, err := store.Create(ctx, aid, in); err == nil {
					t.Fatal("want rejection, got nil")
				}
			})
		}
		// The boundary values that DO fit are accepted (then cleaned up):
		// kind 64, title 200, body 4000, ref exactly 64 KiB.
		boundary := base()
		boundary.Kind = strings.Repeat("k", 64)
		boundary.Title = strings.Repeat("t", 200)
		boundary.Body = strings.Repeat("b", 4000)
		boundary.Ref = json.RawMessage(`{"pad":"` + strings.Repeat("x", 64*1024-11) + `"}`)
		ok, err := store.Create(ctx, aid, boundary)
		if err != nil {
			t.Fatalf("boundary row must be accepted: %v", err)
		}
		if !jsonDocsEqual(ok.Ref, boundary.Ref) {
			t.Fatalf("boundary ref: got %d bytes, want 65536", len(ok.Ref))
		}
		if err := store.Delete(ctx, aid, ok.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("list order, pagination, unread filter", func(t *testing.T) {
		// Rows may share created_at, so the total order is
		// (created_at DESC, id DESC) — assert it is non-increasing, not
		// a specific id sequence.
		ids := make([]string, 0, 5)
		for i := 0; i < 5; i++ {
			n, err := store.Create(ctx, aid, NotificationIn{
				Kind: "page", Title: fmt.Sprintf("Row %d", i), Body: "body",
			})
			if err != nil {
				t.Fatalf("create %d: %v", i, err)
			}
			ids = append(ids, n.ID)
		}
		for _, id := range ids[:3] {
			if _, err := store.MarkRead(ctx, aid, id); err != nil {
				t.Fatalf("mark read %s: %v", id, err)
			}
		}

		all, err := store.List(ctx, aid, NotificationFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(all) != 5 {
			t.Fatalf("list: got %d rows, want 5", len(all))
		}
		for i := 1; i < len(all); i++ {
			prev, cur := all[i-1], all[i]
			if cur.CreatedAt.After(prev.CreatedAt) ||
				(cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID > prev.ID) {
				t.Fatalf("order violated: %s after %s", cur.ID, prev.ID)
			}
		}

		// Pagination: limit 2 walks the same total order.
		page1, err := store.List(ctx, aid, NotificationFilter{Limit: 2})
		if err != nil {
			t.Fatalf("list page1: %v", err)
		}
		page2, err := store.List(ctx, aid, NotificationFilter{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("list page2: %v", err)
		}
		page3, err := store.List(ctx, aid, NotificationFilter{Limit: 2, Offset: 4})
		if err != nil {
			t.Fatalf("list page3: %v", err)
		}
		if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 {
			t.Fatalf("pages: %d + %d + %d rows, want 2 + 2 + 1", len(page1), len(page2), len(page3))
		}
		if page1[0].ID != all[0].ID || page1[1].ID != all[1].ID {
			t.Fatalf("page1 must be the first two rows: %+v vs %+v", page1, all[:2])
		}
		if page2[0].ID != all[2].ID || page3[0].ID != all[4].ID {
			t.Fatalf("pages must not overlap or skip: %+v %+v", page2, page3)
		}

		// The limit cap (100) is invisible at this size: a huge limit
		// still returns all rows.
		capped, err := store.List(ctx, aid, NotificationFilter{Limit: 10000})
		if err != nil {
			t.Fatalf("list capped: %v", err)
		}
		if len(capped) != 5 {
			t.Fatalf("capped list: got %d rows, want 5", len(capped))
		}

		// The Unread filter leaves exactly the two unread rows.
		unread, err := store.List(ctx, aid, NotificationFilter{Unread: true})
		if err != nil {
			t.Fatalf("list unread: %v", err)
		}
		if len(unread) != 2 {
			t.Fatalf("unread: got %d rows, want 2", len(unread))
		}
		for _, n := range unread {
			if n.ReadAt != nil {
				t.Fatalf("unread list leaked a read row: %+v", n)
			}
		}

		for _, id := range ids {
			if err := store.Delete(ctx, aid, id); err != nil {
				t.Fatalf("cleanup delete %s: %v", id, err)
			}
		}
	})

	t.Run("mark read is idempotent, first read wins", func(t *testing.T) {
		n, err := store.Create(ctx, aid, NotificationIn{Kind: "k", Title: "t", Body: "b"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		first, err := store.MarkRead(ctx, aid, n.ID)
		if err != nil {
			t.Fatalf("first mark: %v", err)
		}
		if first.ReadAt == nil {
			t.Fatalf("first mark must set read_at: %+v", first)
		}
		// A second mark must not move read_at (COALESCE keeps the
		// original). The sleep guarantees now() would differ if it
		// were used.
		time.Sleep(11 * time.Millisecond)
		second, err := store.MarkRead(ctx, aid, n.ID)
		if err != nil {
			t.Fatalf("second mark (idempotent): %v", err)
		}
		if second.ReadAt == nil || !second.ReadAt.Equal(*first.ReadAt) {
			t.Fatalf("first read must win: %v -> %v", first.ReadAt, second.ReadAt)
		}
		if _, err := store.MarkRead(ctx, aid, zeroUUID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("unknown id: got %v, want ErrNotificationNotFound", err)
		}
		if err := store.Delete(ctx, aid, n.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("count unread transitions", func(t *testing.T) {
		before, err := store.CountUnread(ctx, aid)
		if err != nil {
			t.Fatalf("count before: %v", err)
		}
		made, err := store.Create(ctx, aid, NotificationIn{Kind: "k", Title: "t", Body: "b"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		n1, err := store.CountUnread(ctx, aid)
		if err != nil {
			t.Fatalf("count after create: %v", err)
		}
		if n1 != before+1 {
			t.Fatalf("count after create: want %d, got %d", before+1, n1)
		}
		if _, err := store.MarkRead(ctx, aid, made.ID); err != nil {
			t.Fatalf("mark read: %v", err)
		}
		n2, err := store.CountUnread(ctx, aid)
		if err != nil {
			t.Fatalf("count after mark: %v", err)
		}
		if n2 != before {
			t.Fatalf("count after mark: want %d, got %d", before, n2)
		}
		if err := store.Delete(ctx, aid, made.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	})

	t.Run("per-user scoping", func(t *testing.T) {
		bBefore, err := store.CountUnread(ctx, bid)
		if err != nil {
			t.Fatalf("count b before: %v", err)
		}
		mine, err := store.Create(ctx, aid, NotificationIn{Kind: "scope", Title: "Mine", Body: "b"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// User b must not read, mark, or delete a's notification — and
		// the row must survive, still unread.
		if _, err := store.Get(ctx, bid, mine.ID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("b gets a's row: got %v, want ErrNotificationNotFound", err)
		}
		if _, err := store.MarkRead(ctx, bid, mine.ID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("b marks a's row: got %v, want ErrNotificationNotFound", err)
		}
		if err := store.Delete(ctx, bid, mine.ID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("b deletes a's row: got %v, want ErrNotificationNotFound", err)
		}
		got, err := store.Get(ctx, aid, mine.ID)
		if err != nil {
			t.Fatalf("row must be intact: %v", err)
		}
		if got.ReadAt != nil {
			t.Fatalf("row must still be unread: %+v", got)
		}
		lb, err := store.List(ctx, bid, NotificationFilter{})
		if err != nil {
			t.Fatalf("list b: %v", err)
		}
		for _, x := range lb {
			if x.ID == mine.ID {
				t.Fatal("a's notification leaked into b's list")
			}
		}
		bAfter, err := store.CountUnread(ctx, bid)
		if err != nil {
			t.Fatalf("count b after: %v", err)
		}
		if bAfter != bBefore {
			t.Fatalf("a's row must not touch b's unread count: %d -> %d", bBefore, bAfter)
		}
		if err := store.Delete(ctx, aid, mine.ID); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		if _, err := store.Get(ctx, aid, mine.ID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("deleted get: got %v, want ErrNotificationNotFound", err)
		}
	})

	t.Run("delete unknown id", func(t *testing.T) {
		if err := store.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrNotificationNotFound) {
			t.Fatalf("want ErrNotificationNotFound, got %v", err)
		}
	})
}
