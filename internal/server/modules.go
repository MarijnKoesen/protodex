// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modulev1 "github.com/marijnkoesen/protodex/gen/buf/registry/module/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/module/v1/modulev1connect"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/module"
	"github.com/marijnkoesen/protodex/internal/store"
)

// Access rules (until the permissions model lands):
//
//   - Public modules are readable by anyone, including anonymous callers.
//   - A user's own namespace is fully accessible to that user only.
//   - Organization namespaces are accessible to every authenticated user
//     (single-organization model; membership is not modeled yet).
//   - Modules of mirror owners are readable like organization modules but
//     cannot be created, changed or deleted over the API.
//
// A private module the caller cannot access is reported as NotFound, so its
// existence does not leak.

// hasAccess reports whether the caller may read private modules of o.
func hasAccess(ctx context.Context, o store.Owner) bool {
	me, ok := auth.FromContext(ctx)
	if !ok {
		return false
	}
	return o.Type != store.OwnerUser || o.ID == me.Owner.ID
}

// canRead reports whether the caller may see module m owned by o.
func canRead(ctx context.Context, o store.Owner, m store.Module) bool {
	return m.Visibility == store.VisibilityPublic || hasAccess(ctx, o)
}

// checkWrite fails unless the caller may create, change or delete modules
// (and their labels) under o.
func checkWrite(ctx context.Context, o store.Owner) error {
	if _, err := caller(ctx); err != nil {
		return err
	}
	switch {
	case o.Mirror:
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("modules of mirror owner %q are managed by mirroring", o.Name))
	case !hasAccess(ctx, o):
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("no write access to owner %q", o.Name))
	}
	return nil
}

func moduleNotFound(owner, name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("module %s/%s not found", owner, name))
}

// getModuleByID loads a module and its owner by module id, hiding modules
// the caller cannot read.
func getModuleByID(ctx context.Context, q store.Querier, id string) (store.Owner, store.Module, error) {
	m, err := q.GetModuleByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Owner{}, store.Module{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("module with id %s not found", id))
	}
	if err != nil {
		return store.Owner{}, store.Module{}, internalErr(err)
	}
	o, err := q.GetOwnerByID(ctx, m.OwnerID)
	if err != nil {
		return store.Owner{}, store.Module{}, internalErr(err)
	}
	if !canRead(ctx, o, m) {
		return store.Owner{}, store.Module{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("module with id %s not found", id))
	}
	return o, m, nil
}

// getModuleByName loads a module by owner and module name, hiding modules
// the caller cannot read. An unknown owner is reported as an unknown module.
func getModuleByName(ctx context.Context, q store.Querier, ownerName, name string) (store.Owner, store.Module, error) {
	o, err := q.GetOwner(ctx, ownerName)
	if errors.Is(err, store.ErrNotFound) {
		return store.Owner{}, store.Module{}, moduleNotFound(ownerName, name)
	}
	if err != nil {
		return store.Owner{}, store.Module{}, internalErr(err)
	}
	m, err := q.GetModule(ctx, o.ID, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !canRead(ctx, o, m)) {
		return store.Owner{}, store.Module{}, moduleNotFound(ownerName, name)
	}
	if err != nil {
		return store.Owner{}, store.Module{}, internalErr(err)
	}
	return o, m, nil
}

// resolveModule resolves a ModuleRef to a readable module and its owner.
func resolveModule(ctx context.Context, q store.Querier, ref *modulev1.ModuleRef) (store.Owner, store.Module, error) {
	switch {
	case ref.GetId() != "":
		return getModuleByID(ctx, q, ref.GetId())
	case ref.GetName() != nil:
		return getModuleByName(ctx, q, ref.GetName().GetOwner(), ref.GetName().GetModule())
	}
	return store.Owner{}, store.Module{}, connect.NewError(connect.CodeInvalidArgument, errors.New("module reference is empty"))
}

var (
	visibilityToProto = map[string]modulev1.ModuleVisibility{
		store.VisibilityPublic:  modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC,
		store.VisibilityPrivate: modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE,
	}
	visibilityFromProto = map[modulev1.ModuleVisibility]string{
		modulev1.ModuleVisibility_MODULE_VISIBILITY_PUBLIC:  store.VisibilityPublic,
		modulev1.ModuleVisibility_MODULE_VISIBILITY_PRIVATE: store.VisibilityPrivate,
	}
)

func moduleProto(m store.Module) *modulev1.Module {
	state := modulev1.ModuleState_MODULE_STATE_ACTIVE
	if m.Deprecated {
		state = modulev1.ModuleState_MODULE_STATE_DEPRECATED
	}
	return &modulev1.Module{
		Id:               m.ID,
		CreateTime:       timestamppb.New(m.CreatedAt),
		UpdateTime:       timestamppb.New(m.UpdatedAt),
		Name:             m.Name,
		OwnerId:          m.OwnerID,
		Visibility:       visibilityToProto[m.Visibility],
		State:            state,
		Description:      m.Description,
		Url:              m.URL,
		DefaultLabelName: m.DefaultLabel,
	}
}

func invalidName(err error) error {
	return connect.NewError(connect.CodeInvalidArgument, err)
}

// moduleService implements buf.registry.module.v1.ModuleService.
type moduleService struct {
	modulev1connect.UnimplementedModuleServiceHandler
	st store.Store
}

// GetModules resolves module references; any unknown or hidden module fails
// the call with NotFound.
func (s *moduleService) GetModules(ctx context.Context, req *connect.Request[modulev1.GetModulesRequest]) (*connect.Response[modulev1.GetModulesResponse], error) {
	resp := &modulev1.GetModulesResponse{}
	for _, ref := range req.Msg.GetModuleRefs() {
		_, m, err := resolveModule(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		resp.Modules = append(resp.Modules, moduleProto(m))
	}
	return connect.NewResponse(resp), nil
}

// ListModules lists the readable modules of the given owners, or of all
// owners when none are given.
func (s *moduleService) ListModules(ctx context.Context, req *connect.Request[modulev1.ListModulesRequest]) (*connect.Response[modulev1.ListModulesResponse], error) {
	m := req.Msg
	var mods []store.Module
	owners := map[string]store.Owner{}
	if len(m.GetOwnerRefs()) == 0 {
		all, err := s.st.ListAllModules(ctx)
		if err != nil {
			return nil, internalErr(err)
		}
		mods = all
	}
	for _, ref := range m.GetOwnerRefs() {
		o, err := resolveOwner(ctx, s.st, ref)
		if err != nil {
			return nil, err
		}
		if _, seen := owners[o.ID]; seen {
			continue
		}
		owners[o.ID] = o
		ms, err := s.st.ListModules(ctx, o.ID)
		if err != nil {
			return nil, internalErr(err)
		}
		mods = append(mods, ms...)
	}
	visible := mods[:0]
	for _, mod := range mods {
		o, ok := owners[mod.OwnerID]
		if !ok {
			var err error
			if o, err = s.st.GetOwnerByID(ctx, mod.OwnerID); err != nil {
				return nil, internalErr(err)
			}
			owners[o.ID] = o
		}
		if canRead(ctx, o, mod) {
			visible = append(visible, mod)
		}
	}
	sortByTime(visible, func(m store.Module) (createdAt, updatedAt int64, id string) {
		return m.CreatedAt.UnixMicro(), m.UpdatedAt.UnixMicro(), m.ID
	}, timeOrder(m.GetOrder()))
	visible, next, err := page(visible, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &modulev1.ListModulesResponse{NextPageToken: next}
	for _, mod := range visible {
		resp.Modules = append(resp.Modules, moduleProto(mod))
	}
	return connect.NewResponse(resp), nil
}

// timeOrdering is the create/update-time order shared by ListModules and
// ListLabels (their Order enums have the same numbering).
type timeOrdering int32

const (
	orderByCreateDesc timeOrdering = iota + 1
	orderByCreateAsc
	orderByUpdateDesc
	orderByUpdateAsc
)

func timeOrder[E ~int32](o E) timeOrdering {
	if o == 0 {
		return orderByCreateDesc
	}
	return timeOrdering(o)
}

func sortByTime[T any](items []T, key func(T) (createdAt, updatedAt int64, id string), order timeOrdering) {
	slices.SortStableFunc(items, func(a, b T) int {
		ac, au, aid := key(a)
		bc, bu, bid := key(b)
		var c int
		switch order {
		case orderByCreateAsc:
			c = cmp.Compare(ac, bc)
		case orderByUpdateDesc:
			c = cmp.Compare(bu, au)
		case orderByUpdateAsc:
			c = cmp.Compare(au, bu)
		default:
			c = cmp.Compare(bc, ac)
		}
		return cmp.Or(c, strings.Compare(aid, bid))
	})
}

// CreateModules creates modules atomically: if one fails, none is created.
// Visibility defaults to private and the default label to "main".
func (s *moduleService) CreateModules(ctx context.Context, req *connect.Request[modulev1.CreateModulesRequest]) (*connect.Response[modulev1.CreateModulesResponse], error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	resp := &modulev1.CreateModulesResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			o, err := resolveOwner(ctx, q, v.GetOwnerRef())
			if err != nil {
				return err
			}
			if err := checkWrite(ctx, o); err != nil {
				return err
			}
			if err := module.ValidateName(v.GetName()); err != nil {
				return invalidName(err)
			}
			label := cmp.Or(v.GetDefaultLabelName(), module.DefaultLabel)
			if err := module.ValidateLabelName(label); err != nil {
				return invalidName(err)
			}
			visibility := store.VisibilityPrivate
			if v.GetVisibility() != modulev1.ModuleVisibility_MODULE_VISIBILITY_UNSPECIFIED {
				visibility = visibilityFromProto[v.GetVisibility()]
			}
			m, err := q.CreateModule(ctx, store.Module{OwnerID: o.ID, Name: v.GetName(), Visibility: visibility,
				DefaultLabel: label, Description: v.GetDescription(), URL: v.GetUrl()})
			if errors.Is(err, store.ErrConflict) {
				return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("module %s/%s already exists", o.Name, v.GetName()))
			}
			if err != nil {
				return internalErr(err)
			}
			resp.Modules = append(resp.Modules, moduleProto(m))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// UpdateModules changes module settings atomically. The default label may
// name a label that does not exist yet, but not an archived one.
func (s *moduleService) UpdateModules(ctx context.Context, req *connect.Request[modulev1.UpdateModulesRequest]) (*connect.Response[modulev1.UpdateModulesResponse], error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	resp := &modulev1.UpdateModulesResponse{}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, v := range req.Msg.GetValues() {
			o, m, err := resolveModule(ctx, q, v.GetModuleRef())
			if err != nil {
				return err
			}
			if err := checkWrite(ctx, o); err != nil {
				return err
			}
			if v.Visibility != nil {
				m.Visibility = visibilityFromProto[v.GetVisibility()]
			}
			if v.State != nil {
				m.Deprecated = v.GetState() == modulev1.ModuleState_MODULE_STATE_DEPRECATED
			}
			if v.Description != nil {
				m.Description = v.GetDescription()
			}
			if v.Url != nil {
				m.URL = v.GetUrl()
			}
			if v.DefaultLabelName != nil {
				if err := module.ValidateLabelName(v.GetDefaultLabelName()); err != nil {
					return invalidName(err)
				}
				l, err := q.GetLabel(ctx, m.ID, v.GetDefaultLabelName())
				switch {
				case err == nil && l.Archived:
					return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("label %q is archived and cannot be the default label", l.Name))
				case err != nil && !errors.Is(err, store.ErrNotFound):
					return internalErr(err)
				}
				m.DefaultLabel = v.GetDefaultLabelName()
			}
			m.UpdatedAt = time.Time{} // the store stamps the update time
			m, err = q.UpdateModule(ctx, m)
			if err != nil {
				return internalErr(err)
			}
			resp.Modules = append(resp.Modules, moduleProto(m))
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// DeleteModules deletes modules with their commits and labels, atomically.
func (s *moduleService) DeleteModules(ctx context.Context, req *connect.Request[modulev1.DeleteModulesRequest]) (*connect.Response[modulev1.DeleteModulesResponse], error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	err := s.st.WithTx(ctx, func(q store.Querier) error {
		for _, ref := range req.Msg.GetModuleRefs() {
			o, m, err := resolveModule(ctx, q, ref)
			if err != nil {
				return err
			}
			if err := checkWrite(ctx, o); err != nil {
				return err
			}
			if err := q.DeleteModule(ctx, m.ID); err != nil {
				return internalErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&modulev1.DeleteModulesResponse{}), nil
}
