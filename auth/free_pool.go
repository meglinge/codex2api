package auth

import (
	"context"
	"database/sql"
)

func (a *Account) UsesTickets() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UseTickets
}

func (s *Store) UpdateAccountUseTickets(ctx context.Context, id int64, enabled bool) error {
	account := s.FindByID(id)
	if account == nil {
		return sql.ErrNoRows
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := s.db.SetAccountUseTickets(ctx, id, enabled); err != nil {
		return err
	}
	account.UseTickets = enabled
	return nil
}

func (a *Account) SetUseTickets(enabled bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.UseTickets = enabled
	a.mu.Unlock()
}
