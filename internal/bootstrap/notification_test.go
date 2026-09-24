package bootstrap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/notify"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/FastR-D/FastTask/internal/scheduler"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// recordingSender stands in for a provider so these tests assert wiring, not protocols:
// the adapters have their own tests in internal/notify, and what is at risk here is that
// a queued notification is never picked up by anything.
type recordingSender struct {
	mu        sync.Mutex
	delivered []string
}

func (s *recordingSender) Kind() string { return notify.KindTelegram }

func (s *recordingSender) Verify(context.Context) error { return nil }

func (s *recordingSender) Send(_ context.Context, address string, _ notify.Message) (notify.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, address)
	return notify.Receipt{ProviderID: "recorded-1"}, nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

// queueOneNotification builds a channel and a subscription through the application
// services, then queues one message for the admin user. It returns the queued row's id.
func queueOneNotification(t *testing.T, app *application.App, sender *recordingSender) string {
	t.Helper()
	app.NotificationService.WithSenderFactory(func(notify.Spec) (notify.Sender, error) { return sender, nil })
	ctx := context.Background()
	var admin persistence.User
	if err := app.Store.DB.Where("identifier = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("the composition root must have created the admin: %v", err)
	}
	channel, err := app.CreateChannel(ctx, admin, application.ChannelCommand{
		Name: "接线测试", Provider: notify.KindTelegram, Secret: "123456789:wiring-token",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := app.CreateTarget(ctx, &admin, admin.ID, application.TargetCommand{
		ChannelID: channel.ID, Address: "-1001234567890", Label: "接线",
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	queued, err := app.Publish(ctx, admin.ID, application.Event{Topic: application.TopicProposalPending, Title: "有待确认的任务树变更"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued=%d, want one delivery", queued)
	}
	var message persistence.NotificationMessage
	if err := app.Store.DB.Order("created_at DESC").First(&message).Error; err != nil {
		t.Fatal(err)
	}
	return message.ID
}

// waitDelivered polls until the sender has been asked to deliver, or fails the test.
func waitDelivered(t *testing.T, sender *recordingSender) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sender.count() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing was delivered: the queue has no drainer in this role")
}

// assertRecordedSent fails unless the message reached the "sent" state, which is what
// proves the drainer did not merely call the provider but recorded the outcome.
func assertRecordedSent(t *testing.T, app *application.App, messageID string) {
	t.Helper()
	var message persistence.NotificationMessage
	if err := app.Store.DB.First(&message, "id = ?", messageID).Error; err != nil {
		t.Fatal(err)
	}
	if message.Status != persistence.NotificationSent || message.ProviderMessageID != "recorded-1" {
		t.Fatalf("message=%+v, want it recorded as sent", message)
	}
}

// TestSchedulerRoleDrainsTheNotificationQueue proves the scheduler role owns delivery
// (doc/notification.md §5): a message queued by the application is picked up by a sweeper
// the composition root registered, with no HTTP request and no worker involved. Without
// this the queue would be write-only in the default single-binary deployment.
func TestSchedulerRoleDrainsTheNotificationQueue(t *testing.T) {
	cfg := testConfig(t)
	var (
		app         *application.App
		maintenance *scheduler.Scheduler
	)
	test := fxtest.New(t, SchedulerRole, fx.Supply(cfg), fx.Populate(&app, &maintenance), fx.NopLogger)
	test.RequireStart()
	defer test.RequireStop()

	if got := maintenance.SweeperCount(); got < 4 {
		t.Fatalf("the scheduler registered %d sweepers, want the harness pair plus the notification drain and prune", got)
	}
	sender := &recordingSender{}
	messageID := queueOneNotification(t, app, sender)

	// The scheduler owns the clock; a sweep is run rather than waited for, so the test
	// does not depend on the interval.
	maintenance.SweepOnce(context.Background())
	waitDelivered(t, sender)
	assertRecordedSent(t, app, messageID)
}

// TestWorkerRoleDrainsNotificationsWithoutAScheduler covers the deployment that runs
// `serve --with-worker` (or `fasttask worker`) with no scheduler: the worker drains the
// same queue as a fallback, so a role that is running never leaves notifications stuck.
// The claim is a conditional update, which is what makes the two drainners safe together.
func TestWorkerRoleDrainsNotificationsWithoutAScheduler(t *testing.T) {
	cfg := testConfig(t)
	var app *application.App
	test := fxtest.New(t, WorkerRole, fx.Supply(cfg), fx.Populate(&app), fx.NopLogger)
	test.RequireStart()
	defer test.RequireStop()

	sender := &recordingSender{}
	messageID := queueOneNotification(t, app, sender)
	waitDelivered(t, sender)
	assertRecordedSent(t, app, messageID)
}
