package postgres

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

func TestPostgresStoreWithRealDriver(t *testing.T) {
	databaseURL := testDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open isolated test database: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("ping isolated test database: %v", err)
	}

	suffix, err := ownerToken()
	if err != nil {
		t.Fatalf("generate temporary schema name: %v", err)
	}
	schema := "yanineko_test_" + suffix
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
	})

	scopedURL, err := withSearchPath(databaseURL, schema)
	if err != nil {
		t.Fatalf("scope database URL: %v", err)
	}
	store, err := Open(ctx, scopedURL)
	if err != nil {
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	t.Cleanup(store.Close)
	secondStore, err := Open(ctx, scopedURL)
	if err != nil {
		t.Fatalf("reopen store with existing migration: %v", err)
	}
	secondStore.Close()

	first := sampleIncoming(100, 1, domain.MessageText, "first")
	claim := beginClaimed(t, ctx, store, first)
	if history, err := store.History(ctx, claim, 0); err != nil || len(history) != 1 || history[0].Text != first.Text {
		t.Fatalf("initial archive = (%#v, %v), want user input before generation", history, err)
	}
	duplicate, err := store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginBusy {
		t.Fatalf("duplicate begin while claimed = (%v, %v), want busy", duplicate.Status, err)
	}
	if err := store.Release(ctx, claim); err != nil {
		t.Fatalf("release failed-generation claim: %v", err)
	}
	retry, err := store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || retry.Status != app.BeginClaimed {
		t.Fatalf("retry failed-generation input = (%v, %v), want reclaimed", retry.Status, err)
	}
	claim = retry.Claim
	if history, err := store.History(ctx, claim, 0); err != nil || len(history) != 1 || history[0].Text != first.Text {
		t.Fatalf("archive after failed-generation retry = (%#v, %v), want one retained user input", history, err)
	}
	if err := store.CommitResponse(ctx, claim, "first reply", postgresModelContext()); err != nil {
		t.Fatalf("commit response and history pair: %v", err)
	}
	history, err := store.History(ctx, app.Claim{
		Incoming: first, OwnerToken: claim.OwnerToken, Version: claim.Version,
	}, 0)
	if err == nil || !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("history after lease release expected ErrLeaseLost, got %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("history returned with stale claim: %#v", history)
	}
	duplicate, err = store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginReady {
		t.Fatalf("duplicate after generation = (%v, %v), want ready", duplicate.Status, err)
	}
	delivery, err := store.ClaimDelivery(ctx, first.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || delivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim delivery = (%v, %v), want claimed", delivery.Status, err)
	}
	if delivery.Delivery.Outgoing.Text != "first reply" || delivery.Delivery.Outgoing.ReplyToMessageID != first.MessageID {
		t.Fatalf("delivery payload = %#v", delivery.Delivery.Outgoing)
	}
	if err := store.MarkSent(ctx, delivery.Delivery, 700); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	duplicate, err = store.Begin(ctx, first, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginSent {
		t.Fatalf("duplicate after send = (%v, %v), want sent", duplicate.Status, err)
	}
	conflicting := first
	conflicting.Text = "changed payload"
	if _, err := store.Begin(ctx, conflicting, app.ConversationLeaseDuration); !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("same update with changed input error = %v, want ErrUpdateConflict", err)
	}
	older := sampleIncoming(99, 1, domain.MessageText, "late delivery")
	olderResult, err := store.Begin(ctx, older, app.ConversationLeaseDuration)
	if err != nil || olderResult.Status != app.BeginClaimed {
		t.Fatalf("unseen lower update identifier = (%v, %v), want claimed", olderResult.Status, err)
	}
	if err := store.Release(ctx, olderResult.Claim); err != nil {
		t.Fatalf("release unseen lower update identifier: %v", err)
	}

	second := sampleIncoming(101, 1, domain.MessageText, "restored context")
	secondClaim := beginClaimed(t, ctx, store, second)
	modelContext, err := store.ModelContext(ctx, secondClaim)
	if err != nil {
		t.Fatalf("load model context: %v", err)
	}
	history, err = store.History(ctx, secondClaim, modelContext.ThroughMessageOrder)
	if err != nil || len(history) != 2 || history[0].Text != "late delivery" || history[1].Text != second.Text {
		t.Fatalf("incremental archive = (%#v, %v), want both late and current inputs", history, err)
	}
	fullArchive, err := store.History(ctx, secondClaim, 0)
	if err != nil || len(fullArchive) != 4 || fullArchive[0].Text != first.Text || fullArchive[1].Text != "first reply" || fullArchive[len(fullArchive)-1].Text != second.Text {
		t.Fatalf("full archive = (%#v, %v), want all accepted turns in sequence", fullArchive, err)
	}
	if err := store.Release(ctx, secondClaim); err != nil {
		t.Fatalf("release restored-context claim: %v", err)
	}

	second = sampleIncoming(102, 1, domain.MessageAbout, "/about")
	aboutClaim := beginClaimed(t, ctx, store, second)
	if err := store.CommitAbout(ctx, aboutClaim, app.AboutMessage); err != nil {
		t.Fatalf("commit about response: %v", err)
	}
	aboutDelivery, err := store.ClaimDelivery(ctx, second.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || aboutDelivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim about delivery = (%v, %v), want claimed", aboutDelivery.Status, err)
	}
	if err := store.MarkDeliveryUnknown(ctx, aboutDelivery.Delivery); err != nil {
		t.Fatalf("mark unknown delivery: %v", err)
	}
	duplicate, err = store.Begin(ctx, second, app.ConversationLeaseDuration)
	if err != nil || duplicate.Status != app.BeginUnknown {
		t.Fatalf("duplicate unknown update = (%v, %v), want unknown", duplicate.Status, err)
	}
	unknownDelivery, err := store.ClaimDelivery(ctx, second.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || unknownDelivery.Status != app.DeliveryUnknownState {
		t.Fatalf("unknown delivery retry = (%v, %v), want no resend", unknownDelivery.Status, err)
	}

	staleInput := sampleIncoming(103, 1, domain.MessageText, "lease fencing")
	staleClaim := beginClaimed(t, ctx, store, staleInput)
	if _, err := store.pool.Exec(ctx, `UPDATE conversations SET lease_until = clock_timestamp() - interval '1 second' WHERE chat_id = $1 AND thread_id = $2`, staleInput.Key.ChatID, staleInput.Key.ThreadID); err != nil {
		t.Fatalf("expire test lease: %v", err)
	}
	newClaim := beginClaimed(t, ctx, store, staleInput)
	if newClaim.Version <= staleClaim.Version || newClaim.OwnerToken == staleClaim.OwnerToken {
		t.Fatalf("reclaimed fencing token = (%d, %q), old = (%d, %q)", newClaim.Version, newClaim.OwnerToken, staleClaim.Version, staleClaim.OwnerToken)
	}
	if err := store.CommitResponse(ctx, staleClaim, "stale response", postgresModelContext()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale response commit error = %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, newClaim); err != nil {
		t.Fatalf("release reclaimed update: %v", err)
	}
	if retryClaim := beginClaimed(t, ctx, store, staleInput); retryClaim.Version <= newClaim.Version {
		t.Fatalf("released update version = %d, want > %d", retryClaim.Version, newClaim.Version)
	} else if err := store.Release(ctx, retryClaim); err != nil {
		t.Fatalf("release retried fencing claim: %v", err)
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
	if _, err := store.pool.Exec(ctx, `UPDATE processed_updates SET delivery_lease_until = clock_timestamp() - interval '1 second' WHERE update_id = $1`, expired.UpdateID); err != nil {
		t.Fatalf("expire delivery lease: %v", err)
	}
	var conversationLeaseActive bool
	if err := store.pool.QueryRow(ctx, `SELECT lease_until > clock_timestamp() FROM conversations WHERE chat_id = $1 AND thread_id = $2`, expired.Key.ChatID, expired.Key.ThreadID).Scan(&conversationLeaseActive); err != nil || !conversationLeaseActive {
		t.Fatalf("processing lease active before delivery recovery = (%v, %v), want true", conversationLeaseActive, err)
	}

	afterExpired := sampleIncoming(105, 1, domain.MessageText, "after uncertain delivery")
	afterExpiredResult, err := store.Begin(ctx, afterExpired, app.ConversationLeaseDuration)
	if err != nil || afterExpiredResult.Status != app.BeginClaimed {
		t.Fatalf("update after expired delivery = (%v, %v), want claimed", afterExpiredResult.Status, err)
	}
	var deliveryState string
	var deliveryOwnerNull, deliveryLeaseNull bool
	if err := store.pool.QueryRow(ctx, `SELECT state, delivery_owner IS NULL, delivery_lease_until IS NULL FROM processed_updates WHERE update_id = $1`, expired.UpdateID).Scan(&deliveryState, &deliveryOwnerNull, &deliveryLeaseNull); err != nil {
		t.Fatalf("read expired delivery state: %v", err)
	}
	if deliveryState != "delivery_unknown" || !deliveryOwnerNull || !deliveryLeaseNull {
		t.Fatalf("expired delivery fields = (%q, owner null %v, lease null %v), want unknown with no lease owner", deliveryState, deliveryOwnerNull, deliveryLeaseNull)
	}
	if err := store.MarkSent(ctx, expiredDelivery.Delivery, 704); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late completion of expired delivery = %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, afterExpiredResult.Claim); err != nil {
		t.Fatalf("release update after expired delivery: %v", err)
	}

	testConcurrentConversations(t, ctx, store)
	testConcurrentUpdatesInOneConversation(t, ctx, store)
	testStorageHistoryRetentionAndIsolation(t, ctx, store)

	var pairCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE chat_id = $1 AND thread_id = $2`, first.Key.ChatID, first.Key.ThreadID).Scan(&pairCount); err != nil {
		t.Fatalf("count saved context messages: %v", err)
	}
	if pairCount != 2 {
		t.Fatalf("saved context message count = %d, want one user/assistant pair; /about must not enter history", pairCount)
	}
}

func testConcurrentConversations(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	inputs := []domain.Incoming{
		sampleIncoming(200, 2, domain.MessageText, "chat two"),
		sampleIncoming(201, 3, domain.MessageText, "chat three"),
	}
	start := make(chan struct{})
	results := make(chan app.BeginResult, len(inputs))
	errorsFound := make(chan error, len(inputs))
	for _, input := range inputs {
		input := input
		go func() {
			<-start
			result, err := store.Begin(ctx, input, app.ConversationLeaseDuration)
			results <- result
			errorsFound <- err
		}()
	}
	close(start)
	for range inputs {
		result, err := <-results, <-errorsFound
		if err != nil || result.Status != app.BeginClaimed {
			t.Fatalf("independent conversation claim = (%v, %v), want claimed", result.Status, err)
		}
	}
}

func testConcurrentUpdatesInOneConversation(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	inputs := []domain.Incoming{
		sampleIncoming(300, 4, domain.MessageText, "same chat first"),
		sampleIncoming(301, 4, domain.MessageText, "same chat second"),
	}
	start := make(chan struct{})
	results := make(chan app.BeginResult, len(inputs))
	errorsFound := make(chan error, len(inputs))
	for _, input := range inputs {
		input := input
		go func() {
			<-start
			result, err := store.Begin(ctx, input, app.ConversationLeaseDuration)
			results <- result
			errorsFound <- err
		}()
	}
	close(start)
	statuses := make([]app.BeginStatus, 0, len(inputs))
	for range inputs {
		result, err := <-results, <-errorsFound
		if err != nil {
			t.Fatalf("same conversation begin: %v", err)
		}
		statuses = append(statuses, result.Status)
	}
	claimed, busy := 0, 0
	for _, status := range statuses {
		switch status {
		case app.BeginClaimed:
			claimed++
		case app.BeginBusy:
			busy++
		}
	}
	if claimed != 1 || busy != 1 {
		t.Fatalf("same conversation statuses = %v, want one claimed and one busy", statuses)
	}
}

func testStorageHistoryRetentionAndIsolation(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	for turn := range 11 {
		turnNumber := strconv.Itoa(turn)
		incoming := sampleIncoming(400+int64(turn), 5, domain.MessageText, "user-"+turnNumber)
		commitTurn(t, ctx, store, incoming, "assistant-"+turnNumber)
	}

	latest := sampleIncoming(411, 5, domain.MessageText, "next")
	claim := beginClaimed(t, ctx, store, latest)
	history, err := store.History(ctx, claim, 0)
	if err != nil || len(history) != 23 || history[0].Text != "user-0" || history[len(history)-1].Text != "next" {
		t.Fatalf("conversation archive = (%#v, %v), want all eleven replies plus current input", history, err)
	}
	if err := store.Release(ctx, claim); err != nil {
		t.Fatalf("release context-limit claim: %v", err)
	}

	other := sampleIncoming(412, 6, domain.MessageText, "other chat")
	otherClaim := beginClaimed(t, ctx, store, other)
	otherHistory, err := store.History(ctx, otherClaim, 0)
	if err != nil || len(otherHistory) != 1 || otherHistory[0].Text != "other chat" {
		t.Fatalf("other chat archive = (%#v, %v), want only its own input", otherHistory, err)
	}
	if err := store.Release(ctx, otherClaim); err != nil {
		t.Fatalf("release other-chat claim: %v", err)
	}
}

func commitTurn(t *testing.T, ctx context.Context, store *Store, incoming domain.Incoming, response string) {
	t.Helper()
	claim := beginClaimed(t, ctx, store, incoming)
	if err := store.CommitResponse(ctx, claim, response, postgresModelContext()); err != nil {
		t.Fatalf("commit turn %d: %v", incoming.UpdateID, err)
	}
	delivery, err := store.ClaimDelivery(ctx, incoming.UpdateID, app.DeliveryLeaseDuration)
	if err != nil || delivery.Status != app.DeliveryClaimed {
		t.Fatalf("claim turn %d delivery = (%v, %v), want claimed", incoming.UpdateID, delivery.Status, err)
	}
	if err := store.MarkSent(ctx, delivery.Delivery, incoming.MessageID+1000); err != nil {
		t.Fatalf("mark turn %d sent: %v", incoming.UpdateID, err)
	}
}

func postgresModelContext() domain.ModelContext {
	return domain.ModelContext{ItemsJSON: []byte("[]"), Initialized: true}
}

func beginClaimed(t *testing.T, ctx context.Context, store *Store, incoming domain.Incoming) app.Claim {
	t.Helper()
	result, err := store.Begin(ctx, incoming, app.ConversationLeaseDuration)
	if err != nil || result.Status != app.BeginClaimed {
		t.Fatalf("begin update %d = (%v, %v), want claimed", incoming.UpdateID, result.Status, err)
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

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv("YANINEKO_TEST_DATABASE_URL"))
	if value == "" {
		t.Skip("YANINEKO_TEST_DATABASE_URL not set; real PostgreSQL driver test was not requested by this invocation")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.User == nil {
		t.Fatal("YANINEKO_TEST_DATABASE_URL must be a PostgreSQL URL for a local test database")
	}
	address := net.ParseIP(parsed.Hostname())
	if address == nil || !address.IsLoopback() || !strings.HasPrefix(strings.TrimPrefix(path.Clean(parsed.Path), "/"), "yanineko_test") {
		t.Fatal("real-driver test is restricted to loopback hosts and databases prefixed yanineko_test")
	}
	return value
}

func withSearchPath(databaseURL, schema string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
