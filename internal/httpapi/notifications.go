package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/danielgtaylor/huma/v2"
)

// The notification surface (doc/notification.md §9, doc/interface.md §21).
//
// Two halves share one registrar because they share one service and one set of shapes,
// and because splitting them would put the admin half in admin.go next to users and
// model providers while the routes it manages live elsewhere:
//
//   - /notifications/*       a user's own subscriptions. This is how a phone registers
//                            its push token and how a person stops a channel reaching them.
//   - /admin/notifications/* the operator's console: channels and their credentials,
//                            every user's subscriptions, the delivery log, a test send
//                            and a broadcast.
//
// A credential never crosses this boundary in either direction. A channel is written with
// its secret and read back as a hint; a target is written with its address and read back
// masked, because an FCM registration token or an APNs device token in a response body is
// a device credential handed to whoever can read the response.

const (
	notificationChannelETag = "notify_channel"
	notificationTargetETag  = "notify_target"
)

func (s notificationRoutes) RegisterRoutes(api huma.API) {
	s.registerUserNotifications(api)
	s.registerAdminNotifications(api)
}

func (s notificationRoutes) registerUserNotifications(api huma.API) {
	type emptyInput struct{}
	register(api, "list-notification-channels", http.MethodGet, "/notifications/channels", "List channels a user may subscribe to", userSecurity(), func(ctx context.Context, input *emptyInput) (*listResponse[application.ChannelView], error) {
		channels, err := s.app.ListChannels(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.ChannelView]{}
		for _, channel := range channels {
			// A disabled channel is the operator's business, not a subscription option.
			if channel.Status == "active" {
				out.Body.Items = append(out.Body.Items, channel)
			}
		}
		if out.Body.Items == nil {
			out.Body.Items = []application.ChannelView{}
		}
		return out, nil
	})

	type listTargetsInput struct {
		Status string `query:"status"`
	}
	register(api, "list-notification-targets", http.MethodGet, "/notifications/targets", "List my notification targets", userSecurity(), func(ctx context.Context, input *listTargetsInput) (*listResponse[application.TargetView], error) {
		targets, err := s.app.ListTargets(ctx, application.TargetFilter{UserID: principal(ctx).UserID, Status: input.Status})
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.TargetView]{}
		out.Body.Items = targets
		return out, nil
	})

	type createTargetInput struct {
		Body struct {
			ChannelID string `json:"channel_id" minLength:"1" maxLength:"80"`
			Address   string `json:"address" minLength:"1" maxLength:"4096"`
			Label     string `json:"label,omitempty" maxLength:"60"`
		}
	}
	register(api, "create-notification-target", http.MethodPost, "/notifications/targets", "Register one of my notification targets", userSecurity(), func(ctx context.Context, input *createTargetInput) (*itemResponse[application.TargetView], error) {
		target, err := s.app.CreateTarget(ctx, nil, principal(ctx).UserID, application.TargetCommand{
			ChannelID: input.Body.ChannelID, Address: input.Body.Address, Label: input.Body.Label,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.TargetView]{Body: *target}, nil
	})

	type updateTargetInput struct {
		TargetID string `path:"target_id"`
		IfMatch  string `header:"If-Match" required:"true"`
		Body     struct {
			Label   *string `json:"label,omitempty" maxLength:"60"`
			Status  *string `json:"status,omitempty" enum:"active,disabled"`
			Address *string `json:"address,omitempty" minLength:"1" maxLength:"4096"`
		}
	}
	register(api, "update-notification-target", http.MethodPatch, "/notifications/targets/{target_id}", "Update one of my notification targets", userSecurity(), func(ctx context.Context, input *updateTargetInput) (*resourceResponse[application.TargetView], error) {
		expected, err := revisionFromResourceETag(input.IfMatch, notificationTargetETag, input.TargetID)
		if err != nil {
			return nil, mapError(err)
		}
		target, err := s.app.UpdateTarget(ctx, principal(ctx).UserID, nil, input.TargetID, expected, application.UpdateTargetCommand{
			Label: input.Body.Label, Status: input.Body.Status, Address: input.Body.Address,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.TargetView]{
			ETag: application.StrongETag(notificationTargetETag, target.ID, target.Revision), Body: *target,
		}, nil
	})

	register(api, "delete-notification-target", http.MethodDelete, "/notifications/targets/{target_id}", "Delete one of my notification targets", userSecurity(), func(ctx context.Context, input *struct {
		TargetID string `path:"target_id"`
	}) (*struct{}, error) {
		if err := s.app.DeleteTarget(ctx, principal(ctx).UserID, nil, input.TargetID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})

	register(api, "test-notification-target", http.MethodPost, "/notifications/targets/{target_id}/test", "Send a test notification to one of my targets", userSecurity(), func(ctx context.Context, input *struct {
		TargetID string `path:"target_id"`
	}) (*itemResponse[application.DeliveryResult], error) {
		result, err := s.app.TestTarget(ctx, nil, principal(ctx).UserID, input.TargetID)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.DeliveryResult]{Body: result}, nil
	})

	type listMessagesInput struct {
		Status string `query:"status"`
		Topic  string `query:"topic"`
		Limit  int    `query:"limit"`
	}
	register(api, "list-notification-messages", http.MethodGet, "/notifications/messages", "List my recent notification deliveries", userSecurity(), func(ctx context.Context, input *listMessagesInput) (*listResponse[application.MessageView], error) {
		messages, err := s.app.ListMessages(ctx, application.MessageFilter{
			UserID: principal(ctx).UserID, Status: input.Status, Topic: input.Topic, Limit: input.Limit,
		})
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.MessageView]{}
		out.Body.Items = messages
		return out, nil
	})
}

func (s notificationRoutes) registerAdminNotifications(api huma.API) {
	register(api, "list-admin-notification-channels", http.MethodGet, "/admin/notifications/channels", "List notification channels", userSecurity(), func(ctx context.Context, input *struct{}) (*listResponse[application.ChannelView], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		channels, err := s.app.ListChannels(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.ChannelView]{}
		out.Body.Items = channels
		return out, nil
	})

	type createChannelInput struct {
		Body struct {
			Name     string            `json:"name" minLength:"1" maxLength:"120"`
			Provider string            `json:"provider" enum:"telegram,bark,fcm,apns"`
			Endpoint string            `json:"endpoint,omitempty" maxLength:"500"`
			Settings map[string]string `json:"settings,omitempty"`
			Secret   string            `json:"secret,omitempty" maxLength:"20000"`
			Status   string            `json:"status,omitempty" enum:"active,disabled"`
		}
	}
	register(api, "create-admin-notification-channel", http.MethodPost, "/admin/notifications/channels", "Create notification channel", userSecurity(), func(ctx context.Context, input *createChannelInput) (*itemResponse[application.ChannelView], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		channel, err := s.app.CreateChannel(ctx, actor, application.ChannelCommand{
			Name: input.Body.Name, Provider: input.Body.Provider, Endpoint: input.Body.Endpoint,
			Settings: input.Body.Settings, Secret: input.Body.Secret, Status: input.Body.Status,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.ChannelView]{Body: *channel}, nil
	})

	type updateChannelInput struct {
		ChannelID string `path:"channel_id"`
		IfMatch   string `header:"If-Match" required:"true"`
		Body      struct {
			Name     *string            `json:"name,omitempty" minLength:"1" maxLength:"120"`
			Endpoint *string            `json:"endpoint,omitempty" maxLength:"500"`
			Settings *map[string]string `json:"settings,omitempty"`
			Secret   *string            `json:"secret,omitempty" maxLength:"20000"`
			Status   *string            `json:"status,omitempty" enum:"active,disabled"`
		}
	}
	register(api, "update-admin-notification-channel", http.MethodPatch, "/admin/notifications/channels/{channel_id}", "Update notification channel", userSecurity(), func(ctx context.Context, input *updateChannelInput) (*resourceResponse[application.ChannelView], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		expected, err := revisionFromResourceETag(input.IfMatch, notificationChannelETag, input.ChannelID)
		if err != nil {
			return nil, mapError(err)
		}
		channel, err := s.app.UpdateChannel(ctx, actor, input.ChannelID, expected, application.UpdateChannelCommand{
			Name: input.Body.Name, Endpoint: input.Body.Endpoint, Settings: input.Body.Settings,
			Secret: input.Body.Secret, Status: input.Body.Status,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.ChannelView]{
			ETag: application.StrongETag(notificationChannelETag, channel.ID, channel.Revision), Body: *channel,
		}, nil
	})

	type channelInput struct {
		ChannelID string `path:"channel_id"`
	}
	register(api, "delete-admin-notification-channel", http.MethodDelete, "/admin/notifications/channels/{channel_id}", "Delete notification channel", userSecurity(), func(ctx context.Context, input *channelInput) (*struct{}, error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if err := s.app.DeleteChannel(ctx, actor, input.ChannelID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})

	register(api, "verify-admin-notification-channel", http.MethodPost, "/admin/notifications/channels/{channel_id}/verification", "Verify notification channel credentials", userSecurity(), func(ctx context.Context, input *channelInput) (*itemResponse[application.ChannelCheck], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		check, err := s.app.VerifyChannel(ctx, actor, input.ChannelID)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.ChannelCheck]{Body: check}, nil
	})

	type testChannelInput struct {
		ChannelID string `path:"channel_id"`
		Body      struct {
			Address string `json:"address" minLength:"1" maxLength:"4096"`
		}
	}
	register(api, "test-admin-notification-channel", http.MethodPost, "/admin/notifications/channels/{channel_id}/test", "Send a test notification through a channel", userSecurity(), func(ctx context.Context, input *testChannelInput) (*itemResponse[application.DeliveryResult], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		result, err := s.app.SendTest(ctx, actor, input.ChannelID, input.Body.Address)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.DeliveryResult]{Body: result}, nil
	})

	type listTargetsInput struct {
		UserID    string `query:"user_id"`
		ChannelID string `query:"channel_id"`
		Status    string `query:"status"`
		Limit     int    `query:"limit"`
	}
	register(api, "list-admin-notification-targets", http.MethodGet, "/admin/notifications/targets", "List notification targets", userSecurity(), func(ctx context.Context, input *listTargetsInput) (*listResponse[application.TargetView], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		targets, err := s.app.ListTargets(ctx, application.TargetFilter{
			UserID: input.UserID, ChannelID: input.ChannelID, Status: input.Status, Limit: input.Limit,
		})
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.TargetView]{}
		out.Body.Items = targets
		return out, nil
	})

	type createTargetInput struct {
		Body struct {
			UserID    string `json:"user_id" minLength:"1" maxLength:"80"`
			ChannelID string `json:"channel_id" minLength:"1" maxLength:"80"`
			Address   string `json:"address" minLength:"1" maxLength:"4096"`
			Label     string `json:"label,omitempty" maxLength:"60"`
			Status    string `json:"status,omitempty" enum:"active,disabled"`
		}
	}
	register(api, "create-admin-notification-target", http.MethodPost, "/admin/notifications/targets", "Register a notification target for a user", userSecurity(), func(ctx context.Context, input *createTargetInput) (*itemResponse[application.TargetView], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		target, err := s.app.CreateTarget(ctx, &actor, strings.TrimSpace(input.Body.UserID), application.TargetCommand{
			ChannelID: input.Body.ChannelID, Address: input.Body.Address, Label: input.Body.Label, Status: input.Body.Status,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.TargetView]{Body: *target}, nil
	})

	type updateTargetInput struct {
		TargetID string `path:"target_id"`
		IfMatch  string `header:"If-Match" required:"true"`
		Body     struct {
			Label   *string `json:"label,omitempty" maxLength:"60"`
			Status  *string `json:"status,omitempty" enum:"active,disabled"`
			Address *string `json:"address,omitempty" minLength:"1" maxLength:"4096"`
		}
	}
	register(api, "update-admin-notification-target", http.MethodPatch, "/admin/notifications/targets/{target_id}", "Update a notification target", userSecurity(), func(ctx context.Context, input *updateTargetInput) (*resourceResponse[application.TargetView], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		expected, err := revisionFromResourceETag(input.IfMatch, notificationTargetETag, input.TargetID)
		if err != nil {
			return nil, mapError(err)
		}
		target, err := s.app.UpdateTarget(ctx, "", &actor, input.TargetID, expected, application.UpdateTargetCommand{
			Label: input.Body.Label, Status: input.Body.Status, Address: input.Body.Address,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.TargetView]{
			ETag: application.StrongETag(notificationTargetETag, target.ID, target.Revision), Body: *target,
		}, nil
	})

	type targetInput struct {
		TargetID string `path:"target_id"`
	}
	register(api, "delete-admin-notification-target", http.MethodDelete, "/admin/notifications/targets/{target_id}", "Delete a notification target", userSecurity(), func(ctx context.Context, input *targetInput) (*struct{}, error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if err := s.app.DeleteTarget(ctx, "", &actor, input.TargetID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})

	register(api, "test-admin-notification-target", http.MethodPost, "/admin/notifications/targets/{target_id}/test", "Send a test notification to a target", userSecurity(), func(ctx context.Context, input *targetInput) (*itemResponse[application.DeliveryResult], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		result, err := s.app.TestTarget(ctx, &actor, "", input.TargetID)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.DeliveryResult]{Body: result}, nil
	})

	type listMessagesInput struct {
		UserID    string `query:"user_id"`
		ChannelID string `query:"channel_id"`
		Status    string `query:"status"`
		Topic     string `query:"topic"`
		Limit     int    `query:"limit"`
	}
	register(api, "list-admin-notification-messages", http.MethodGet, "/admin/notifications/messages", "List notification deliveries", userSecurity(), func(ctx context.Context, input *listMessagesInput) (*listResponse[application.MessageView], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		messages, err := s.app.ListMessages(ctx, application.MessageFilter{
			UserID: input.UserID, ChannelID: input.ChannelID, Status: input.Status, Topic: input.Topic, Limit: input.Limit,
		})
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.MessageView]{}
		out.Body.Items = messages
		return out, nil
	})

	type broadcastInput struct {
		Body struct {
			Topic     string `json:"topic,omitempty" maxLength:"80"`
			Title     string `json:"title" minLength:"1" maxLength:"160"`
			Body      string `json:"body,omitempty" maxLength:"1000"`
			URL       string `json:"url,omitempty" maxLength:"500"`
			ChannelID string `json:"channel_id,omitempty" maxLength:"80"`
		}
	}
	type broadcastResult struct {
		Queued int `json:"queued"`
	}
	register(api, "broadcast-admin-notification", http.MethodPost, "/admin/notifications/broadcast", "Queue a notification for every reachable user", userSecurity(), func(ctx context.Context, input *broadcastInput) (*itemResponse[broadcastResult], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		queued, err := s.app.Broadcast(ctx, actor, application.BroadcastCommand{
			Topic: input.Body.Topic, Title: input.Body.Title, Body: input.Body.Body, URL: input.Body.URL, ChannelID: input.Body.ChannelID,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[broadcastResult]{Body: broadcastResult{Queued: queued}}, nil
	})

	register(api, "dispatch-admin-notifications", http.MethodPost, "/admin/notifications/dispatch", "Deliver the notifications that are due now", userSecurity(), func(ctx context.Context, input *struct{}) (*itemResponse[application.DispatchReport], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		report, err := s.app.DispatchDue(ctx, 0)
		if err != nil {
			return nil, mapError(err)
		}
		if _, err := s.app.Prune(ctx); err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.DispatchReport]{Body: report}, nil
	})
}
