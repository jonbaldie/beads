package uow

import (
	"context"

	publicops "github.com/jonbaldie/beads/issueops"
)

func (u *notifyingUOW) recordCreate(ctx context.Context, createdID string, request publicops.CreateRequest) {
	u.rec.Record(opCreate, u.snapshotter().AnyPlane(ctx, createdID))
	for _, source := range createEdgeSourcesFromRequest(createdID, request) {
		u.rec.Record(opDepAdd, u.snapshotter().AnyPlaneWithEdges(ctx, source))
	}
}

func (u *notifyingUOW) recordUpdate(ctx context.Context, request publicops.UpdateRequest) {
	if !updateRequestWrites(request) {
		return
	}
	u.rec.Record(opUpdate, u.snapshotter().AnyPlane(ctx, request.IssueID))
}

func (u *notifyingUOW) recordClose(ctx context.Context, id string) {
	u.rec.Record(opClose, u.snapshotter().AnyPlane(ctx, id))
}

func (u *notifyingUOW) recordUpdateLanded(ctx context.Context, id string) {
	u.rec.Record(opUpdate, u.snapshotter().AnyPlane(ctx, id))
}

func (u *notifyingUOW) recordDepAdd(ctx context.Context, id string) {
	u.rec.Record(opDepAdd, u.snapshotter().AnyPlaneWithEdges(ctx, id))
}

func (u *notifyingUOW) recordReopen(ctx context.Context, id string, changed bool) {
	if !changed {
		return
	}
	u.rec.Record(opUpdate, u.snapshotter().AnyPlane(ctx, id))
}
