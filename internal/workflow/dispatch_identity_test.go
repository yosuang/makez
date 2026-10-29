package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	enums "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
)

type singleHistory struct {
	event *historypb.HistoryEvent
	err   error
	read  bool
}

func (i *singleHistory) HasNext() bool                          { return !i.read }
func (i *singleHistory) Next() (*historypb.HistoryEvent, error) { i.read = true; return i.event, i.err }

func TestDuplicateWorkflowRequiresMatchingDurableInput(t *testing.T) {
	for _, scenario := range []string{"matching", "mismatch", "expired-history"} {
		t.Run(scenario, func(t *testing.T) {
			in := Input{WorkspaceID: "workspace", TaskID: "task"}
			original := in
			if scenario == "mismatch" {
				original.WorkspaceID = "foreign"
			}
			payload, err := converter.GetDefaultDataConverter().ToPayloads(original)
			if err != nil {
				t.Fatal(err)
			}
			iterator := &singleHistory{event: &historypb.HistoryEvent{Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{Input: payload}}}}
			if scenario == "expired-history" {
				iterator.err = serviceerror.NewNotFound("history expired")
			}
			remote := new(mocks.Client)
			remote.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
				return o.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE && o.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL
			}), mock.Anything, in).Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("existing", "request", "run")).Once()
			remote.On("GetWorkflowHistory", mock.Anything, ID(in.WorkspaceID, in.TaskID), "run", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT).Return(iterator).Once()
			err = start(t.Context(), remote, ID(in.WorkspaceID, in.TaskID), Task, in)
			if scenario == "matching" && err != nil || scenario != "matching" && err == nil {
				t.Fatalf("%s: %v", scenario, err)
			}
			if scenario == "expired-history" {
				var missing *serviceerror.NotFound
				if !errors.As(err, &missing) {
					t.Fatal("history uncertainty lost", err)
				}
			}
			remote.AssertExpectations(t)
		})
	}
}
