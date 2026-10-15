package application

import (
	"errors"
	"fmt"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

func supportJobPlanTx(tx *gorm.DB, job persistence.AgentJob) (persistence.DailyPlan, error) {
	var plan persistence.DailyPlan
	if job.SubjectType != "daily_plan" {
		return plan, ErrValidation
	}
	if err := tx.Where("id = ? AND user_id = ?", job.SubjectID, job.UserID).First(&plan).Error; err != nil {
		return plan, notFound(err)
	}
	return plan, nil
}

func supportPlanReadyTx(tx *gorm.DB, plan persistence.DailyPlan, now time.Time) error {
	if plan.Status != "active" {
		return fmt.Errorf("%w: support items require an active plan", ErrConflict)
	}
	var user persistence.User
	if err := tx.Where("id = ?", plan.UserID).First(&user).Error; err != nil {
		return notFound(err)
	}
	loc, _ := resolveZone(user.Timezone)
	if plan.LocalDate > now.In(loc).Format("2006-01-02") {
		return fmt.Errorf("%w: support items cannot be generated for a future plan", ErrConflict)
	}
	return nil
}

// Internal enqueue callers may omit the revision. Capture it only while creating
// a new job; an already queued result without a baseline never gains one later.
func captureSupportJobBaselineTx(tx *gorm.DB, job *persistence.AgentJob, now time.Time) error {
	plan, err := supportJobPlanTx(tx, *job)
	if err != nil {
		return err
	}
	if job.BaseRevision != 0 && job.BaseRevision != plan.Revision {
		return ErrRevision
	}
	if err := supportPlanReadyTx(tx, plan, now); err != nil {
		return err
	}
	job.BaseRevision = plan.Revision
	return nil
}

func validateSupportJobTx(tx *gorm.DB, job persistence.AgentJob, now time.Time) (persistence.DailyPlan, error) {
	plan, err := supportJobPlanTx(tx, job)
	if err != nil {
		return plan, err
	}
	if job.BaseRevision <= 0 || job.BaseRevision != plan.Revision {
		return plan, fmt.Errorf("%w: the plan changed after support generation was requested; request a new suggestion", ErrRevision)
	}
	return plan, supportPlanReadyTx(tx, plan, now)
}

// Only support-plan eligibility failures become terminal job outcomes here.
// Database or transport errors retain the existing worker recovery behavior.
func supportJobFailureCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrRevision):
		return "REVISION_MISMATCH", true
	case errors.Is(err, domain.ErrSupportBeforeCoreDone):
		return "CORE_NOT_DONE", true
	case errors.Is(err, ErrConflict):
		return "PLAN_NOT_ACTIONABLE", true
	case errors.Is(err, ErrNotFound), persistence.IsNotFound(err):
		return "PLAN_NOT_FOUND", true
	case errors.Is(err, ErrValidation):
		return "SUPPORT_PLAN_INVALID", true
	default:
		return "", false
	}
}
