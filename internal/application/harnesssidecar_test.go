package application

import (
	"context"
	"github.com/FastR-D/FastTask/internal/persistence"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type sidecarFixtureDriver struct {
	drive func(context.Context, SidecarRun) (SidecarResult, error)
}

func (d sidecarFixtureDriver) Healthy(context.Context) bool         { return true }
func (d sidecarFixtureDriver) Cancel(context.Context, string) error { return nil }
func (d sidecarFixtureDriver) Drive(ctx context.Context, run SidecarRun) (SidecarResult, error) {
	return d.drive(ctx, run)
}
func TestHarnessWorkerSidecarCompletionGenerationAndStatus(t *testing.T) {
	for _, mode := range []string{"normal", "rotation", "new-generation", "unknown-stop", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			store, owner, _ := panelTestStore(t)
			ctx := context.Background()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			var svc *AgentService
			driver := sidecarFixtureDriver{drive: func(ctx context.Context, input SidecarRun) (SidecarResult, error) {
				p, err := svc.AuthenticateHarness(ctx, "Bearer "+input.HarnessToken)
				if err != nil {
					return SidecarResult{}, err
				}
				if mode == "rotation" {
					svc.store.DB.Model(&persistence.AgentHarnessToken{}).Where("id = ?", p.TokenID).Update("expires_at", persistence.Now().Add(5*time.Minute))
					reply, err := svc.HarnessHeartbeat(ctx, p)
					if err != nil {
						return SidecarResult{}, err
					}
					p, err = svc.AuthenticateHarness(ctx, "Bearer "+reply.HarnessToken)
					if err != nil {
						return SidecarResult{}, err
					}
				}
				if mode == "new-generation" {
					grant, err := svc.IssueRunGrant(ctx, owner, input.RunID, "")
					if err != nil {
						return SidecarResult{}, err
					}
					p, err = svc.AuthenticateHarness(ctx, "Bearer "+grant.HarnessToken)
					if err != nil {
						return SidecarResult{}, err
					}
				}
				stream, err := svc.OpenModelProxy(ctx, p, []byte(`{}`))
				if err != nil {
					return SidecarResult{}, err
				}
				_, err = io.Copy(io.Discard, stream.Body)
				stream.Body.Close()
				if err != nil {
					return SidecarResult{}, err
				}
				if mode == "cancelled" {
					if err := svc.RequestCancellation(ctx, owner, input.RunID); err != nil {
						return SidecarResult{}, err
					}
				}
				reason := "end_turn"
				if mode == "unknown-stop" {
					reason = "not-a-stop"
				}
				return SidecarResult{StopReason: reason, Checkpoint: []byte("fixture-checkpoint")}, nil
			}}
			svc = NewAgentService(New(store), WithSidecarDriver(driver), WithCredentialsResolver(func(context.Context) (*ModelCredentials, error) {
				return &ModelCredentials{BaseURL: provider.URL, Model: "fixture", APIKey: "fixture-private"}, nil
			}))
			submitted, err := svc.SubmitCommands(ctx, owner, CommandsRequest{HarnessMode: HarnessModeSidecar, Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "run via worker"}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			run, _ := svc.repo.GetRun(ctx, owner, submitted.RunID)
			var job persistence.AgentJob
			if err = store.DB.First(&job, "id = ?", run.JobID).Error; err != nil {
				t.Fatal(err)
			}
			output, err := svc.ExecuteRun(ctx, job)
			current, _ := svc.repo.GetRun(ctx, owner, run.ID)
			switch mode {
			case "new-generation":
				if err == nil || current.Status == persistence.RunSucceeded {
					t.Fatalf("stale worker completed newer host: %+v %v", output, err)
				}
			case "unknown-stop":
				if err == nil || current.Status != persistence.RunFailed || output["status"] != persistence.RunFailed {
					t.Fatalf("worker falsely reported success: %+v %v %s", output, err, current.Status)
				}
			case "cancelled":
				if err != nil || current.Status != persistence.RunCancelled || output["status"] != persistence.RunCancelled {
					t.Fatalf("cancelled completion: %+v %v", output, err)
				}
			default:
				if err != nil || current.Status != persistence.RunSucceeded || output["status"] != persistence.RunSucceeded {
					t.Fatalf("sidecar completion: %+v %v %s", output, err, current.Status)
				}
			}
		})
	}
}
