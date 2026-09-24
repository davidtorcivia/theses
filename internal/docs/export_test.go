package docs

// Editing reports how many documents have a periodic revision timer armed,
// which is what a test asserts stops when the editing does.
func (s *Service) Editing() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
