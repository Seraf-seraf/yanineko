package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "turso.tech/database/tursogo"

	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestSQLiteStoreWithRealDriver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db, err := sql.Open("turso", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory SQLite-compatible database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Fatalf("ping in-memory SQLite-compatible database: %v", err)
	}
	store := newStore(db)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("run SQLite migrations: %v", err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("re-run existing SQLite migrations: %v", err)
	}

	mediaInput := sampleIncoming(90, 2, domain.MessageText, "Что на фото?")
	mediaClaim := beginClaimed(t, ctx, store, mediaInput)
	mediaTranscript := "Что на фото?\n\n[Описание изображения: чёрная кошка]"
	if err := store.UpdateUserMessage(ctx, mediaClaim, mediaTranscript); err != nil {
		t.Fatalf("replace archived media input with text description: %v", err)
	}
	mediaHistory, err := store.History(ctx, mediaClaim, 0)
	if err != nil || len(mediaHistory) != 1 || mediaHistory[0].Text != mediaTranscript {
		t.Fatalf("archived media transcript = (%#v, %v), want text description", mediaHistory, err)
	}
	if err := store.Release(ctx, mediaClaim); err != nil {
		t.Fatalf("release media-description test claim: %v", err)
	}

	first := sampleIncoming(100, 1, domain.MessageText, "first")
	claim := beginClaimed(t, ctx, store, first)
	if history, err := store.History(ctx, claim, -1); err != nil || len(history) != 1 || history[0].Text != first.Text || history[0].Role != domain.RoleUser {
		t.Fatalf("initial archive = (%#v, %v), want input archived before generation", history, err)
	}
	duplicate, err := store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginBusy {
		t.Fatalf("duplicate while processing = (%v, %v), want busy", duplicate.Status, err)
	}
	if err := store.Release(ctx, claim); err != nil {
		t.Fatalf("release failed-generation claim: %v", err)
	}
	retry, err := store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || retry.Status != app.BeginClaimed {
		t.Fatalf("retry failed-generation input = (%v, %v), want reclaimed", retry.Status, err)
	}
	claim = retry.Claim
	if history, err := store.History(ctx, claim, -1); err != nil || len(history) != 1 || history[0].Text != first.Text {
		t.Fatalf("archive after failed-generation retry = (%#v, %v), want one retained user input", history, err)
	}
	if err := store.CommitResponse(ctx, claim, "first reply", testModelContext()); err != nil {
		t.Fatalf("atomically commit response and history: %v", err)
	}
	var throughMessageOrder int64
	var itemsJSON string
	if err := store.db.QueryRowContext(ctx, `SELECT through_message_order, items_json FROM model_contexts WHERE chat_id = ? AND thread_id = ?`, first.Key.ChatID, first.Key.ThreadID).Scan(&throughMessageOrder, &itemsJSON); err != nil || throughMessageOrder != 2 || itemsJSON != "[]" {
		t.Fatalf("persisted model context = (%d, %q, %v), want opaque checkpoint and archive watermark", throughMessageOrder, itemsJSON, err)
	}
	duplicate, err = store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginReady {
		t.Fatalf("duplicate after generation = (%v, %v), want ready", duplicate.Status, err)
	}
	delivery, err := store.ClaimDelivery(ctx, first.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || delivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim first delivery = (%v, %v), want claimed", delivery.Status, err)
	}
	if err := store.MarkSent(ctx, delivery.Delivery, 700); err != nil {
		t.Fatalf("mark first delivery sent: %v", err)
	}
	duplicate, err = store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginSent {
		t.Fatalf("duplicate after delivery = (%v, %v), want sent", duplicate.Status, err)
	}
	conflicting := first
	conflicting.Text = "conflicting duplicate"
	if _, err := store.Begin(ctx, conflicting, app.ConversationLeaseDuration); !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("duplicate update with different content error = %v, want ErrUpdateConflict", err)
	}

	second := sampleIncoming(101, 1, domain.MessageText, "restored")
	secondClaim := beginClaimed(t, ctx, store, second)
	modelContext, err := store.ModelContext(ctx, secondClaim)
	if err != nil {
		t.Fatalf("load model context: %v", err)
	}
	history, err := store.History(ctx, secondClaim, modelContext.ThroughMessageOrder)
	if err != nil || len(history) != 1 || history[0].Text != second.Text || history[0].Role != domain.RoleUser {
		t.Fatalf("incremental history = (%#v, %v), want only the newly archived input", history, err)
	}
	archive, err := store.History(ctx, secondClaim, -1)
	if err != nil || len(archive) != 3 || archive[0].Text != first.Text || archive[1].Text != "first reply" || archive[2].Text != second.Text {
		t.Fatalf("full archive = (%#v, %v), want all messages without trimming", archive, err)
	}
	if err := store.Release(ctx, secondClaim); err != nil {
		t.Fatalf("release pre-generation claim: %v", err)
	}
	secondClaim = beginClaimed(t, ctx, store, second)
	if err := store.Release(ctx, secondClaim); err != nil {
		t.Fatalf("release retry before generation: %v", err)
	}

	about := sampleIncoming(102, 1, domain.MessageAbout, "/about")
	aboutClaim := beginClaimed(t, ctx, store, about)
	if err := store.CommitAbout(ctx, aboutClaim, app.AboutMessage); err != nil {
		t.Fatalf("commit static about response: %v", err)
	}
	aboutDelivery, err := store.ClaimDelivery(ctx, about.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || aboutDelivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim about delivery = (%v, %v), want claimed", aboutDelivery.Status, err)
	}
	if err := store.MarkDeliveryUnknown(ctx, aboutDelivery.Delivery); err != nil {
		t.Fatalf("persist unknown about delivery: %v", err)
	}
	duplicate, err = store.Begin(ctx, about, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginUnknown {
		t.Fatalf("unknown duplicate = (%v, %v), want unknown", duplicate.Status, err)
	}
	unknown, err := store.ClaimDelivery(ctx, about.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || unknown.Status != app.DeliveryUnknownState {
		t.Fatalf("unknown delivery retry = (%v, %v), want no resend", unknown.Status, err)
	}

	fenced := sampleIncoming(103, 1, domain.MessageText, "fencing")
	oldClaim := beginClaimed(t, ctx, store, fenced)
	if _, err := store.db.ExecContext(ctx, `UPDATE conversations SET lease_until = julianday('now') - 1 WHERE chat_id = ? AND thread_id = ?`, fenced.Key.ChatID, fenced.Key.ThreadID); err != nil {
		t.Fatalf("expire SQLite lease: %v", err)
	}
	newClaim := beginClaimed(t, ctx, store, fenced)
	if newClaim.Version <= oldClaim.Version || newClaim.OwnerToken == oldClaim.OwnerToken {
		t.Fatalf("reclaimed fencing token = (%d, %q), old = (%d, %q)", newClaim.Version, newClaim.OwnerToken, oldClaim.Version, oldClaim.OwnerToken)
	}
	if err := store.CommitResponse(ctx, oldClaim, "stale response", testModelContext()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale response commit error = %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, newClaim); err != nil {
		t.Fatalf("release fenced claim: %v", err)
	}

	expired := sampleIncoming(104, 1, domain.MessageAbout, "/about")
	expiredClaim := beginClaimed(t, ctx, store, expired)
	if err := store.CommitAbout(ctx, expiredClaim, app.AboutMessage); err != nil {
		t.Fatalf("commit response before expired delivery: %v", err)
	}
	expiredDelivery, err := store.ClaimDelivery(ctx, expired.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || expiredDelivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim expiring delivery = (%v, %v), want claimed", expiredDelivery.Status, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE processed_updates SET delivery_lease_until = julianday('now') - 1 WHERE update_id = ?`, expired.UpdateID); err != nil {
		t.Fatalf("expire delivery lease: %v", err)
	}
	var conversationLeaseActive bool
	if err := store.db.QueryRowContext(ctx, `SELECT lease_until > julianday('now') FROM conversations WHERE chat_id = ? AND thread_id = ?`, expired.Key.ChatID, expired.Key.ThreadID).Scan(&conversationLeaseActive); err != nil || !conversationLeaseActive {
		t.Fatalf("processing lease active before delivery recovery = (%v, %v), want true", conversationLeaseActive, err)
	}

	afterExpired := sampleIncoming(105, 1, domain.MessageText, "after uncertain delivery")
	afterExpiredResult, err := store.Begin(ctx, afterExpired, app.ConversationLeaseDuration)
	if err != nil || afterExpiredResult.Status != app.BeginClaimed {
		t.Fatalf("update after expired delivery = (%v, %v), want claimed", afterExpiredResult.Status, err)
	}
	var deliveryState string
	var deliveryOwner sql.NullString
	var deliveryLease sql.NullFloat64
	if err := store.db.QueryRowContext(ctx, `SELECT state, delivery_owner, delivery_lease_until FROM processed_updates WHERE update_id = ?`, expired.UpdateID).Scan(&deliveryState, &deliveryOwner, &deliveryLease); err != nil {
		t.Fatalf("read expired delivery state: %v", err)
	}
	if deliveryState != "delivery_unknown" || deliveryOwner.Valid || deliveryLease.Valid {
		t.Fatalf("expired delivery fields = (%q, %v, %v), want unknown with no lease owner", deliveryState, deliveryOwner, deliveryLease)
	}
	if err := store.MarkSent(ctx, expiredDelivery.Delivery, 704); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late completion of expired delivery = %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, afterExpiredResult.Claim); err != nil {
		t.Fatalf("release update after expired delivery: %v", err)
	}

	older := sampleIncoming(99, 1, domain.MessageText, "lower update identifier")
	olderResult, err := store.Begin(ctx, older, app.ConversationLeaseDuration)
	if err != nil || olderResult.Status != app.BeginClaimed {
		t.Fatalf("unseen lower update identifier = (%v, %v), want claimed", olderResult.Status, err)
	}
	if err := store.Release(ctx, olderResult.Claim); err != nil {
		t.Fatalf("release unseen lower update identifier: %v", err)
	}

	testConcurrentSQLiteUpdates(t, ctx, store)
	testConcurrentSQLiteConversations(t, ctx, store)
	testSQLiteHistoryRetentionAndIsolation(t, ctx, store)

	var aboutHistoryCount int64
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages WHERE chat_id = ? AND thread_id = ? AND update_id = ?`, about.Key.ChatID, about.Key.ThreadID, about.UpdateID).Scan(&aboutHistoryCount); err != nil {
		t.Fatalf("count about context rows: %v", err)
	}
	if aboutHistoryCount != 0 {
		t.Fatalf("about history rows = %d, want zero", aboutHistoryCount)
	}
}

func testConcurrentSQLiteUpdates(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	inputs := []domain.Incoming{
		sampleIncoming(200, 2, domain.MessageText, "concurrent first"),
		sampleIncoming(201, 2, domain.MessageText, "concurrent second"),
	}
	start := make(chan struct{})
	results := make([]app.BeginResult, len(inputs))
	errorsFound := make([]error, len(inputs))
	var waitGroup sync.WaitGroup
	for index, input := range inputs {
		waitGroup.Add(1)
		go func(index int, input domain.Incoming) {
			defer waitGroup.Done()
			<-start
			results[index], errorsFound[index] = store.Begin(ctx, input, app.ConversationLeaseDuration)
		}(index, input)
	}
	close(start)
	waitGroup.Wait()
	statuses := make([]app.BeginStatus, len(results))
	claimedResult := app.BeginResult{}
	for index, result := range results {
		if err := errorsFound[index]; err != nil {
			t.Fatalf("concurrent SQLite begin: %v", err)
		}
		statuses[index] = result.Status
		if result.Status == app.BeginClaimed {
			claimedResult = result
		}
	}
	claimed, busy := 0, 0
	for _, status := range statuses {
		if status == app.BeginClaimed {
			claimed++
		}
		if status == app.BeginBusy {
			busy++
		}
	}
	if claimed != 1 || busy != 1 {
		t.Fatalf("same-chat SQLite statuses = %v, want one claimed and one busy", statuses)
	}
	if err := store.Release(ctx, claimedResult.Claim); err != nil {
		t.Fatalf("release same-chat SQLite claim: %v", err)
	}
}

func testConcurrentSQLiteConversations(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	inputs := []domain.Incoming{
		sampleIncoming(300, 3, domain.MessageText, "independent first"),
		sampleIncoming(301, 4, domain.MessageText, "independent second"),
	}
	start := make(chan struct{})
	results := make([]app.BeginResult, len(inputs))
	errorsFound := make([]error, len(inputs))
	var waitGroup sync.WaitGroup
	for index, input := range inputs {
		waitGroup.Add(1)
		go func(index int, input domain.Incoming) {
			defer waitGroup.Done()
			<-start
			results[index], errorsFound[index] = store.Begin(ctx, input, app.ConversationLeaseDuration)
		}(index, input)
	}
	close(start)
	waitGroup.Wait()
	for index, result := range results {
		if errorsFound[index] != nil || result.Status != app.BeginClaimed {
			t.Fatalf("independent SQLite conversation %d = (%v, %v), want claimed", inputs[index].Key.ChatID, result.Status, errorsFound[index])
		}
	}
	for _, result := range results {
		if err := store.Release(ctx, result.Claim); err != nil {
			t.Fatalf("release independent SQLite conversation: %v", err)
		}
	}
}

func testSQLiteHistoryRetentionAndIsolation(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	for turn := range 11 {
		turnNumber := strconv.Itoa(turn)
		incoming := sampleIncoming(400+int64(turn), 5, domain.MessageText, "user-"+turnNumber)
		commitTurn(t, ctx, store, incoming, "assistant-"+turnNumber)
	}
	latest := sampleIncoming(411, 5, domain.MessageText, "next")
	claim := beginClaimed(t, ctx, store, latest)
	history, err := store.History(ctx, claim, -1)
	if err != nil || len(history) != 23 || history[0].Text != "user-0" || history[len(history)-1].Text != "next" {
		t.Fatalf("SQLite archive = (%#v, %v), want all eleven replies plus current input", history, err)
	}
	if err := store.Release(ctx, claim); err != nil {
		t.Fatalf("release context-limit claim: %v", err)
	}
	other := sampleIncoming(412, 6, domain.MessageText, "other chat")
	otherClaim := beginClaimed(t, ctx, store, other)
	otherHistory, err := store.History(ctx, otherClaim, -1)
	if err != nil || len(otherHistory) != 1 || otherHistory[0].Text != "other chat" {
		t.Fatalf("SQLite other-chat archive = (%#v, %v), want only its own input", otherHistory, err)
	}
	if err := store.Release(ctx, otherClaim); err != nil {
		t.Fatalf("release other-chat claim: %v", err)
	}
}

func commitTurn(t *testing.T, ctx context.Context, store *Store, incoming domain.Incoming, response string) {
	t.Helper()
	claim := beginClaimed(t, ctx, store, incoming)
	if err := store.CommitResponse(ctx, claim, response, testModelContext()); err != nil {
		t.Fatalf("commit SQLite turn %d: %v", incoming.UpdateID, err)
	}
	delivery, err := store.ClaimDelivery(ctx, incoming.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || delivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim SQLite turn %d delivery = (%v, %v), want claimed", incoming.UpdateID, delivery.Status, err)
	}
	if err := store.MarkSent(ctx, delivery.Delivery, incoming.MessageID+1000); err != nil {
		t.Fatalf("mark SQLite turn %d sent: %v", incoming.UpdateID, err)
	}
}

func testModelContext() domain.ModelContext {
	return domain.ModelContext{ItemsJSON: []byte("[]"), Initialized: true}
}

func beginClaimed(t *testing.T, ctx context.Context, store *Store, incoming domain.Incoming) app.Claim {
	t.Helper()
	result, err := store.Begin(ctx, incoming, app.ConversationLeaseDuration)
	if err != nil || result.Status != app.BeginClaimed {
		t.Fatalf("begin SQLite update %d = (%v, %v), want claimed", incoming.UpdateID, result.Status, err)
	}
	return result.Claim
}

func sampleIncoming(updateID int64, chatID int64, kind domain.MessageKind, text string) domain.Incoming {
	return domain.Incoming{
		UpdateID:  updateID,
		Key:       domain.ConversationKey{ChatID: chatID},
		MessageID: updateID + 1,
		Text:      text,
		Kind:      kind,
	}
}
