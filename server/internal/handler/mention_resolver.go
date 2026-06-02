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
// Status is a discriminated union:
//   - "resolved"   — UUID matches exactly one live, non-archived agent/member.
//   - "self-healed" — UUID did not resolve but the link text matched exactly one
//     live, non-archived agent; routed there. SelfHealedAgentID holds the real UUID.
//   - "archived"   — UUID matches an agent that exists but is archived.
//   - "unresolved" — UUID matches no agent or member in this workspace.
type MentionResolution struct {
	// Status: "resolved" | "self-healed" | "archived" | "unresolved"
	Status     string
	MentionURL string // e.g. "mention://agent/8c3e…"
	Reason     string // human-readable reason for non-resolved outcomes

	// SelfHealedAgentID is the real agent UUID when Status == "self-healed".
	SelfHealedAgentID string
	// Diagnostic is the visible message emitted to the issue thread on self-heal.
	Diagnostic string
}

// resolveMention is the single source of truth for whether a parsed mention
// target can be dispatched in the given workspace.
//
//   - "resolved"    — UUID matches exactly one live, non-archived agent/member.
//   - "self-healed" — UUID unresolved but link text matches exactly one live agent.
//   - "archived"    — UUID matches an agent that exists but is archived.
//   - "unresolved"  — UUID matches no agent or member in this workspace.
//
// mention://issue/… and mention://all/… are side-effect-free; they always
// return "resolved" so the caller can safely pass any mention type here.
//
// Both validate-on-post (validateMentions → CreateComment/UpdateComment) and
// the dispatch path (enqueueMentionedAgentTasks) call this function so the
// two can never disagree on whether a target is resolvable.
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
			// UUID not found — attempt label-based self-heal before failing loud.
			return h.resolveByLabel(ctx, m, mentionURL, workspaceID)
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

// resolveByLabel attempts to self-heal an unresolved agent mention by matching
// the link text against live non-archived agent names in the workspace.
//
// Strict rules (from the design):
//   - Exactly one live match → "self-healed".
//   - Zero or more than one live match → fall through to fail-loud.
//   - Zero live matches but at least one archived match → fail-loud (archived agents never route).
//
// This is only called when a UUID lookup has already failed, so it never runs
// on the happy path (valid UUID → resolved directly).
func (h *Handler) resolveByLabel(ctx context.Context, m util.Mention, mentionURL string, workspaceID pgtype.UUID) MentionResolution {
	label := strings.TrimSpace(m.Label)
	if label == "" {
		return MentionResolution{
			Status:     "unresolved",
			MentionURL: mentionURL,
			Reason:     mentionURL + " resolves to no live agent",
		}
	}

	liveAgents, err := h.Queries.ListAgents(ctx, workspaceID)
	if err != nil {
		// Can't query roster — fail loud rather than guess.
		return MentionResolution{
			Status:     "unresolved",
			MentionURL: mentionURL,
			Reason:     mentionURL + " resolves to no live agent (roster unavailable)",
		}
	}

	var matches []db.Agent
	for _, a := range liveAgents {
		if strings.EqualFold(strings.TrimSpace(a.Name), label) {
			matches = append(matches, a)
		}
	}

	switch len(matches) {
	case 1:
		realID := uuidToString(matches[0].ID)
		return MentionResolution{
			Status:            "self-healed",
			MentionURL:        mentionURL,
			SelfHealedAgentID: realID,
			Diagnostic: fmt.Sprintf(
				"mention `%s` routed to @%s by name match; UUID `%s` was stale or wrong",
				mentionURL, matches[0].Name, m.ID,
			),
		}
	case 0:
		// No live match — check if any archived agents match by name so we can
		// give a precise "label matches only archived" error.
		allAgents, err := h.Queries.ListAllAgents(ctx, workspaceID)
		if err == nil {
			for _, a := range allAgents {
				if strings.EqualFold(strings.TrimSpace(a.Name), label) && a.ArchivedAt.Valid {
					return MentionResolution{
						Status:     "unresolved",
						MentionURL: mentionURL,
						Reason:     mentionURL + ": label \"" + label + "\" matches only archived agent(s); archived agents cannot route",
					}
				}
			}
		}
		return MentionResolution{
			Status:     "unresolved",
			MentionURL: mentionURL,
			Reason:     mentionURL + " resolves to no live agent",
		}
	default:
		// >1 live match — ambiguous; do not guess.
		return MentionResolution{
			Status:     "unresolved",
			MentionURL: mentionURL,
			Reason:     mentionURL + ": label \"" + label + "\" matches multiple live agents (ambiguous; provide the correct UUID)",
		}
	}
}

// validateMentions parses all agent and member mentions in content and
// resolves each against the workspace roster. Returns a structured error
// naming every failing mention if any agent/member mention is unresolved or
// archived. mention://issue/… and mention://all/… links are ignored.
//
// "self-healed" mentions are treated as resolvable and do not cause rejection;
// the diagnostic for self-healed mentions is emitted post-save by the dispatch path.
//
// This is the fail-loud gate wired into CreateComment and UpdateComment: any
// agent mention that cannot be dispatched causes the operation to be rejected
// with a clear error rather than silently dropping the dispatch.
func (h *Handler) validateMentions(ctx context.Context, content string, workspaceID pgtype.UUID) error {
	mentions := util.ParseMentions(content)
	var failures []string
	for _, m := range mentions {
		if m.Type != "agent" && m.Type != "member" {
			continue
		}
		res := h.resolveMention(ctx, m, workspaceID)
		if res.Status != "resolved" && res.Status != "self-healed" {
			failures = append(failures, res.Reason)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("comment contains unresolvable mention(s): %s", strings.Join(failures, "; "))
}
