package handler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

var mentionTestSeq uint64

// mentionResolverFixture creates a live agent and an archived agent in the
// handler test workspace, returning their IDs for use in resolver tests.
// Both agents are cleaned up via t.Cleanup.
type mentionResolverFixture struct {
	LiveAgentID     string
	ArchivedAgentID string
	WorkspaceUUID   pgtype.UUID
}

func newMentionResolverFixture(t *testing.T) mentionResolverFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	wsUUID := util.MustParseUUID(testWorkspaceID)

	liveID := createHandlerTestAgent(t, "MentionResolver Live Agent", []byte("{}"))

	// Create the archived agent, then immediately archive it.
	archivedID := createHandlerTestAgent(t, "MentionResolver Archived Agent", []byte("{}"))
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET archived_at = now(), archived_by = $1 WHERE id = $2`,
		testUserID, archivedID,
	); err != nil {
		t.Fatalf("archive test agent: %v", err)
	}

	return mentionResolverFixture{
		LiveAgentID:     liveID,
		ArchivedAgentID: archivedID,
		WorkspaceUUID:   wsUUID,
	}
}

// TestResolveMention_FabricatedUUID verifies that an agent mention whose UUID
// is not present in the workspace resolves to "unresolved". This is the
// primary silent-strand scenario: a model hallucinates a UUID and the
// platform must reject it rather than silently dropping the dispatch.
func TestResolveMention_FabricatedUUID(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	fabricated := util.Mention{Type: "agent", ID: "00000000-dead-beef-0000-000000000000"}
	res := testHandler.resolveMention(ctx, fabricated, fx.WorkspaceUUID)

	if res.Status != "unresolved" {
		t.Errorf("fabricated UUID: expected status %q, got %q (reason: %s)", "unresolved", res.Status, res.Reason)
	}
	if res.Reason == "" {
		t.Error("fabricated UUID: expected non-empty Reason")
	}
}

// TestResolveMention_ArchivedAgent verifies that a mention to an archived
// agent resolves to "archived" — never to "resolved". Archived agents must
// not route regardless of whether their UUID is valid.
func TestResolveMention_ArchivedAgent(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	m := util.Mention{Type: "agent", ID: fx.ArchivedAgentID}
	res := testHandler.resolveMention(ctx, m, fx.WorkspaceUUID)

	if res.Status != "archived" {
		t.Errorf("archived agent: expected status %q, got %q (reason: %s)", "archived", res.Status, res.Reason)
	}
}

// TestResolveMention_ValidAgent is the regression test: a mention to a live,
// non-archived agent must resolve to "resolved" — the happy path must not
// be affected by the validation layer.
func TestResolveMention_ValidAgent(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	m := util.Mention{Type: "agent", ID: fx.LiveAgentID}
	res := testHandler.resolveMention(ctx, m, fx.WorkspaceUUID)

	if res.Status != "resolved" {
		t.Errorf("live agent: expected status %q, got %q (reason: %s)", "resolved", res.Status, res.Reason)
	}
}

// TestValidateMentions_RejectsUnresolvable confirms that validateMentions
// returns a non-nil error when the comment body contains an unresolvable
// agent mention, and that the error names the offending mention URL.
func TestValidateMentions_RejectsUnresolvable(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	fabricatedID := "00000000-dead-beef-0000-000000000000"
	content := fmt.Sprintf("[@Ghost](mention://agent/%s) do something", fabricatedID)
	err := testHandler.validateMentions(ctx, content, fx.WorkspaceUUID)

	if err == nil {
		t.Fatal("validateMentions: expected error for unresolvable mention, got nil")
	}
	if errStr := err.Error(); len(errStr) == 0 {
		t.Error("validateMentions: error message must not be empty")
	}
}

// TestValidateMentions_RejectsArchived confirms that validateMentions
// returns a non-nil error when the comment body mentions an archived agent.
func TestValidateMentions_RejectsArchived(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	content := fmt.Sprintf("[@OldBot](mention://agent/%s) wake up", fx.ArchivedAgentID)
	err := testHandler.validateMentions(ctx, content, fx.WorkspaceUUID)

	if err == nil {
		t.Fatal("validateMentions: expected error for archived agent mention, got nil")
	}
}

// TestValidateMentions_AllowsLiveAgent is the regression test: a comment that
// mentions only live agents must pass validateMentions without error.
func TestValidateMentions_AllowsLiveAgent(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	content := fmt.Sprintf("[@LiveBot](mention://agent/%s) please review", fx.LiveAgentID)
	if err := testHandler.validateMentions(ctx, content, fx.WorkspaceUUID); err != nil {
		t.Errorf("validateMentions: unexpected error for live-agent mention: %v", err)
	}
}

// TestValidateMentions_IssueMentionUnaffected confirms that issue cross-reference
// mentions (mention://issue/…) are not blocked by validateMentions, even when
// the UUID is fabricated. Issue mentions carry no dispatch side effect.
func TestValidateMentions_IssueMentionUnaffected(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	content := "[SLE-1](mention://issue/00000000-dead-beef-0000-000000000000) related issue"
	if err := testHandler.validateMentions(ctx, content, fx.WorkspaceUUID); err != nil {
		t.Errorf("validateMentions: issue mention should not be blocked: %v", err)
	}
}

// --- Phase 2 (SLE-141): resolve-by-label self-heal tests ---

// TestResolveMention_SelfHeal_NameUUIDMismatch is the primary Phase 2 scenario:
// an agent mention with the correct link text but a stale/fabricated UUID must
// self-heal to the real agent and set Status == "self-healed". This is the
// exact failure mode that stranded SLE-62 for ~20h.
func TestResolveMention_SelfHeal_NameUUIDMismatch(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	// Load the name of the live agent so we can use it as the label with a
	// fabricated UUID — simulating a model that knew the right name but
	// hallucinated the UUID.
	var liveName string
	if err := testPool.QueryRow(ctx,
		`SELECT name FROM agent WHERE id = $1`, fx.LiveAgentID,
	).Scan(&liveName); err != nil {
		t.Fatalf("load live agent name: %v", err)
	}

	m := util.Mention{
		Type:  "agent",
		ID:    "00000000-dead-beef-0000-000000000000", // fabricated UUID
		Label: liveName,                               // correct name
	}
	res := testHandler.resolveMention(ctx, m, fx.WorkspaceUUID)

	if res.Status != "self-healed" {
		t.Errorf("name/UUID mismatch: expected status %q, got %q (reason: %s)", "self-healed", res.Status, res.Reason)
	}
	if res.SelfHealedAgentID != fx.LiveAgentID {
		t.Errorf("name/UUID mismatch: expected SelfHealedAgentID %q, got %q", fx.LiveAgentID, res.SelfHealedAgentID)
	}
	if res.Diagnostic == "" {
		t.Error("name/UUID mismatch: expected non-empty Diagnostic")
	}
}

// TestResolveMention_SelfHeal_ValidatePasses confirms that validateMentions
// does NOT reject a self-healed mention — the comment must be allowed through
// so dispatch can route it to the correct agent.
func TestResolveMention_SelfHeal_ValidatePasses(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	var liveName string
	if err := testPool.QueryRow(ctx,
		`SELECT name FROM agent WHERE id = $1`, fx.LiveAgentID,
	).Scan(&liveName); err != nil {
		t.Fatalf("load live agent name: %v", err)
	}

	// Content uses correct name but fabricated UUID — validateMentions must not reject it.
	content := fmt.Sprintf("[@%s](mention://agent/00000000-dead-beef-0000-000000000000) please help", liveName)
	if err := testHandler.validateMentions(ctx, content, fx.WorkspaceUUID); err != nil {
		t.Errorf("validateMentions: self-healed mention should not be rejected: %v", err)
	}
}

// TestResolveMention_AmbiguousLabel_FailLoud confirms that when the link text
// matches more than one live agent, self-heal does not fire — the mention falls
// through to fail-loud to prevent a silent mis-route.
func TestResolveMention_AmbiguousLabel_FailLoud(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	// Create two live agents with the same name. createHandlerTestAgent uses the
	// same underlying name, so we generate a unique shared name per test run.
	seq := atomic.AddUint64(&mentionTestSeq, 1)
	sharedName := fmt.Sprintf("MentionAmbiguousAgent%d", seq)
	createHandlerTestAgent(t, sharedName, []byte("{}"))
	createHandlerTestAgent(t, sharedName, []byte("{}"))

	wsUUID := util.MustParseUUID(testWorkspaceID)
	m := util.Mention{
		Type:  "agent",
		ID:    "00000000-dead-beef-0000-000000000001", // fabricated UUID
		Label: sharedName,
	}
	res := testHandler.resolveMention(ctx, m, wsUUID)

	if res.Status == "self-healed" {
		t.Error("ambiguous label: must not self-heal when label matches multiple live agents")
	}
	if res.Status != "unresolved" {
		t.Errorf("ambiguous label: expected status %q, got %q (reason: %s)", "unresolved", res.Status, res.Reason)
	}
}

// TestResolveMention_LabelMatchesArchivedOnly_FailLoud confirms that when the
// link text matches only archived agents (and the UUID is stale), self-heal
// does not fire. Archived agents must never route.
func TestResolveMention_LabelMatchesArchivedOnly_FailLoud(t *testing.T) {
	fx := newMentionResolverFixture(t)
	ctx := context.Background()

	// Load the name of the archived agent created by the fixture.
	var archivedName string
	if err := testPool.QueryRow(ctx,
		`SELECT name FROM agent WHERE id = $1`, fx.ArchivedAgentID,
	).Scan(&archivedName); err != nil {
		t.Fatalf("load archived agent name: %v", err)
	}

	// Fabricated UUID + name of the archived agent — must NOT self-heal.
	m := util.Mention{
		Type:  "agent",
		ID:    "00000000-dead-beef-0000-000000000002", // fabricated UUID
		Label: archivedName,
	}
	res := testHandler.resolveMention(ctx, m, fx.WorkspaceUUID)

	if res.Status == "self-healed" {
		t.Error("archived-only label: must not self-heal when label matches only archived agents")
	}
	if res.Status == "resolved" {
		t.Error("archived-only label: must not resolve when the matched agent is archived")
	}
	if res.Status != "unresolved" {
		t.Errorf("archived-only label: expected status %q, got %q (reason: %s)", "unresolved", res.Status, res.Reason)
	}
}
