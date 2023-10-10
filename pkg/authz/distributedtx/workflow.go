package distributedtx

import (
	"fmt"
	"k8s.io/klog/v2"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/cespare/xxhash/v2"
	"github.com/cschleiden/go-workflows/workflow"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	lockResourceType                         = "lock"
	lockRelationName                         = "workflow"
	workflowResourceType                     = "workflow"
	MaxKubeAttempts                          = 5
	StrategyOptimisticWriteToSpiceDBAndKube  = "Optimistic"
	StrategyPessimisticWriteToSpiceDBAndKube = "Pessimistic"
	DefaultWorkflowTimeout                   = time.Second * 30
)

var KubeBackoff = wait.Backoff{
	Duration: 100 * time.Millisecond,
	Factor:   2,
	Jitter:   0.1,
	Steps:    MaxKubeAttempts,
}

type WriteObjInput struct {
	RequestInfo   *request.RequestInfo
	Header        http.Header
	UserInfo      *user.DefaultInfo
	ObjectMeta    *metav1.ObjectMeta
	Rels          []*v1.Relationship
	Preconditions []*v1.Precondition
	Body          []byte
}

func (input *WriteObjInput) validate() error {
	if input.UserInfo.GetName() == "" {
		return fmt.Errorf("missing user info in CreateObjectInput")
	}

	return nil
}

func (input *WriteObjInput) toKubeReqInput() *KubeReqInput {
	return &KubeReqInput{
		RequestInfo: input.RequestInfo,
		Header:      input.Header,
		ObjectMeta:  input.ObjectMeta,
		Body:        input.Body,
	}
}

type RollbackRelationships []*v1.RelationshipUpdate

func NewRollbackRelationships(rels ...*v1.RelationshipUpdate) *RollbackRelationships {
	r := RollbackRelationships(rels)
	return &r
}

func (r *RollbackRelationships) WithRels(relationships ...*v1.RelationshipUpdate) *RollbackRelationships {
	*r = append(*r, relationships...)
	return r
}

func (r *RollbackRelationships) Cleanup(ctx workflow.Context) {

	invert := func(op v1.RelationshipUpdate_Operation) v1.RelationshipUpdate_Operation {
		switch op {
		case v1.RelationshipUpdate_OPERATION_CREATE:
			fallthrough
		case v1.RelationshipUpdate_OPERATION_TOUCH:
			return v1.RelationshipUpdate_OPERATION_DELETE
		case v1.RelationshipUpdate_OPERATION_DELETE:
			return v1.RelationshipUpdate_OPERATION_TOUCH
		}
		return v1.RelationshipUpdate_OPERATION_UNSPECIFIED
	}

	updates := make([]*v1.RelationshipUpdate, 0, len(*r))
	for _, rel := range *r {
		rel := rel
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    invert(rel.Operation),
			Relationship: rel.Relationship,
		})
	}

	for {
		f := workflow.ExecuteActivity[*v1.WriteRelationshipsResponse](ctx,
			workflow.DefaultActivityOptions,
			activityHandler.WriteToSpiceDB,
			&v1.WriteRelationshipsRequest{Updates: updates})

		if _, err := f.Get(ctx); err != nil {
			if s, ok := status.FromError(err); ok {
				if s.Code() == codes.InvalidArgument {
					fmt.Println("unrecoverable error when rolling back tuples", err)
					break
				}
			}
			fmt.Println("error rolling back tuples", err)
			continue
		}
		// no error, delete succeeded, exit loop
		break
	}
}

// PessimisticWriteToSpiceDBAndKube ensures that a write exists in both SpiceDB
// and kube, or neither, using locks. It prevents multiple users from writing
// the same object/fields at the same time
func PessimisticWriteToSpiceDBAndKube(ctx workflow.Context, input *WriteObjInput) (*KubeResp, error) {
	if err := input.validate(); err != nil {
		return nil, fmt.Errorf("invalid input to PessimisticWriteToSpiceDBAndKube: %w", err)
	}

	instance := workflow.WorkflowInstance(ctx)
	resourceLockRel := ResourceLockRel(input, instance.InstanceID)

	// tuples to remove when the workflow is complete.
	// in some cases we will roll back the input, in all cases we remove
	// the lock when complete.
	rollback := NewRollbackRelationships(resourceLockRel)

	preconditions := []*v1.Precondition{
		resourceLockDoesNotExist(resourceLockRel.Relationship),
	}
	preconditions = append(preconditions, input.Preconditions...)

	operation := v1.RelationshipUpdate_OPERATION_TOUCH
	if input.RequestInfo.Verb == "delete" {
		operation = v1.RelationshipUpdate_OPERATION_DELETE
	}

	updates := make([]*v1.RelationshipUpdate, 0, len(input.Rels))
	for _, r := range input.Rels {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    operation,
			Relationship: r,
		})
	}

	arg := &v1.WriteRelationshipsRequest{
		OptionalPreconditions: preconditions,
		Updates:               append(updates, resourceLockRel),
	}

	_, err := workflow.ExecuteActivity[*v1.WriteRelationshipsResponse](ctx,
		workflow.DefaultActivityOptions,
		activityHandler.WriteToSpiceDB,
		arg).Get(ctx)
	if err != nil {
		// request failed for some reason
		fmt.Println("spicedb write failed", err)
		for _, u := range updates {
			fmt.Println(u.String(), u)
		}

		rollback.WithRels(updates...).Cleanup(ctx)

		// if the spicedb write fails, report it as a kube conflict error
		// we return this for any error, not just lock conflicts, so that the
		// user will attempt to retry instead of the workflow (nothing from the
		// workflow has succeeded, so there's not much use in retrying automatically).
		return KubeConflict(err, input), nil
	}

	backoff := wait.Backoff{
		Duration: KubeBackoff.Duration,
		Factor:   KubeBackoff.Factor,
		Jitter:   KubeBackoff.Jitter,
		Steps:    KubeBackoff.Steps,
		Cap:      KubeBackoff.Cap,
	}
	for i := 0; i <= MaxKubeAttempts; i++ {
		// Attempt to write to kube
		out, err := workflow.ExecuteActivity[*KubeResp](ctx,
			workflow.DefaultActivityOptions,
			activityHandler.WriteToKube,
			input.toKubeReqInput()).Get(ctx)
		if err != nil {
			// didn't get a response from kube, try again
			fmt.Println("kube write failed", err)
			time.Sleep(backoff.Step())
			continue
		}

		details := out.Err.ErrStatus.Details
		if details != nil && details.RetryAfterSeconds > 0 {
			time.Sleep(time.Duration(out.Err.ErrStatus.Details.RetryAfterSeconds) * time.Second)
			continue
		}

		if isSuccessfulCreate(input, out) || isSuccessfulDelete(input, out) {
			rollback.Cleanup(ctx)
			return out, nil
		}

		// some other status code is some other type of error, remove
		// the original tuple and the lock tuple
		klog.V(3).ErrorS(err, "unsuccessful Kube API operation on PessimisticWriteToSpiceDBAndKube", "response", out)
		rollback.WithRels(updates...).Cleanup(ctx)
		return out, nil
	}

	rollback.WithRels(updates...).Cleanup(ctx)
	return nil, fmt.Errorf("failed to communicate with kubernetes after %d attempts", MaxKubeAttempts)
}

func isSuccessfulDelete(input *WriteObjInput, out *KubeResp) bool {
	return input.RequestInfo.Verb == "delete" && (out.StatusCode == http.StatusNotFound || out.StatusCode == http.StatusOK)
}

func isSuccessfulCreate(input *WriteObjInput, out *KubeResp) bool {
	return input.RequestInfo.Verb == "create" && (out.StatusCode == http.StatusConflict || out.StatusCode == http.StatusCreated || out.StatusCode == http.StatusOK)
}

// OptimisticWriteToSpiceDBAndKube ensures that a write exists in both SpiceDB and kube,
// or neither. It attempts to perform the writes and rolls back if errors are
// encountered, leaving the user to retry on write conflicts.
func OptimisticWriteToSpiceDBAndKube(ctx workflow.Context, input *WriteObjInput) (*KubeResp, error) {
	if err := input.validate(); err != nil {
		return nil, fmt.Errorf("invalid input to PessimisticWriteToSpiceDBAndKube: %w", err)
	}

	// TODO: this could optionally use dry-run to preflight the kube request
	operation := v1.RelationshipUpdate_OPERATION_CREATE
	if input.RequestInfo.Verb == "delete" {
		operation = v1.RelationshipUpdate_OPERATION_DELETE
	}

	updates := make([]*v1.RelationshipUpdate, 0, len(input.Rels))
	for _, r := range input.Rels {
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    operation,
			Relationship: r,
		})
	}

	rollback := NewRollbackRelationships(updates...)
	_, err := workflow.ExecuteActivity[*v1.WriteRelationshipsResponse](ctx,
		workflow.DefaultActivityOptions,
		activityHandler.WriteToSpiceDB,
		&v1.WriteRelationshipsRequest{
			Updates: updates,
		}).Get(ctx)
	if err != nil {
		rollback.Cleanup(ctx)
		fmt.Println("SpiceDB WRITE ERR", err)
		// report spicedb write errors as conflicts
		return KubeConflict(err, input), nil
	}

	out, err := workflow.ExecuteActivity[*KubeResp](ctx,
		workflow.DefaultActivityOptions,
		activityHandler.WriteToKube,
		input.toKubeReqInput()).Get(ctx)
	if err != nil {
		// if there's an error, might need to roll back the spicedb write

		// check if object exists - the activity may have failed, but the write to Kube could have succeeded
		exists, err := workflow.ExecuteActivity[bool](ctx,
			workflow.DefaultActivityOptions,
			activityHandler.CheckKubeResource,
			input.toKubeReqInput()).Get(ctx)
		if err != nil {
			return nil, err
		}

		// if the object doesn't exist, clean up the spicedb write
		if !exists {
			rollback.Cleanup(ctx)
			return nil, err
		}
	}

	return out, nil
}

// ResourceLockRel generates a relationship representing a worfklow's lock over a
// specific resource in kube.
func ResourceLockRel(input *WriteObjInput, workflowID string) *v1.RelationshipUpdate {
	// Delete names come from the request, Create names come from the object
	// TODO: this could benefit from an objectid helper shared with jmespath
	name := input.RequestInfo.Name
	if input.ObjectMeta != nil {
		name = input.ObjectMeta.Name
	}

	lockKey := input.RequestInfo.Path + "/" + name + "/" + input.RequestInfo.Verb
	lockHash := fmt.Sprintf("%x", xxhash.Sum64String(lockKey))
	return &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_CREATE,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{
				ObjectType: lockResourceType,
				ObjectId:   lockHash,
			},
			Relation: lockRelationName,
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{
					ObjectType: workflowResourceType,
					ObjectId:   workflowID,
				},
			},
		},
	}
}

// KubeConflict wraps an error and turns it into a standard kube conflict
// response.
func KubeConflict(err error, input *WriteObjInput) *KubeResp {
	var out KubeResp
	statusError := k8serrors.NewConflict(schema.GroupResource{
		Group:    input.RequestInfo.APIGroup,
		Resource: input.RequestInfo.Resource,
	}, input.ObjectMeta.Name, err)
	out.StatusCode = http.StatusConflict
	out.Err = *statusError
	out.Body, _ = json.Marshal(statusError)
	return &out
}

func resourceLockDoesNotExist(lockRel *v1.Relationship) *v1.Precondition {
	return &v1.Precondition{
		Operation: v1.Precondition_OPERATION_MUST_NOT_MATCH,
		Filter: &v1.RelationshipFilter{
			ResourceType:          lockResourceType,
			OptionalResourceId:    lockRel.Resource.ObjectId,
			OptionalRelation:      lockRelationName,
			OptionalSubjectFilter: &v1.SubjectFilter{SubjectType: workflowResourceType},
		},
	}
}

func WorkflowForLockMode(lockMode string) (any, error) {
	f := PessimisticWriteToSpiceDBAndKube
	if lockMode == StrategyOptimisticWriteToSpiceDBAndKube {
		f = OptimisticWriteToSpiceDBAndKube
	}

	return f, nil
}
