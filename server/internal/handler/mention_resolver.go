package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MentionResolution is the result of resolving a single mention://agent/ or
// mention://member/ link against the workspace roster.
//
// Status is a discriminated union: "resolved", "unresolved", or "archived".
// Phase 2 (SLE-141) will add "self-healed" — the type is intentionally open
// so adding the new variant is non-breaking.
type MentionResolution struct {
	// Status: "resolved" | "unresolved" | "archived"
	Status     string
	MentionURL string // e.g. "mention://agent/8c3e…"
	Reason     string // human-readable reason for non-resolved outcomes
}

// resolveMention is the single source of truth for whether a parsed mention
// target can be dispatched in the given workspace.
//
//   - "resolved"   — UUID matches exactly one live, non-archived agent/member.
//   - "archived"   — UUID matches an agent that exists but is archived.
//   - "unresolved" — UUID matches no agent or member in this workspace.
//
// mention://issue/… and mention://all/… are side-effect-free; they always
// return "resolved" so the caller can safely pass any mention type here.
// validateMentions (validate-on-post) calls this function so that validation
// and CreateComment/UpdateComment share a single resolution path.
func (h *Handler) resolveMention(ctx context.Context, m util.Mention, workspaceID pgtype.UUID) MentionResolution {
	mentionURL := fmt.Sprintf("mention://%s/%s", m.Type, m.ID)
	switch m.Type {
	case "agent":
		agentUUID, err := util.ParseUUID(m.ID)
		if err != nil {
			return MentionResolution{
				Status:     "unresolved",
				MentionURL: mentionURL,
				Reason:     mentionURL + ": invalid UUID",
			}
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID:          agentUUID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return MentionResolution{
				Status:     "unresolved",
				MentionURL: mentionURL,
				Reason:     mentionURL + " resolves to no live agent",
			}
		}
		if agent.ArchivedAt.Valid {
			return MentionResolution{
				Status:     "archived",
				MentionURL: mentionURL,
				Reason:     mentionURL + " resolves to an archived agent",
			}
		}
		return MentionResolution{Status: "resolved", MentionURL: mentionURL}

	case "member":
		memberUUID, err := util.ParseUUID(m.ID)
		if err != nil {
			return MentionResolution{
				Status:     "unresolved",
				MentionURL: mentionURL,
				Reason:     mentionURL + ": invalid UUID",
			}
		}
		_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID:      memberUUID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return MentionResolution{
				Status:     "unresolved",
				MentionURL: mentionURL,
				Reason:     mentionURL + " resolves to no workspace member",
			}
		}
		return MentionResolution{Status: "resolved", MentionURL: mentionURL}

	default:
		// squad/issue/all — side-effect-free; always resolved.
		return MentionResolution{Status: "resolved", MentionURL: mentionURL}
	}
}

// validateMentions parses all agent and member mentions in content and
// resolves each against the workspace roster. Returns a structured error
// naming every failing mention if any agent/member mention is unresolved or
// archived. mention://issue/… and mention://all/… links are ignored.
//
// This is the fail-loud gate wired into CreateComment: any agent mention that
// cannot be dispatched causes the comment create to be rejected with a clear
// error rather than silently dropping the dispatch.
func (h *Handler) validateMentions(ctx context.Context, content string, workspaceID pgtype.UUID) error {
	mentions := util.ParseMentions(content)
	var failures []string
	for _, m := range mentions {
		if m.Type != "agent" && m.Type != "member" {
			continue
		}
		res := h.resolveMention(ctx, m, workspaceID)
		if res.Status != "resolved" {
			failures = append(failures, res.Reason)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("comment contains unresolvable mention(s): %s", strings.Join(failures, "; "))
}
