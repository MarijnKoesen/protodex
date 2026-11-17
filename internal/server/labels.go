// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Labels are movable pointers from a name to a commit of a module. A
// module's default label (normally "main") is not created with the module:
// labels need a commit, so it appears on the first push.

// labeled is a label with the module and owner it belongs to.
type labeled struct {
	owner store.Owner
	mod   store.Module
	label store.Label
}

func labelNotFound(o store.Owner, m store.Module, name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("label %q not found in module %s/%s", name, o.Name, m.Name))
}

// getLabelByID loads a readable label by id.
func getLabelByID(ctx context.Context, q store.Querier, id string) (labeled, error) {
	l, err := q.GetLabelByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return labeled{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("label with id %s not found", id))
	}
	if err != nil {
		return labeled{}, internalErr(err)
	}
	o, m, err := getModuleByID(ctx, q, l.ModuleID)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return labeled{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("label with id %s not found", id))
	}
	if err != nil {
		return labeled{}, err
	}
	return labeled{o, m, l}, nil
}

// getLabel loads a label of a module by name.
func getLabel(ctx context.Context, q store.Querier, o store.Owner, m store.Module, name string) (labeled, error) {
	l, err := q.GetLabel(ctx, m.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return labeled{}, labelNotFound(o, m, name)
	}
	if err != nil {
		return labeled{}, internalErr(err)
	}
	return labeled{o, m, l}, nil
}

// resolveLabel resolves a LabelRef to an existing, readable label.
func resolveLabel(ctx context.Context, q store.Querier, ref *modulev1.LabelRef) (labeled, error) {
	if id := ref.GetId(); id != "" {
		return getLabelByID(ctx, q, id)
	}
	n := ref.GetName()
	if n == nil {
		return labeled{}, connect.NewError(connect.CodeInvalidArgument, errors.New("label reference is empty"))
	}
	o, m, err := getModuleByName(ctx, q, n.GetOwner(), n.GetModule())
	if err != nil {
		return labeled{}, err
	}
	return getLabel(ctx, q, o, m, n.GetLabel())
}

// labelProto converts a label. The commit-check state is always DISABLED
// (protodex has no policy checks), dated at the commit's creation.
func labelProto(ctx context.Context, q store.Querier, l labeled) (*modulev1.Label, error) {
	c, err := q.GetCommit(ctx, l.mod.ID, l.label.CommitID)
	if err != nil {
		return nil, internalErr(err)
	}
	p := &modulev1.Label{
		Id:         l.label.ID,
		CreateTime: timestamppb.New(l.label.CreatedAt),
		UpdateTime: timestamppb.New(l.label.UpdatedAt),
		Name:       l.label.Name,
		OwnerId:    l.mod.OwnerID,
		ModuleId:   l.mod.ID,
		CommitId:   l.label.CommitID,
		CommitCheckState: &modulev1.CommitCheckState{
			Status:     modulev1.CommitCheckStatus_COMMIT_CHECK_STATUS_DISABLED,
			UpdateTime: timestamppb.New(c.CreatedAt),
		},
	}
	if l.label.ArchivedAt != nil {
		p.ArchiveTime = timestamppb.New(*l.label.ArchivedAt)
	}
	if tuuidPattern.MatchString(l.label.UpdatedBy) {
		p.UpdatedByUserId = l.label.UpdatedBy
	}
	return p, nil
}

var tuuidPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// labelService implements buf.registry.module.v1.LabelService
// (ListLabelHistory stays unimplemented: label history is not recorded yet).
type labelService struct {
	modulev1connect.UnimplementedLabelServiceHandler
	st store.Store
}

// GetLabels resolves label references, archived labels included.
func (s *labelService) GetLabels(ctx context.Context, req *connect.Request[modulev1.GetLabelsRequest]) (*connect.Response[modulev1.GetLabelsResponse], error) {
	resp := &modulev1.GetLabelsResponse{}
	for _, ref := range req.Msg.GetLabelRefs() {
		l, err := resolveLabel(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		p, err := labelProto(ctx, s.st, l)
		if err != nil {
			return nil, err
		}
		resp.Labels = append(resp.Labels, p)
	}
	return connect.NewResponse(resp), nil
}

// resourceLabels resolves a ResourceRef and returns the labels it selects:
// all labels of a module, one label, or the labels pointing at a commit.
// Names resolve "ref" as a commit id first, then as a label name.
func resourceLabels(ctx context.Context, q store.Querier, ref *modulev1.ResourceRef) (store.Owner, store.Module, []store.Label, error) {
	var (
		o        store.Owner
		m        store.Module
		err      error
		label    string
		commitID string
	)
	if id := ref.GetId(); id != "" {
		switch _, merr := q.GetModuleByID(ctx, id); {
		case merr == nil:
			o, m, err = getModuleByID(ctx, q, id)
		case !errors.Is(merr, store.ErrNotFound):
			return o, m, nil, internalErr(merr)
		default:
			if l, lerr := getLabelByID(ctx, q, id); lerr == nil {
				return l.owner, l.mod, []store.Label{l.label}, nil
			} else if connect.CodeOf(lerr) != connect.CodeNotFound {
				return o, m, nil, lerr
			}
			c, cerr := q.GetCommitByID(ctx, id)
			if errors.Is(cerr, store.ErrNotFound) {
				return o, m, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("resource with id %s not found", id))
			}
			if cerr != nil {
				return o, m, nil, internalErr(cerr)
			}
			o, m, err = getModuleByID(ctx, q, c.ModuleID)
			if connect.CodeOf(err) == connect.CodeNotFound {
				return o, m, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("resource with id %s not found", id))
			}
			commitID = c.ID
		}
	} else {
		n := ref.GetName()
		if n == nil {
			return o, m, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("resource reference is empty"))
		}
		o, m, err = getModuleByName(ctx, q, n.GetOwner(), n.GetModule())
		if err != nil {
			return o, m, nil, err
		}
		label = n.GetLabelName()
		if r := n.GetRef(); r != "" {
			c, cerr := q.GetCommit(ctx, m.ID, r)
			switch {
			case cerr == nil:
				commitID = c.ID
			case errors.Is(cerr, store.ErrNotFound):
				label = r
			default:
				return o, m, nil, internalErr(cerr)
			}
		}
	}
	if err != nil {
		return o, m, nil, err
	}
	if label != "" {
		l, err := getLabel(ctx, q, o, m, label)
		if err != nil {
			return o, m, nil, err
		}
		return o, m, []store.Label{l.label}, nil
	}
	ls, err := q.ListLabels(ctx, m.ID)
	if err != nil {
		return o, m, nil, internalErr(err)
	}
	if commitID != "" {
		ls = slices.DeleteFunc(ls, func(l store.Label) bool { return l.CommitID != commitID })
	}
	return o, m, ls, nil
}

// ListLabels lists the labels selected by the resource reference. Archived
// labels are excluded unless the archive filter asks for them.
func (s *labelService) ListLabels(ctx context.Context, req *connect.Request[modulev1.ListLabelsRequest]) (*connect.Response[modulev1.ListLabelsResponse], error) {
	m := req.Msg
	o, mod, ls, err := resourceLabels(ctx, s.st, m.GetResourceRef())
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(m.GetNameQuery())
	// Every label points at a commit whose check status is DISABLED.
	noneMatch := len(m.GetCommitCheckStatuses()) > 0 &&
		!slices.Contains(m.GetCommitCheckStatuses(), modulev1.CommitCheckStatus_COMMIT_CHECK_STATUS_DISABLED)
	ls = slices.DeleteFunc(ls, func(l store.Label) bool {
		switch m.GetArchiveFilter() {
		case modulev1.ListLabelsRequest_ARCHIVE_FILTER_ARCHIVED_ONLY:
			if !l.Archived {
				return true
			}
		case modulev1.ListLabelsRequest_ARCHIVE_FILTER_ALL:
		default:
			if l.Archived {
				return true
			}
		}
		return noneMatch || !strings.Contains(strings.ToLower(l.Name), query)
	})
	sortByTime(ls, func(l store.Label) (int64, int64, string) {
		return l.CreatedAt.UnixMicro(), l.UpdatedAt.UnixMicro(), l.ID
	}, timeOrder(m.GetOrder()))
	ls, next, err := page(ls, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &modulev1.ListLabelsResponse{NextPageToken: next}
	for _, l := range ls {
		p, err := labelProto(ctx, s.st, labeled{o, mod, l})
		if err != nil {
			return nil, err
		}
		resp.Labels = append(resp.Labels, p)
	}
	return connect.NewResponse(resp), nil
}

// CreateOrUpdateLabels creates labels or moves them to a newer commit of
// their module, unarchiving archived labels. Moving a label to an older
// commit is FailedPrecondition. The batch is atomic.
func (s *labelService) CreateOrUpdateLabels(ctx context.Context, req *connect.Request[modulev1.CreateOrUpdateLabelsRequest]) (*connect.Response[modulev1.CreateOrUpdateLabelsResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	resp := &modulev1.CreateOrUpdateLabelsResponse{}
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			var (
				l      labeled
				exists bool
			)
			if id := v.GetLabelRef().GetId(); id != "" {
				found, err := getLabelByID(ctx, q, id)
				if err != nil {
					return err
				}
				l, exists = found, true
			} else {
				n := v.GetLabelRef().GetName()
				o, m, err := getModuleByName(ctx, q, n.GetOwner(), n.GetModule())
				if err != nil {
					return err
				}
				if err := module.ValidateLabelName(n.GetLabel()); err != nil {
					return invalidName(err)
				}
				l = labeled{owner: o, mod: m, label: store.Label{ModuleID: m.ID, Name: n.GetLabel()}}
				switch cur, err := q.GetLabel(ctx, m.ID, n.GetLabel()); {
				case err == nil:
					l.label, exists = cur, true
				case !errors.Is(err, store.ErrNotFound):
					return internalErr(err)
				}
			}
			if err := requireModule(ctx, q, l.owner, l.mod, permWrite); err != nil {
				return err
			}
			c, err := q.GetCommit(ctx, l.mod.ID, v.GetCommitId())
			if errors.Is(err, store.ErrNotFound) {
				return connect.NewError(connect.CodeNotFound, fmt.Errorf("commit %s not found in module %s/%s", v.GetCommitId(), l.owner.Name, l.mod.Name))
			}
			if err != nil {
				return internalErr(err)
			}
			if exists && c.ID != l.label.CommitID {
				cur, err := q.GetCommit(ctx, l.mod.ID, l.label.CommitID)
				if err != nil {
					return internalErr(err)
				}
				if c.CreatedAt.Before(cur.CreatedAt) {
					return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("label %q points at commit %s, which is newer than commit %s", l.label.Name, cur.ID, c.ID))
				}
			}
			if !exists || c.ID != l.label.CommitID || l.label.Archived {
				l.label, err = q.SetLabel(ctx, store.Label{ID: l.label.ID, ModuleID: l.mod.ID, Name: l.label.Name,
					CommitID: c.ID, UpdatedBy: me.Owner.ID})
				if err != nil {
					return internalErr(err)
				}
				if err := record(ctx, q, audit.LabelUpdated, l.owner.Name+"/"+l.mod.Name+":"+l.label.Name, map[string]any{
					"module_id": l.mod.ID, "label": l.label.Name, "commit_id": c.ID}); err != nil {
					return err
				}
			}
			p, err := labelProto(ctx, q, l)
			if err != nil {
				return err
			}
			resp.Labels = append(resp.Labels, p)
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// setArchived archives or unarchives the referenced labels atomically. The
// module's default label cannot be archived.
func (s *labelService) setArchived(ctx context.Context, refs []*modulev1.LabelRef, archived bool) error {
	if _, err := caller(ctx); err != nil {
		return err
	}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, ref := range refs {
			l, err := resolveLabel(ctx, q, ref)
			if err != nil {
				return err
			}
			if err := requireModule(ctx, q, l.owner, l.mod, permWrite); err != nil {
				return err
			}
			if archived && l.label.Name == l.mod.DefaultLabel {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("label %q is the default label of module %s/%s and cannot be archived", l.label.Name, l.owner.Name, l.mod.Name))
			}
			if l.label.Archived == archived {
				continue
			}
			if err := q.ArchiveLabel(ctx, l.mod.ID, l.label.Name, archived); err != nil {
				return internalErr(err)
			}
			typ := audit.LabelUnarchived
			if archived {
				typ = audit.LabelArchived
			}
			if err := record(ctx, q, typ, l.owner.Name+"/"+l.mod.Name+":"+l.label.Name, map[string]any{
				"module_id": l.mod.ID, "label": l.label.Name}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return internalErr(err)
	}
	return nil
}

// ArchiveLabels archives labels; archiving an archived label is a no-op.
func (s *labelService) ArchiveLabels(ctx context.Context, req *connect.Request[modulev1.ArchiveLabelsRequest]) (*connect.Response[modulev1.ArchiveLabelsResponse], error) {
	if err := s.setArchived(ctx, req.Msg.GetLabelRefs(), true); err != nil {
		return nil, err
	}
	return connect.NewResponse(&modulev1.ArchiveLabelsResponse{}), nil
}

// UnarchiveLabels unarchives labels; unarchiving an active label is a no-op.
func (s *labelService) UnarchiveLabels(ctx context.Context, req *connect.Request[modulev1.UnarchiveLabelsRequest]) (*connect.Response[modulev1.UnarchiveLabelsResponse], error) {
	if err := s.setArchived(ctx, req.Msg.GetLabelRefs(), false); err != nil {
		return nil, err
	}
	return connect.NewResponse(&modulev1.UnarchiveLabelsResponse{}), nil
}
