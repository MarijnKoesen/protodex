// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1"
	"github.com/marijnkoesen/protodex/gen/buf/registry/iam/v1/iamv1connect"
	ownerv1 "github.com/marijnkoesen/protodex/gen/buf/registry/owner/v1"
	"github.com/marijnkoesen/protodex/internal/audit"
	"github.com/marijnkoesen/protodex/internal/auth"
	"github.com/marijnkoesen/protodex/internal/store"
)

// tokenService implements buf.registry.iam.v1.TokenService (UpdateTokens stays
// unimplemented). Callers may only manage their own tokens until the
// permissions model lands.
type tokenService struct {
	iamv1connect.UnimplementedTokenServiceHandler
	st store.Store
}

func caller(ctx context.Context) (auth.Identity, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
	}
	return id, nil
}

// record appends an audit event inside q's transaction.
func record(ctx context.Context, q store.Querier, typ, subject string, data map[string]any) error {
	if _, err := audit.Record(ctx, q, audit.Event{Type: typ, Subject: subject, Data: data}); err != nil {
		return internalErr(err)
	}
	return nil
}

func internalErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func tokenProto(t store.Token, ownerID string) *iamv1.Token {
	p := &iamv1.Token{
		Id:         t.ID,
		CreateTime: timestamppb.New(t.CreatedAt),
		UpdateTime: timestamppb.New(t.CreatedAt),
		Name:       t.Name,
		UserId:     ownerID,
	}
	if t.ExpiresAt != nil {
		p.ExpireTime = timestamppb.New(*t.ExpiresAt)
	}
	if t.LastUsedAt != nil {
		p.LastUsedTime = timestamppb.New(*t.LastUsedAt)
	}
	return p
}

// resolveUser maps a UserRef (nil means the caller) to the caller's own
// account, refusing other users.
func (s *tokenService) resolveUser(me auth.Identity, ref *ownerv1.UserRef) error {
	if ref == nil {
		return nil
	}
	switch v := ref.GetValue().(type) {
	case *ownerv1.UserRef_Id:
		if v.Id == me.Owner.ID {
			return nil
		}
	case *ownerv1.UserRef_Name:
		if v.Name == me.Owner.Name {
			return nil
		}
	default:
		return nil
	}
	// Don't reveal whether the other user exists.
	return connect.NewError(connect.CodePermissionDenied, errors.New("tokens of other users cannot be managed"))
}

// findToken locates one of the caller's tokens by ref.
func (s *tokenService) findToken(ctx context.Context, me auth.Identity, ref *iamv1.TokenRef) (store.Token, error) {
	toks, err := s.st.ListTokens(ctx, me.Owner.ID)
	if err != nil {
		return store.Token{}, internalErr(err)
	}
	notFound := connect.NewError(connect.CodeNotFound, errors.New("token not found"))
	switch v := ref.GetValue().(type) {
	case *iamv1.TokenRef_Id:
		for _, t := range toks {
			if t.ID == v.Id {
				return t, nil
			}
		}
	case *iamv1.TokenRef_Name_:
		if err := s.resolveUser(me, v.Name.GetUserRef()); err != nil {
			return store.Token{}, err
		}
		var match []store.Token
		for _, t := range toks {
			if t.Name == v.Name.GetName() {
				match = append(match, t)
			}
		}
		if len(match) > 1 {
			return store.Token{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("token name %q is ambiguous; use the token id", v.Name.GetName()))
		}
		if len(match) == 1 {
			return match[0], nil
		}
	default:
		return store.Token{}, connect.NewError(connect.CodeInvalidArgument, errors.New("token ref is empty"))
	}
	return store.Token{}, notFound
}

// GetTokens returns the referenced tokens of the caller.
func (s *tokenService) GetTokens(ctx context.Context, req *connect.Request[iamv1.GetTokensRequest]) (*connect.Response[iamv1.GetTokensResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	resp := &iamv1.GetTokensResponse{}
	for _, ref := range req.Msg.GetTokenRefs() {
		t, err := s.findToken(ctx, me, ref)
		if err != nil {
			return nil, err
		}
		resp.Tokens = append(resp.Tokens, tokenProto(t, me.Owner.ID))
	}
	return connect.NewResponse(resp), nil
}

// ListTokens lists the caller's tokens. Pagination is not implemented: all
// tokens are returned in one page.
func (s *tokenService) ListTokens(ctx context.Context, req *connect.Request[iamv1.ListTokensRequest]) (*connect.Response[iamv1.ListTokensResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	for _, ref := range req.Msg.GetUserRefs() {
		if err := s.resolveUser(me, ref); err != nil {
			return nil, err
		}
	}
	toks, err := s.st.ListTokens(ctx, me.Owner.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	now := time.Now()
	toks = slices.DeleteFunc(toks, func(t store.Token) bool {
		expired := t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
		switch req.Msg.GetExpireFilter() {
		case iamv1.ListTokensRequest_EXPIRE_FILTER_EXPIRED_ONLY:
			return !expired
		case iamv1.ListTokensRequest_EXPIRE_FILTER_ALL:
			return false
		default: // unspecified and UNEXPIRED_ONLY
			return expired
		}
	})
	sortTokens(toks, req.Msg.GetOrder())
	resp := &iamv1.ListTokensResponse{}
	for _, t := range toks {
		resp.Tokens = append(resp.Tokens, tokenProto(t, me.Owner.ID))
	}
	return connect.NewResponse(resp), nil
}

// CreateTokens creates tokens for the caller and returns their secrets, which
// are never retrievable again.
func (s *tokenService) CreateTokens(ctx context.Context, req *connect.Request[iamv1.CreateTokensRequest]) (*connect.Response[iamv1.CreateTokensResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, v := range req.Msg.GetValues() {
		if err := s.resolveUser(me, v.GetUserRef()); err != nil {
			return nil, err
		}
		if strings.TrimSpace(v.GetName()) == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token name is required"))
		}
		if len(v.GetScopedPermissions()) > 0 {
			// Silently dropping a restriction would widen the token.
			return nil, connect.NewError(connect.CodeUnimplemented, errors.New("scoped permissions are not supported yet"))
		}
		if v.GetExpireTime() != nil && !v.GetExpireTime().AsTime().After(now) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("expire_time must be in the future"))
		}
	}

	resp := &iamv1.CreateTokensResponse{}
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		resp.Values = nil
		for _, v := range req.Msg.GetValues() {
			secret, err := auth.GenerateSecret()
			if err != nil {
				return err
			}
			t := store.Token{OwnerID: me.Owner.ID, Name: v.GetName(), Hash: auth.HashSecret(secret)}
			if v.GetExpireTime() != nil {
				e := v.GetExpireTime().AsTime()
				t.ExpiresAt = &e
			}
			t, err = q.CreateToken(ctx, t)
			if err != nil {
				return err
			}
			if err := record(ctx, q, audit.TokenCreated, me.Owner.Name, map[string]any{
				"token_id": t.ID, "token_name": t.Name, "expires": t.ExpiresAt}); err != nil {
				return err
			}
			resp.Values = append(resp.Values, &iamv1.CreateTokensResponse_Value{Token: tokenProto(t, me.Owner.ID), Secret: secret})
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(resp), nil
}

// DeleteTokens deletes the referenced tokens of the caller.
func (s *tokenService) DeleteTokens(ctx context.Context, req *connect.Request[iamv1.DeleteTokensRequest]) (*connect.Response[iamv1.DeleteTokensResponse], error) {
	me, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	names := map[string]string{}
	for _, ref := range req.Msg.GetTokenRefs() {
		t, err := s.findToken(ctx, me, ref)
		if err != nil {
			return nil, err
		}
		ids = append(ids, t.ID)
		names[t.ID] = t.Name
	}
	// All-or-nothing: a failure must not leave a partial revocation behind.
	err = s.st.WithTx(ctx, func(q store.Querier) error {
		for _, id := range ids {
			if err := q.DeleteToken(ctx, id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return err
			}
			if err := record(ctx, q, audit.TokenDeleted, me.Owner.Name, map[string]any{
				"token_id": id, "token_name": names[id]}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&iamv1.DeleteTokensResponse{}), nil
}

// sortTokens orders toks per the request; unset times sort as the zero time
// and ties fall back to newest-created first.
func sortTokens(toks []store.Token, order iamv1.ListTokensRequest_Order) {
	at := func(p *time.Time) time.Time {
		if p == nil {
			return time.Time{}
		}
		return *p
	}
	key := func(t store.Token) time.Time {
		switch order {
		case iamv1.ListTokensRequest_ORDER_EXPIRE_TIME_DESC, iamv1.ListTokensRequest_ORDER_EXPIRE_TIME_ASC:
			return at(t.ExpiresAt)
		case iamv1.ListTokensRequest_ORDER_LAST_USED_TIME_DESC, iamv1.ListTokensRequest_ORDER_LAST_USED_TIME_ASC:
			return at(t.LastUsedAt)
		}
		return t.CreatedAt
	}
	asc := order == iamv1.ListTokensRequest_ORDER_CREATE_TIME_ASC ||
		order == iamv1.ListTokensRequest_ORDER_EXPIRE_TIME_ASC ||
		order == iamv1.ListTokensRequest_ORDER_LAST_USED_TIME_ASC
	slices.SortStableFunc(toks, func(a, b store.Token) int {
		c := key(a).Compare(key(b))
		if !asc {
			c = -c
		}
		if c == 0 {
			c = b.CreatedAt.Compare(a.CreatedAt) // ties: newest first
		}
		return c
	})
}
