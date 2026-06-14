package main

import (
	"context"
	"errors"
	"sort"
	"time"
)

// MockDatabase is an in-memory Database mirroring the slice of the SQL contract the reviser
// depends on: UNIQUE(version) on interest_profile, the at-most-one-active partial index, and the
// append-only created_at window over feedback. Zero I/O — the whole reviser is exercised in
// memory. InsertFeedback is a test-only seeding helper (production hone only READS feedback).
type MockDatabase struct {
	profiles map[int]InterestProfile // UNIQUE(version)
	feedback []Feedback              // append-only

	// nowFn stamps CreatedAt on inserted profiles / seeded feedback when the caller leaves it zero
	// (mirroring the SQL DEFAULT CURRENT_TIMESTAMP). Tests override it for determinism.
	nowFn func() time.Time
}

var (
	errUniqueViolation = errors.New("mock: unique violation")
	errCheckViolation  = errors.New("mock: check violation")
)

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		profiles: make(map[int]InterestProfile),
		nowFn:    time.Now,
	}
}

var _ Database = (*MockDatabase)(nil)

func (m *MockDatabase) GetActiveInterestProfile(_ context.Context) (InterestProfile, bool, error) {
	for _, p := range m.profiles {
		if p.Status == profileActive {
			return p, true, nil
		}
	}
	return InterestProfile{}, false, nil
}

func (m *MockDatabase) ListInterestProfiles(_ context.Context) ([]InterestProfile, error) {
	out := make([]InterestProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (m *MockDatabase) ListFeedbackSince(_ context.Context, since time.Time) ([]Feedback, error) {
	var out []Feedback
	for _, f := range m.feedback { // append-only slice preserves insertion (id) order
		if f.CreatedAt.After(since) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *MockDatabase) InsertInterestProfile(_ context.Context, p InterestProfile) error {
	if _, ok := m.profiles[p.Version]; ok {
		return errUniqueViolation // UNIQUE(version) — versions are immutable
	}
	p.Status = profileStatusOr(p.Status)
	if !isValidProfileStatus(p.Status) {
		return errCheckViolation
	}
	if p.Status == profileActive {
		// Mirror the partial unique index idx_interest_profile_active: at most one active row.
		for _, ex := range m.profiles {
			if ex.Status == profileActive {
				return errUniqueViolation
			}
		}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = m.nowFn()
	}
	m.profiles[p.Version] = p
	return nil
}

// InsertFeedback is a test-only helper to seed the learning signal (production hone never writes
// feedback — rara-core's feedback/quarantine roles do). It appends in insertion (id) order.
func (m *MockDatabase) InsertFeedback(_ context.Context, f Feedback) error {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = m.nowFn()
	}
	m.feedback = append(m.feedback, f)
	return nil
}
