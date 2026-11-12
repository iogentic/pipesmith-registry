package auth

import (
	"fmt"

	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// HashRuns is the number of digests VerifyPassword computed so far.
func HashRuns() int64 { return hashRuns.Load() }

// SessionCount is the number of sessions in the store.
func SessionCount(s *Service) int { return s.sessions.len() }

// PairCount is the number of pairs the limiter holds.
func PairCount(s *Service) int { return s.limiter.len() }

// SetMaxPairs replaces the bound of the limiter table.
func SetMaxPairs(s *Service, n int) {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	s.limiter.max = n
}

// SessionLimits gives the bounds the store uses.
func SessionLimits(s *Service) (perUser, total int) { return s.sessions.perUser, s.sessions.total }

// SetSessionLimits replaces the bounds of the store.
func SetSessionLimits(s *Service, perUser, total int) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.sessions.perUser, s.sessions.total = perUser, total
}

// FillSessions starts n sessions of n invented users without a sign in.
func FillSessions(s *Service, n int) error {
	for i := range n {
		if _, err := s.sessions.create(model.Subject{User: model.User{ID: fmt.Sprintf("fill-%d", i), Source: "default"}}); err != nil {
			return err
		}
	}
	return nil
}

// FillPairs counts one failure for n invented pairs.
func FillPairs(s *Service, n int) {
	for i := range n {
		user := fmt.Sprintf("fill-%d", i)
		if s.limiter.begin(user, "192.0.2.1") == 0 {
			s.limiter.end(user, "192.0.2.1", false)
		}
	}
}
