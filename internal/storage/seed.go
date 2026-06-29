package storage

import (
	"context"
	"fmt"

	"github.com/pod32g/omni-notify/internal/models"
)

// SeedProviders upserts config-managed providers. Each is marked managed_by so
// that API-created providers (managed_by=api) are never touched here.
func (s *Store) SeedProviders(ctx context.Context, provs []models.ProviderConfig) error {
	for _, p := range provs {
		p.ManagedBy = models.ManagedByConfig
		if err := s.UpsertProvider(ctx, p); err != nil {
			return fmt.Errorf("seed provider %q: %w", p.Name, err)
		}
	}
	return nil
}

// SeedRoutes upserts config-managed routes.
func (s *Store) SeedRoutes(ctx context.Context, routes []models.Route) error {
	for _, r := range routes {
		r.ManagedBy = models.ManagedByConfig
		if err := s.UpsertRoute(ctx, r); err != nil {
			return fmt.Errorf("seed route %q: %w", r.Name, err)
		}
	}
	return nil
}
