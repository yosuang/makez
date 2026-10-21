package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/store"
	"github.com/stretchr/testify/mock"
	enums "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
)

func TestSlowRecoveryDoesNotBlockOutboxDelivery(t *testing.T) {
	db := activityStore(t)
	old := activityTask(t, db, true)
	pending, err := db.Pending(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range pending {
		if err := db.Delivered(t.Context(), row.ID); err != nil {
			t.Fatal(err)
		}
	}
	remote := new(mocks.Client)
	entered := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan struct{})
	remote.On("DescribeWorkflowExecution", mock.Anything, ID(old.WorkspaceID, old.ID), "").Run(func(args mock.Arguments) {
		close(entered)
		select {
		case <-release:
		case <-args.Get(0).(context.Context).Done():
		}
	}).Return(&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING}}, nil).Once()
	remote.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool { return o.TaskQueue == Queue }), mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(delivered) }).Return(nil, nil).Once()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); Dispatch(ctx, db, remote) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not start")
	}
	_, _, err = db.Submit(t.Context(), "activity-test", "fresh", store.SubmitInput{Prompt: "new", AgentID: "demo", AgentVersion: "1", ProfileID: "local", ProfileVersion: "1", TimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery blocked fresh outbox delivery")
	}
	remote.AssertExpectations(t)
}
