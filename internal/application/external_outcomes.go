package application

import (
	"context"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/integrationclient"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// ExternalOutcomes uses the caller's existing private source connection. It
// reads fresh source state, so removed records do not survive in a stale local
// accomplishments cache. Source records never mutate task completion state.
func (s *RemoteToolService) ExternalOutcomes(ctx context.Context, userID, system, week string, limit, offset int) (integrationclient.OutcomePage, error) {
	if !validIntegrationSystem(system) || limit < 1 || limit > 10 || offset < 0 || offset > 1000000 {
		return integrationclient.OutcomePage{}, ErrValidation
	}
	var user persistence.User
	if err := s.Store.DB.WithContext(ctx).Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
		return integrationclient.OutcomePage{}, notFound(err)
	}
	var start, end *time.Time
	week = strings.TrimSpace(week)
	if week != "all" {
		loc, _ := resolveZone(user.Timezone)
		if week == "" {
			week = domain.CurrentISOWeek(persistence.Now(), loc)
		}
		from, to, _, _, err := domain.ISOWeekRange(week, loc)
		if err != nil {
			return integrationclient.OutcomePage{}, ErrValidation
		}
		start, end = &from, &to
	}
	config, problem := s.remoteConfig(ctx, userID, system)
	if problem != nil {
		return integrationclient.OutcomeUnavailable(system, problem), nil
	}
	return s.client.Outcomes(ctx, config, start, end, limit, offset), nil
}
