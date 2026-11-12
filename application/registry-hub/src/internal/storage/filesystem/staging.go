package filesystem

import "os"

func (s *Store) createStaging(directory string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.CreateTemp(directory, ".staging-")
	if err != nil {
		return nil, err
	}
	if s.staging == nil {
		s.staging = make(map[string]struct{})
	}
	s.staging[file.Name()] = struct{}{}
	return file, nil
}

func (s *Store) removeStaging(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(path)
	delete(s.staging, path)
}

func (s *Store) activeStaging(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, active := s.staging[path]
	return active
}

func (s *Store) removeOrphan(path string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.staging[path]; active {
		return false, nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
