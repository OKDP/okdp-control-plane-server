package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// ErrInvalidMember reports a member the request describes badly.
var ErrInvalidMember = errors.New("invalid member")

// MemberService manages who holds which role on a project.
type MemberService interface {
	ListMembers(ctx context.Context, project string) (*models.ProjectMembers, error)
	SetMember(ctx context.Context, project string, member models.ProjectMember) (*models.ProjectMember, error)
	RemoveMember(ctx context.Context, project, kind, name string) error
}

type DefaultMemberService struct {
	repo repository.MemberRepository
}

func NewDefaultMemberService(repo repository.MemberRepository) *DefaultMemberService {
	return &DefaultMemberService{repo: repo}
}

func (s *DefaultMemberService) ListMembers(ctx context.Context, project string) (*models.ProjectMembers, error) {
	members, err := s.repo.List(ctx, project)
	if err != nil {
		return nil, err
	}
	return &models.ProjectMembers{Members: members, CanManage: s.repo.CanManage(ctx, project)}, nil
}

func (s *DefaultMemberService) SetMember(ctx context.Context, project string, member models.ProjectMember) (*models.ProjectMember, error) {
	member.Name = strings.TrimSpace(member.Name)
	if err := validateMember(member.Kind, member.Name); err != nil {
		return nil, err
	}
	if !slices.Contains(models.ProjectRoles, member.Role) {
		return nil, fmt.Errorf("%w: role must be one of %s", ErrInvalidMember, strings.Join(models.ProjectRoles, ", "))
	}
	if err := s.repo.Set(ctx, project, member); err != nil {
		return nil, err
	}
	return &member, nil
}

func (s *DefaultMemberService) RemoveMember(ctx context.Context, project, kind, name string) error {
	name = strings.TrimSpace(name)
	if err := validateMember(kind, name); err != nil {
		return err
	}
	return s.repo.Remove(ctx, project, kind, name)
}

func validateMember(kind, name string) error {
	if kind != models.MemberKindUser && kind != models.MemberKindGroup {
		return fmt.Errorf("%w: kind must be %s or %s", ErrInvalidMember, models.MemberKindUser, models.MemberKindGroup)
	}
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidMember)
	}
	return nil
}
