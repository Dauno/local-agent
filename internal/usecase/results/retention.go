package results

import (
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const maxRetentionAge = 3650 * 24 * time.Hour

// RetentionPolicy decides eligibility only. Deletion remains an adapter-owned
// operation once delivery and external-agent holds are available; uncertainty
// therefore retains data rather than removing a physically valid result.
type RetentionPolicy struct {
	Context      time.Duration
	Conversation time.Duration
	Workstream   time.Duration
	Exported     time.Duration
}

// RetentionEvidence distinguishes a completed negative lookup from an unknown
// or failed lookup. Every checker must complete before deletion is eligible.
type RetentionEvidence struct {
	ReferencesChecked         bool
	HasLiveReference          bool
	MaterializationsChecked   bool
	HasPendingMaterialization bool
	BackendChecked            bool
	BackendHeld               bool
	VerifiedPublicationAt     time.Time
}

func (p RetentionPolicy) Validate() error {
	for _, age := range []time.Duration{p.Context, p.Conversation, p.Workstream, p.Exported} {
		if age <= 0 || age > maxRetentionAge {
			return domain.ErrResultInvalid
		}
	}
	return nil
}

// Eligible returns false for any live durable reference, pending reservation,
// typed-backend hold, incomplete evidence, or unavailable identity.
func (p RetentionPolicy) Eligible(identity domain.ResultIdentity, now time.Time, evidence RetentionEvidence) bool {
	if p.Validate() != nil || identity.Validate() != nil || identity.State != domain.ResultAvailable || now.IsZero() ||
		!evidence.ReferencesChecked || !evidence.MaterializationsChecked || !evidence.BackendChecked ||
		evidence.HasLiveReference || evidence.HasPendingMaterialization || evidence.BackendHeld {
		return false
	}
	anchor := identity.CreatedAt
	if identity.Retention == domain.ResultRetentionExported {
		if evidence.VerifiedPublicationAt.IsZero() || evidence.VerifiedPublicationAt.Before(identity.CreatedAt) {
			return false
		}
		anchor = evidence.VerifiedPublicationAt
	}
	age := p.ageFor(identity.Retention)
	return !anchor.Add(age).After(now.UTC())
}

func (p RetentionPolicy) ageFor(class domain.ResultRetentionClass) time.Duration {
	switch class {
	case domain.ResultRetentionContext:
		return p.Context
	case domain.ResultRetentionConversation:
		return p.Conversation
	case domain.ResultRetentionWorkstream:
		return p.Workstream
	case domain.ResultRetentionExported:
		return p.Exported
	default:
		return 0
	}
}
