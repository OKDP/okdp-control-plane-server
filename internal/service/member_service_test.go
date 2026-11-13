package service

import (
	"context"
	"errors"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// memberRepoSpy records the member it is asked to set.
type memberRepoSpy struct {
	repository.MemberRepository
	set *models.ProjectMember
}

func (s *memberRepoSpy) Set(_ context.Context, _ string, m models.ProjectMember) error {
	s.set = &m
	return nil
}

func TestSetMemberRefusesABadMember(t *testing.T) {
	cases := map[string]models.ProjectMember{
		"unknown kind": {Kind: "serviceaccount", Name: "default", Role: "view"},
		"empty name":   {Kind: "group", Name: "  ", Role: "view"},
		"unknown role": {Kind: "group", Name: "data_scientist", Role: "cluster-admin"},
	}
	for name, member := range cases {
		t.Run(name, func(t *testing.T) {
			spy := &memberRepoSpy{}
			_, err := NewDefaultMemberService(spy).SetMember(context.Background(), "demo", member)
			if !errors.Is(err, ErrInvalidMember) {
				t.Fatalf("err = %v, want ErrInvalidMember", err)
			}
			if spy.set != nil {
				t.Fatalf("the repository should not be called")
			}
		})
	}
}

func TestSetMemberTrimsTheName(t *testing.T) {
	spy := &memberRepoSpy{}
	saved, err := NewDefaultMemberService(spy).SetMember(context.Background(), "demo",
		models.ProjectMember{Kind: "group", Name: " data_scientist ", Role: "edit"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.set.Name != "data_scientist" || saved.Name != "data_scientist" {
		t.Fatalf("set %q, returned %q, want data_scientist", spy.set.Name, saved.Name)
	}
}
