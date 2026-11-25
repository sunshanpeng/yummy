package runtimeconfig

import (
	"bytes"
	"context"
	"slices"
	"sort"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
)

type Change struct {
	Field     string
	Name      string
	Operation string
}

func (s *Service) Compare(ctx context.Context, p authorization.Principal, ws, app string, baseRevision, targetRevision int64) ([]Change, error) {
	if baseRevision < 1 || targetRevision < 1 {
		return nil, authorization.ErrInvalid
	}
	var changes []Change
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ConfigRead); err != nil {
			return err
		}
		base, a, err := readRevision(tx, ws, app, baseRevision)
		if err != nil {
			return err
		}
		target, b, err := readRevision(tx, ws, app, targetRevision)
		if err != nil {
			return err
		}
		for _, field := range []struct {
			name    string
			changed bool
		}{
			{"type", a.Type != b.Type}, {"port", a.Port != b.Port}, {"probe", a.Probe != b.Probe},
			{"command", !slices.Equal(a.Command, b.Command)}, {"args", !slices.Equal(a.Args, b.Args)},
			{"domains", !slices.Equal(a.Domains, b.Domains)}, {"resources", a.Resources != b.Resources},
			{"volumes", !slices.Equal(a.Volumes, b.Volumes)}, {"confirmed", base.Confirmed != target.Confirmed},
		} {
			if field.changed {
				changes = append(changes, Change{Field: field.name, Operation: "UPDATED"})
			}
		}
		for _, fields := range []struct {
			kind string
			a, b []secretValue
		}{{"variable", a.Variables, b.Variables}, {"file", a.Files, b.Files}} {
			left := make(map[string]secretValue, len(fields.a))
			for _, value := range fields.a {
				left[value.Name] = value
			}
			for _, value := range fields.b {
				old, exists := left[value.Name]
				if !exists {
					changes = append(changes, Change{fields.kind, value.Name, "ADDED"})
					continue
				}
				delete(left, value.Name)
				same := old.Sensitive == value.Sensitive && old.Value == value.Value
				if same && value.Sensitive {
					same, err = s.compareSecret(ws, app, fields.kind, baseRevision, targetRevision, old, value)
					if err != nil {
						return err
					}
				}
				if !same {
					changes = append(changes, Change{fields.kind, value.Name, "UPDATED"})
				}
			}
			for _, value := range left {
				changes = append(changes, Change{fields.kind, value.Name, "REMOVED"})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Field != changes[j].Field {
			return changes[i].Field < changes[j].Field
		}
		return changes[i].Name < changes[j].Name
	})
	return changes, nil
}

func (s *Service) compareSecret(ws, app, kind string, base, target int64, a, b secretValue) (bool, error) {
	left, err := s.box.Decrypt(a.Ciphertext, secretIdentity(ws, app, kind, a.Name, base))
	if err != nil {
		return false, err
	}
	defer clear(left)
	right, err := s.box.Decrypt(b.Ciphertext, secretIdentity(ws, app, kind, b.Name, target))
	if err != nil {
		return false, err
	}
	defer clear(right)
	return bytes.Equal(left, right), nil
}
